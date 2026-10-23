package proxy

import (
	"testing"

	"wopal-codex-gateway/internal/compaction"
)

func testWopalCompactionItem(t *testing.T, id, summary string) map[string]any {
	t.Helper()
	sealed, err := compaction.SealCapsule(compaction.Capsule{
		Version: 1,
		ID:      id,
		Model:   "wopal-ai/deepseek-v4.1-flash-free",
		Summary: summary,
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "compaction", "id": id, "encrypted_content": sealed}
}

func testWopalReasoningItem() map[string]any {
	return map[string]any{"type": "reasoning", "id": "rs_chatcmpl-abc123_0", "encrypted_content": "wopal-reasoning-ciphertext"}
}

func testWopalAdditionalTools() map[string]any {
	return map[string]any{
		"type": "additional_tools",
		"role": "developer",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "browser",
			"tools": []any{map[string]any{
				"type": "function", "name": "open_url", "description": "Open a URL in the built-in browser",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"}},
			}},
		}},
	}
}
