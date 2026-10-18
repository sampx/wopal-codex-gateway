package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
		bytes.Contains(payload, []byte(wopalReasoningIDPrefix)) || bytes.Contains(payload, []byte("rs_cmb-"))
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
		if itemTypeOf(item) == "reasoning" &&
			isWopalReasoningID(reasoningItemID(item)) {
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
func rewriteOfficialRequestBody(req *http.Request, logger *activityLogger) {
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
		logger.write(map[string]any{"event": "official_sanitizer_decode_failed", "error": err.Error()})
		return
	}
	if !containsOfficialSanitizeMarker(decoded) {
		return
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		logger.write(map[string]any{"event": "official_sanitizer_decode_failed", "error": err.Error()})
		return
	}
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	hadWopalCompaction := inputHasWopalCompactionCapsule(input)
	hadLocalPreviousResponse := hasWopalLocalPreviousResponseID(payload)
	expanded, neutralized := sanitizeOfficialRequestInput(input)
	if neutralized == 0 && !hadLocalPreviousResponse {
		return
	}
	payload["input"] = expanded
	if hadWopalCompaction || hadLocalPreviousResponse {
		delete(payload, "previous_response_id")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.write(map[string]any{"event": "official_sanitizer_encode_failed", "error": err.Error()})
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	logger.write(map[string]any{
		"event":      "official_wopal_items_neutralized",
		"model":      stringOrEmpty(payload["model"]),
		"item_count": neutralized,
	})
}
