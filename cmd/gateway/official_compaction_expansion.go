package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"wopal-codex-gateway/internal/activity"
)

// Official-branch request sanitization. OpenAI verifies compaction and
// reasoning encrypted_content and rejects unknown payloads with
// invalid_encrypted_content, which fails closed browser and computer-use auto
// review after a Wopal compaction, or fails a turn after switching a
// Wopal-compacted thread to an official model. The gateway therefore never
// lets its own opaque items cross the official branch.

// officialCompactionUnavailableContent replaces a Wopal compaction capsule that
// cannot be opened. It keeps official auto review running instead of failing
// closed, without inventing conversation history.
const officialCompactionUnavailableContent = "The earlier part of this conversation was compacted by a Wopal AI model. The preserved compaction summary could not be restored for this request; continue from the remaining conversation context."

// wopalReasoningIDPrefix identifies reasoning items minted by the Wopal
// chat-completions translation; OpenAI reasoning items never use this prefix.
const (
	wopalReasoningIDPrefix          = "rs_chatcmpl"
	wopalCompactionResponseIDPrefix = "resp_wopal_compact_"
)

func isWopalReasoningID(id string) bool {
	return strings.HasPrefix(id, wopalReasoningIDPrefix) || strings.HasPrefix(id, "rs_cmb-")
}

func containsOfficialSanitizeMarker(payload []byte) bool {
	return bytes.Contains(payload, []byte(wopalCompactionCapsulePrefix)) ||
		bytes.Contains(payload, []byte(wopalCompactionResponseIDPrefix)) ||
		bytes.Contains(payload, []byte(wopalReasoningIDPrefix)) ||
		bytes.Contains(payload, []byte("rs_cmb-")) ||
		bytes.Contains(payload, []byte(`"reasoning"`))
}

func hasWopalLocalPreviousResponseID(payload map[string]any) bool {
	return strings.HasPrefix(strings.TrimSpace(stringOrEmpty(payload["previous_response_id"])), wopalCompactionResponseIDPrefix)
}

func itemTypeOf(item map[string]any) string {
	return strings.TrimSpace(fmt.Sprint(item["type"]))
}

func reasoningItemID(item map[string]any) string {
	id, _ := item["id"].(string)
	return strings.TrimSpace(id)
}

func wopalEncryptedContent(item map[string]any) string {
	encoded, _ := item["encrypted_content"].(string)
	return strings.TrimSpace(encoded)
}

// A reasoning item without encrypted content cannot be resolved by the
// official API when the request uses store=false. That is true for Wopal-minted
// reasoning items regardless of how their id was generated; the id prefix is
// not a reliable provenance marker.
func reasoningItemNeedsNeutralization(item map[string]any) bool {
	if itemTypeOf(item) != "reasoning" {
		return false
	}
	if isWopalReasoningID(reasoningItemID(item)) {
		return true
	}
	return wopalEncryptedContent(item) == ""
}

// sanitizeOfficialRequestInput rewrites request input items for the official
// branch and reports how many gateway-issued items were neutralized.
func inputHasWopalCompactionCapsule(input []any) bool {
	for _, value := range input {
		item, ok := value.(map[string]any)
		if ok && strings.HasPrefix(wopalEncryptedContent(item), wopalCompactionCapsulePrefix) {
			return true
		}
	}
	return false
}

func sanitizeOfficialRequestInput(input []any) ([]any, int) {
	requestHasTools := false
	for _, value := range input {
		if isWopalCompactionRetainedItem(value) {
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
		if encoded := wopalEncryptedContent(item); strings.HasPrefix(encoded, wopalCompactionCapsulePrefix) {
			neutralized++
			if base, err := expandWopalCapsuleForOfficial(item, requestHasTools); err == nil {
				expanded = append(expanded, base...)
			} else {
				expanded = append(expanded, wopalCompactionUnavailableMessage(item["id"]))
			}
			continue
		}
		if reasoningItemNeedsNeutralization(item) {
			neutralized++
			continue
		}
		expanded = append(expanded, value)
	}
	return expanded, neutralized
}

func expandWopalCapsuleForOfficial(item map[string]any, requestHasTools bool) ([]any, error) {
	capsule, err := openWopalCompactionCapsule(wopalEncryptedContent(item))
	if err != nil {
		return nil, err
	}
	base, err := wopalCompactionExpandedBase(capsule)
	if err != nil {
		return nil, err
	}
	if requestHasTools && len(capsule.Prefix) > 0 {
		base = base[len(capsule.Prefix):]
	}
	return base, nil
}

func wopalCompactionUnavailableMessage(id any) map[string]any {
	return map[string]any{
		"type":    "message",
		"id":      wopalCompactionSummaryID(id),
		"role":    "developer",
		"content": officialCompactionUnavailableContent,
	}
}

func stringOrEmpty(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// rewriteOfficialRequestBody neutralizes gateway-issued items in an
// official-bound HTTP request body. Requests without gateway-issued items keep
// their exact bytes, headers, and compression.
func rewriteOfficialRequestBody(req *http.Request, logger *activity.Logger) {
	if req.Method != http.MethodPost || req.Body == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRoutingBodyBytes+1))
	if err != nil {
		raw = nil
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if len(raw) > maxRoutingBodyBytes {
		return
	}
	decoded, err := decodeRoutingBody(raw, req.Header.Get("Content-Encoding"))
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
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	hadWopalCompaction := inputHasWopalCompactionCapsule(input)
	hadLocalPreviousResponse := hasWopalLocalPreviousResponseID(payload)
	expanded, neutralized := sanitizeOfficialRequestInput(input)
	expanded, strippedIDs, droppedReferences, itemTypes := normalizeOfficialInputItems(expanded)
	if neutralized == 0 && strippedIDs == 0 && droppedReferences == 0 && !hadLocalPreviousResponse {
		return
	}
	payload["input"] = expanded
	if hadWopalCompaction || hadLocalPreviousResponse {
		delete(payload, "previous_response_id")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Write(map[string]any{"event": "official_sanitizer_encode_failed", "error": err.Error()})
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	fields := map[string]any{
		"event":              "official_wopal_items_neutralized",
		"model":              stringOrEmpty(payload["model"]),
		"item_count":         neutralized,
		"stripped_item_ids":  strippedIDs,
		"dropped_references": droppedReferences,
	}
	if len(itemTypes) > 0 {
		fields["item_types"] = itemTypes
	}
	logger.Write(fields)
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

// normalizeOfficialInputItems makes an official-bound input list fully
// self-describing. The official Responses backend rejects any input item that
// can only be resolved through persisted-item lookup when store is false
// ("Supplied input item IDs require persisted-item lookup that is not supported
// by rustponsesapi"). Codex keeps thread items with stable ids, and item
// references appear when a long thread is reconstructed, so the gateway strips
// metadata ids and drops pure references before forwarding.
func normalizeOfficialInputItems(input []any) ([]any, int, int, map[string]int) {
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
