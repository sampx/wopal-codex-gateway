package proxy

import "strings"

// sanitizeQuotaFallbackHistory prepares official Responses history for an
// external-provider fallback. CLIProxyAPI's Responses->chat translator uses a
// synthetic "[reasoning unavailable]" string when a session advertises
// reasoning but an historical reasoning item has no usable summary. That text
// is not part of the user's conversation and must never enter model context.
//
// The gateway therefore removes session-level reasoning flags on the fallback
// replay and converts only real historical summaries into ordinary assistant
// context. Summary-less reasoning items are omitted. Tool calls, outputs and
// regular messages keep their relative order and identity.
func sanitizeQuotaFallbackHistory(message map[string]any) {
	if message == nil {
		return
	}
	delete(message, "reasoning")
	delete(message, "reasoning_effort")

	input, ok := message["input"].([]any)
	if !ok {
		return
	}
	normalized := make([]any, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			normalized = append(normalized, raw)
			continue
		}

		itemType := strings.TrimSpace(stringValue(item["type"]))
		summary := collectUsableQuotaReasoning(item)
		if itemType == "reasoning" {
			if summary != "" {
				normalized = append(normalized, quotaReasoningSummaryMessage(summary))
			}
			continue
		}

		if _, exists := item["reasoning_content"]; exists {
			delete(item, "reasoning_content")
			if summary != "" {
				normalized = append(normalized, quotaReasoningSummaryMessage(summary))
			}
		}
		normalized = append(normalized, item)
	}
	message["input"] = normalized
}

func collectUsableQuotaReasoning(item map[string]any) string {
	if item == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if summary, ok := item["summary"].([]any); ok {
		for _, raw := range summary {
			entry, _ := raw.(map[string]any)
			if strings.TrimSpace(stringValue(entry["type"])) != "summary_text" {
				continue
			}
			if text := usableQuotaReasoningText(stringValue(entry["text"])); text != "" {
				parts = appendUniqueReasoningPart(parts, text)
			}
		}
	}
	if text := usableQuotaReasoningText(stringValue(item["reasoning_content"])); text != "" {
		parts = appendUniqueReasoningPart(parts, text)
	}
	return strings.Join(parts, "\n")
}

func usableQuotaReasoningText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "[reasoning unavailable]") {
		return ""
	}
	return value
}

func appendUniqueReasoningPart(parts []string, value string) []string {
	for _, existing := range parts {
		if existing == value {
			return parts
		}
	}
	return append(parts, value)
}

func quotaReasoningSummaryMessage(summary string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": summary},
		},
	}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
