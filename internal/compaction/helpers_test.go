package compaction

import (
	"encoding/json"
	"fmt"
	"testing"
)

func testWopalCompactionItem(t *testing.T, id, summary string) map[string]any {
	t.Helper()
	sealed, err := SealCapsule(Capsule{
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

func rawWopalAdditionalTools(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(testWopalAdditionalTools())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testWopalReasoningItem() map[string]any {
	return map[string]any{"type": "reasoning", "id": "rs_chatcmpl-abc123_0", "encrypted_content": "wopal-reasoning-ciphertext"}
}

func toString(value any) string { return fmt.Sprint(value) }
