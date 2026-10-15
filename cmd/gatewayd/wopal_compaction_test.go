package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func testWopalCompactionItem(t *testing.T, id, summary string) map[string]any {
	t.Helper()
	sealed, err := sealWopalCompactionCapsule(wopalCompactionCapsule{
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

func TestWopalCompactionCapsuleRoundTripAndTamperDetection(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	sealed, err := sealWopalCompactionCapsule(wopalCompactionCapsule{
		Version: 1,
		ID:      "cmp-test",
		Model:   "wopal-ai/deepseek-v4.1-flash-free",
		Summary: "remember ORCHID-7319",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "ORCHID-7319") {
		t.Fatal("sealed capsule leaked plaintext summary")
	}
	opened, err := openWopalCompactionCapsule(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != "cmp-test" || opened.Summary != "remember ORCHID-7319" {
		t.Fatalf("opened capsule = %#v", opened)
	}

	tampered := []byte(sealed)
	idx := len(tampered) - 2
	if tampered[idx] == 'A' {
		tampered[idx] = 'B'
	} else {
		tampered[idx] = 'A'
	}
	if _, err := openWopalCompactionCapsule(string(tampered)); err == nil {
		t.Fatal("tampered compaction capsule was accepted")
	}
}

func TestPrepareWopalTurnRestoresFreshCompactionReplay(t *testing.T) {
	configureWopalCompactionKey("fresh-replay-secret")
	item := testWopalCompactionItem(t, "cmp-fresh", "The important token is ORCHID-7319.")
	message := map[string]any{
		"type":                 "response.create",
		"model":                "wopal-ai/deepseek-v4.1-flash-free",
		"previous_response_id": "resp-before-reconnect",
		"input": []any{
			item,
			map[string]any{"type": "message", "id": "u2", "role": "user", "content": "what token?"},
		},
	}
	prepared, err := prepareWopalWebsocketTurn(message, "wopal-ai/deepseek-v4.1-flash-free", wopalConversationState{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.nextCompactionID != "cmp-fresh" {
		t.Fatalf("next compaction id = %q", prepared.nextCompactionID)
	}
	if strings.Contains(string(prepared.body), "previous_response_id") {
		t.Fatalf("stale previous_response_id survived replay expansion: %s", prepared.body)
	}
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(prepared.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 2 {
		t.Fatalf("input len=%d want 2: %s", len(body.Input), prepared.body)
	}
	if body.Input[0]["role"] != "developer" || !strings.Contains(body.Input[0]["content"].(string), "ORCHID-7319") {
		t.Fatalf("compaction summary was not restored: %#v", body.Input[0])
	}
	if body.Input[1]["id"] != "u2" {
		t.Fatalf("tail item missing: %#v", body.Input[1])
	}
}

func TestPrepareWopalTurnReusesSameCompactionWithoutResettingPostCompactHistory(t *testing.T) {
	configureWopalCompactionKey("repeat-replay-secret")
	item := testWopalCompactionItem(t, "cmp-repeat", "The important token is ORCHID-7319.")
	base := map[string]any{
		"model":  "wopal-ai/deepseek-v4.1-flash-free",
		"stream": true,
		"input": []any{
			wopalCompactionSummaryMessage(wopalCompactionCapsule{Version: 1, ID: "cmp-repeat", Summary: "The important token is ORCHID-7319."}),
			map[string]any{"type": "message", "id": "u3", "role": "user", "content": "after compact one"},
		},
	}
	baseRaw, _ := json.Marshal(base)
	state := wopalConversationState{
		lastRequest:        baseRaw,
		lastResponseOutput: []byte(`[{"type":"message","id":"a3","role":"assistant","content":[{"type":"output_text","text":"post compact answer"}]}]`),
		lastResponseID:     "r3",
		activeCompactionID: "cmp-repeat",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4.1-flash-free", "previous_response_id": "r3",
		"input": []any{
			item,
			map[string]any{"type": "message", "id": "u4", "role": "user", "content": "after compact two"},
		},
	}
	prepared, err := prepareWopalWebsocketTurn(message, "wopal-ai/deepseek-v4.1-flash-free", state)
	if err != nil {
		t.Fatal(err)
	}
	body := string(prepared.body)
	for _, want := range []string{"ORCHID-7319", `"id":"u3"`, `"id":"a3"`, `"id":"u4"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("reused compaction lost %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"type":"compaction"`) {
		t.Fatalf("opaque compaction item leaked to chat-backed request: %s", body)
	}
	if strings.Count(body, "wopal_compaction_summary_cmp-repeat") != 1 {
		t.Fatalf("same compaction summary was duplicated: %s", body)
	}
}
