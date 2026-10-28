package compaction

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/routing"
)

// Official-branch request sanitization. OpenAI verifies compaction and
// reasoning encrypted_content and rejects unknown payloads with
// invalid_encrypted_content, which fails closed browser and computer-use auto
// review after a Wopal compaction, or fails a turn after switching a
// Wopal-compacted thread to an official model. The gateway therefore never
// lets its own opaque items cross the official branch.

// unavailableContent replaces a Wopal compaction capsule that
// cannot be opened. It keeps official auto review running instead of failing
// closed, without inventing conversation history.
const unavailableContent = "The earlier part of this conversation was compacted by a Wopal AI model. The preserved compaction summary could not be restored for this request; continue from the remaining conversation context."

// ReasoningIDPrefix identifies reasoning items minted by the Wopal
// chat-completions translation; OpenAI reasoning items never use this prefix.
const (
	ReasoningIDPrefix          = "rs_chatcmpl"
	compactionResponseIDPrefix = "resp_wopal_compact_"
)

func isWopalReasoningID(id string) bool {
	return strings.HasPrefix(id, ReasoningIDPrefix) || strings.HasPrefix(id, "rs_cmb-")
}

func hasLocalPreviousResponseID(payload map[string]any) bool {
	return strings.HasPrefix(strings.TrimSpace(routing.StringOrEmpty(payload["previous_response_id"])), compactionResponseIDPrefix)
}

func itemTypeOf(item map[string]any) string {
	return strings.TrimSpace(fmt.Sprint(item["type"]))
}

func reasoningItemID(item map[string]any) string {
	id, _ := item["id"].(string)
	return strings.TrimSpace(id)
}

func EncryptedContent(item map[string]any) string {
	encoded, _ := item["encrypted_content"].(string)
	return strings.TrimSpace(encoded)
}

// A reasoning item without encrypted content cannot be resolved by the
// official API when the request uses store=false. That is true for Wopal-minted
// reasoning items regardless of how their id was generated; the id prefix is
// not a reliable provenance marker.
func reasoningNeedsNeutralization(item map[string]any) bool {
	if itemTypeOf(item) != "reasoning" {
		return false
	}
	if isWopalReasoningID(reasoningItemID(item)) {
		return true
	}
	return EncryptedContent(item) == ""
}

// sanitizeRequestInput rewrites request input items for the official
// branch and reports how many gateway-issued items were neutralized.
func inputHasCapsule(input []any) bool {
	for _, value := range input {
		item, ok := value.(map[string]any)
		if ok && strings.HasPrefix(EncryptedContent(item), CapsulePrefix) {
			return true
		}
	}
	return false
}

func sanitizeRequestInput(input []any) ([]any, int) {
	requestHasTools := false
	for _, value := range input {
		if isRetainedItem(value) {
			requestHasTools = true
			break
		}
	}
	expanded := make([]any, 0, len(input)+1)
	neutralized := 0
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			expanded = append(expanded, value)
			continue
		}
		// A Wopal compaction capsule is recognized by its ciphertext prefix,
		// not its item type, so every carrier (compaction, compaction_summary,
		// context_compaction) is neutralized through one rule.
		if encoded := EncryptedContent(item); strings.HasPrefix(encoded, CapsulePrefix) {
			neutralized++
			if base, err := expandCapsuleForOfficial(item, requestHasTools); err == nil {
				expanded = append(expanded, base...)
			} else {
				expanded = append(expanded, unavailableMessage(item["id"]))
			}
			continue
		}
		if reasoningNeedsNeutralization(item) {
			neutralized++
			continue
		}
		expanded = append(expanded, value)
	}
	return expanded, neutralized
}

func expandCapsuleForOfficial(item map[string]any, requestHasTools bool) ([]any, error) {
	capsule, err := OpenCapsule(EncryptedContent(item))
	if err != nil {
		return nil, err
	}
	base, err := expandedBase(capsule)
	if err != nil {
		return nil, err
	}
	if requestHasTools && len(capsule.Prefix) > 0 {
		base = base[len(capsule.Prefix):]
	}
	return base, nil
}

func unavailableMessage(id any) map[string]any {
	return map[string]any{
		"type":    "message",
		"id":      SummaryID(id),
		"role":    "developer",
		"content": unavailableContent,
	}
}

// Collaboration tool names whose message argument the Codex client may mark as
// encrypted. Left as encrypted: true, the official model is free to emit
// ciphertext for the delegated task, and a sub-agent that runs on a
// third-party model can only show that ciphertext.
var officialCollaborationMessageTools = map[string]bool{
	"spawn_agent":   true,
	"send_message":  true,
	"followup_task": true,
}

// officialCollaborationNamespace is the only tool namespace whose message
// argument uses Codex's encrypted-content contract.
const officialCollaborationNamespace = "collaboration"

// clearCollaborationMessageEncryption removes parameters.properties.message.encrypted
// from the three message-bearing tools inside the collaboration namespace. A
// same-named function in another namespace, or a direct top-level function, is
// unrelated and must keep its schema untouched.
func clearCollaborationMessageEncryption(tools any) (any, int) {
	list, ok := tools.([]any)
	if !ok {
		return tools, 0
	}
	cleared := 0
	for _, value := range list {
		namespace, ok := value.(map[string]any)
		if !ok || itemTypeOf(namespace) != "namespace" ||
			strings.TrimSpace(fmt.Sprint(namespace["name"])) != officialCollaborationNamespace {
			continue
		}
		inner, ok := namespace["tools"].([]any)
		if !ok {
			continue
		}
		for _, raw := range inner {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(fmt.Sprint(tool["name"]))
			if !officialCollaborationMessageTools[name] {
				continue
			}
			parameters, ok := tool["parameters"].(map[string]any)
			if !ok {
				continue
			}
			properties, ok := parameters["properties"].(map[string]any)
			if !ok {
				continue
			}
			message, ok := properties["message"].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := message["encrypted"]; !ok {
				continue
			}
			delete(message, "encrypted")
			properties["message"] = message
			parameters["properties"] = properties
			tool["parameters"] = parameters
			cleared++
		}
	}
	return tools, cleared
}

// clearCodexCollaborationEncryption strips the encryption flag from the
// collaboration tools carried by a Codex request, both at the top level and in
// any additional_tools input item, and reports the number of cleared properties.
func clearCodexCollaborationEncryption(payload map[string]any) int {
	if payload == nil {
		return 0
	}
	cleared := 0
	if tools, ok := payload["tools"]; ok {
		if rewritten, count := clearCollaborationMessageEncryption(tools); count > 0 {
			payload["tools"] = rewritten
			cleared += count
		}
	}
	input, ok := payload["input"].([]any)
	if !ok {
		return cleared
	}
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok || itemTypeOf(item) != "additional_tools" {
			continue
		}
		if tools, ok := item["tools"]; ok {
			if rewritten, count := clearCollaborationMessageEncryption(tools); count > 0 {
				item["tools"] = rewritten
				cleared += count
			}
		}
	}
	return cleared
}

// openAICiphertextPrefix is the stable start of the unpadded base64url Fernet
// token used by Responses API encrypted content. The prefix alone is not
// sufficient: delegated task text may legitimately begin with the same bytes.
const openAICiphertextPrefix = "gAAAAA"

const (
	openAICiphertextVersion    = byte(0x80)
	openAICiphertextFixedBytes = 57 // version + timestamp + IV + HMAC
	openAICiphertextBlockBytes = 16
)

// looksLikeOpenAICiphertext validates the complete Fernet wire shape instead
// of guessing from a content prefix. Codex serializes plaintext delegated tasks
// and genuine OpenAI ciphertext with identical provenance metadata, so the
// gateway has no producer signal to consult here. Structural validation rejects
// arbitrary prefix collisions; a task body that is itself a complete Fernet
// token remains indistinguishable until the client exposes explicit provenance.
func looksLikeOpenAICiphertext(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, openAICiphertextPrefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) < openAICiphertextFixedBytes+openAICiphertextBlockBytes {
		return false
	}
	return decoded[0] == openAICiphertextVersion &&
		(len(decoded)-openAICiphertextFixedBytes)%openAICiphertextBlockBytes == 0
}

// normalizePlaintextAgentMessages converts an agent_message whose
// encrypted_content is not a genuine OpenAI ciphertext into a plain user
// message, and reports how many items it converted.
//
// A sub-agent task delegated by a third-party parent carries the task text in
// encrypted_content without encryption. The official upstream validates that
// field and rejects foreign content, so the item cannot cross the official
// branch as-is. An item holding genuine official ciphertext is left completely
// untouched: the official upstream can still resolve it, and rewriting it would
// discard a payload only that upstream can read.
func normalizePlaintextAgentMessages(input []any) int {
	converted := 0
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok || itemTypeOf(item) != "agent_message" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		convertedParts := 0
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok || itemTypeOf(part) != "encrypted_content" {
				continue
			}
			encoded, _ := part["encrypted_content"].(string)
			if strings.TrimSpace(encoded) == "" || looksLikeOpenAICiphertext(encoded) {
				continue
			}
			delete(part, "encrypted_content")
			part["type"] = "input_text"
			part["text"] = encoded
			convertedParts++
		}
		if convertedParts == 0 {
			continue
		}
		delete(item, "author")
		delete(item, "recipient")
		item["type"] = "message"
		item["role"] = "user"
		converted++
	}
	return converted
}

// SanitizeParsedRequest neutralizes gateway-issued items in an already parsed
// official-bound payload. It returns false when the payload needs no rewrite.
// The caller can therefore reuse one parse for routing and sanitization while
// keeping the untouched passthrough contract.
func SanitizeParsedRequest(payload map[string]any, logger *activity.Logger) bool {
	if payload == nil {
		return false
	}
	input, _ := payload["input"].([]any)
	hadWopalCompaction := inputHasCapsule(input)
	hadLocalPreviousResponse := hasLocalPreviousResponseID(payload)
	expanded, neutralized := sanitizeRequestInput(input)
	expanded, strippedIDs, droppedReferences, itemTypes := normalizeInputItems(expanded)
	convertedMessages := normalizePlaintextAgentMessages(expanded)
	clearedTools, renamedTools := prepareOfficialCollaboration(payload)
	if neutralized == 0 && strippedIDs == 0 && droppedReferences == 0 && !hadLocalPreviousResponse &&
		convertedMessages == 0 && clearedTools == 0 && renamedTools == 0 {
		return false
	}
	if input != nil {
		payload["input"] = expanded
	}
	if hadWopalCompaction || hadLocalPreviousResponse {
		delete(payload, "previous_response_id")
	}
	if logger != nil {
		fields := collectedNeutralizationFields(payload, neutralized, strippedIDs, droppedReferences, itemTypes)
		if convertedMessages > 0 {
			fields["converted_agent_messages"] = convertedMessages
		}
		if renamedTools > 0 {
			fields["renamed_collaboration_items"] = renamedTools
		}
		if clearedTools > 0 {
			fields["cleared_collaboration_encryption"] = clearedTools
		}
		logger.Write(fields)
	}
	return true
}

// writeParsedRequest replaces a request body with the parsed payload after a
// sanitizer rewrite. It removes compression and keeps Content-Length in sync.
func writeParsedRequest(req *http.Request, payload map[string]any) error {
	if req == nil {
		return fmt.Errorf("official request is nil")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func collectedNeutralizationFields(payload map[string]any, neutralized, strippedIDs, droppedReferences int, itemTypes map[string]int) map[string]any {
	fields := map[string]any{
		"event":              "official_wopal_items_neutralized",
		"model":              routing.StringOrEmpty(payload["model"]),
		"item_count":         neutralized,
		"stripped_item_ids":  strippedIDs,
		"dropped_references": droppedReferences,
	}
	if len(itemTypes) > 0 {
		fields["item_types"] = itemTypes
	}
	return fields
}

// RewriteOfficialRequestBody neutralizes gateway-issued items in an
// official-bound HTTP request body. Requests without gateway-issued items keep
// their exact bytes, headers, and compression.
func RewriteOfficialRequestBody(req *http.Request, logger *activity.Logger) {
	if req.Method != http.MethodPost || req.Body == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, routing.MaxBodyBytes+1))
	if err != nil {
		raw = nil
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if len(raw) > routing.MaxBodyBytes {
		return
	}
	decoded, err := DecodeBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		logger.Write(map[string]any{"event": "official_sanitizer_decode_failed", "error": err.Error()})
		return
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		logger.Write(map[string]any{"event": "official_sanitizer_decode_failed", "error": err.Error()})
		return
	}
	if !SanitizeParsedRequest(payload, logger) {
		return
	}
	if err := writeParsedRequest(req, payload); err != nil {
		logger.Write(map[string]any{"event": "official_sanitizer_encode_failed", "error": err.Error()})
	}
}

// officialItemTypesKeepingID lists the item types whose id carries protocol
// meaning for a stateless request: reasoning ids pair with encrypted_content
// and compaction ids identify the capsule. Every other input item is
// self-describing, so its id only invites a persisted-item lookup that the
// upstream cannot perform when store is false.
var officialItemTypesKeepingID = map[string]bool{
	"reasoning":  true,
	"compaction": true,
}

// officialContentKeys mark an item as carrying its own payload. An item with an
// id and none of these keys can only be resolved by a server-side lookup.
var officialContentKeys = []string{
	"content", "tools", "arguments", "input", "output", "action", "call_id", "summary", "encrypted_content",
}

func officialItemHasPayload(item map[string]any) bool {
	for _, key := range officialContentKeys {
		if value, ok := item[key]; ok && value != nil {
			return true
		}
	}
	return false
}

// normalizeInputItems makes an official-bound input list fully
// self-describing. The official Responses backend rejects any input item that
// can only be resolved through persisted-item lookup when store is false
// ("Supplied input item IDs require persisted-item lookup that is not supported
// by rustponsesapi"). Codex keeps thread items with stable ids, and item
// references appear when a long thread is reconstructed, so the gateway strips
// metadata ids and drops pure references before forwarding.
func normalizeInputItems(input []any) ([]any, int, int, map[string]int) {
	normalized := make([]any, 0, len(input))
	strippedIDs := 0
	droppedReferences := 0
	types := make(map[string]int, 8)
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			normalized = append(normalized, value)
			continue
		}
		itemType := itemTypeOf(item)
		if _, ok := item["id"]; !ok {
			normalized = append(normalized, item)
			types[itemType]++
			continue
		}
		if itemType == "item_reference" {
			droppedReferences++
			types["item_reference(dropped)"]++
			continue
		}
		if !officialItemTypesKeepingID[itemType] && !officialItemHasPayload(item) {
			droppedReferences++
			types[itemType+"(dropped)"]++
			continue
		}
		if !officialItemTypesKeepingID[itemType] {
			delete(item, "id")
			strippedIDs++
		}
		normalized = append(normalized, item)
		types[itemType]++
	}
	return normalized, strippedIDs, droppedReferences, types
}

// SanitizeWebsocketFrame removes gateway-issued items from an official-bound
// websocket frame. It returns the original payload when nothing needs rewriting.
func SanitizeWebsocketFrame(payload []byte, logger *activity.Logger) ([]byte, bool) {
	var message map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&message); err != nil {
		return nil, false
	}
	input, _ := message["input"].([]any)
	hadWopalCompaction := inputHasCapsule(input)
	hadLocalPreviousResponse := hasLocalPreviousResponseID(message)
	expanded, neutralized := sanitizeRequestInput(input)
	expanded, strippedIDs, droppedReferences, itemTypes := normalizeInputItems(expanded)
	convertedMessages := normalizePlaintextAgentMessages(expanded)
	clearedTools, renamedTools := prepareOfficialCollaboration(message)
	if neutralized == 0 && strippedIDs == 0 && droppedReferences == 0 && !hadLocalPreviousResponse &&
		convertedMessages == 0 && clearedTools == 0 && renamedTools == 0 {
		return nil, false
	}
	if input != nil {
		message["input"] = expanded
	}
	if hadWopalCompaction || hadLocalPreviousResponse {
		delete(message, "previous_response_id")
	}
	loggerFields := map[string]any{
		"event":              "official_wopal_items_neutralized",
		"transport":          "websocket",
		"model":              routing.StringOrEmpty(message["model"]),
		"item_count":         neutralized,
		"stripped_item_ids":  strippedIDs,
		"dropped_references": droppedReferences,
	}
	if convertedMessages > 0 {
		loggerFields["converted_agent_messages"] = convertedMessages
	}
	if renamedTools > 0 {
		loggerFields["renamed_collaboration_items"] = renamedTools
	}
	if clearedTools > 0 {
		loggerFields["cleared_collaboration_encryption"] = clearedTools
	}
	if logger != nil && len(itemTypes) > 0 {
		loggerFields["item_types"] = itemTypes
	}
	if logger != nil {
		logger.Write(loggerFields)
	}
	rewritten, err := json.Marshal(message)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}
