// Package routing holds the pure policy shared by the gateway's notification,
// quota, compaction and proxy packages. It depends on the standard library
// only, so every other package may use it without creating a cycle.
package routing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	// MaxBodyBytes bounds any request body the gateway inspects.
	MaxBodyBytes = 64 * 1024 * 1024
	// OfficialQuotaFailoverModel is the Wopal model used when the official
	// quota reserve is protected.
	OfficialQuotaFailoverModel = "wopal-ai/deepseek-v4.1-flash"
)

// Kind is the provider family a model name resolves to.
type Kind string

const (
	Unknown  Kind = "unknown"
	Official Kind = "official"
	Wopal    Kind = "wopal-ai"
	Disabled Kind = "disabled"
)

// Classify maps a model name to its provider family. Values are part of the
// routing contract and must stay stable.
func Classify(model string) Kind {
	model = strings.TrimSpace(model)
	if model == "" {
		return Unknown
	}
	if strings.HasPrefix(model, "wopal-ai/") {
		return Wopal
	}
	if strings.Contains(model, "/") {
		return Disabled
	}
	return Official
}

// JoinURLPath joins a target base path and a request suffix.
func JoinURLPath(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

// ParseRoutingHintModel extracts model=<name> from X-Codex-Routing-Hint.
func ParseRoutingHintModel(value string) string {
	for _, part := range strings.Split(value, ";") {
		key, raw, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "model") {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

// InputHasCompactionTrigger reports whether an input list carries the
// compaction trigger item.
func InputHasCompactionTrigger(input []any) bool {
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if ok && strings.TrimSpace(StringOrEmpty(item["type"])) == "compaction_trigger" {
			return true
		}
	}
	return false
}

// CloneMessage shallow-copies a JSON object.
func CloneMessage(message map[string]any) map[string]any {
	cloned := make(map[string]any, len(message))
	for key, value := range message {
		cloned[key] = value
	}
	return cloned
}

// PreviousResponseID returns the trimmed previous_response_id, if present.
func PreviousResponseID(message map[string]any) string {
	value, _ := message["previous_response_id"].(string)
	return strings.TrimSpace(value)
}

// StringOrEmpty returns a string value or an empty string.
func StringOrEmpty(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// ResponseOutputFromRawEvent extracts the response output array and id from a
// response.completed event, falling back to collected output items.
func ResponseOutputFromRawEvent(data []byte, collected []json.RawMessage) ([]byte, string) {
	var envelope struct {
		Response struct {
			ID     string          `json:"id"`
			Output json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		responseID := strings.TrimSpace(envelope.Response.ID)
		trimmed := bytes.TrimSpace(envelope.Response.Output)
		if len(trimmed) > 2 && trimmed[0] == '[' {
			return bytes.Clone(trimmed), responseID
		}
		if len(collected) == 0 {
			return []byte("[]"), responseID
		}
		return MarshalRawMessages(collected), responseID
	}
	return []byte("[]"), ""
}

// RawOutputItemFromEvent extracts the item from a response.output_item event.
func RawOutputItemFromEvent(data []byte) json.RawMessage {
	var envelope struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	trimmed := bytes.TrimSpace(envelope.Item)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return bytes.Clone(trimmed)
}

// MarshalRawMessages builds a JSON array from raw items without re-encoding.
func MarshalRawMessages(items []json.RawMessage) []byte {
	length := 2
	if len(items) > 1 {
		length += len(items) - 1
	}
	for _, item := range items {
		length += len(item)
	}
	out := make([]byte, 0, length)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	out = append(out, ']')
	return out
}

// AccountID returns the trimmed ChatGPT account header.
func AccountID(req *http.Request) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(req.Header.Get("ChatGPT-Account-ID"))
}
