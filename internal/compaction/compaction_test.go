package compaction

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWopalCompactionPromptPreservesCodexHandoffContract(t *testing.T) {
	for _, required := range []string{
		"CONTEXT CHECKPOINT COMPACTION",
		"Current progress and key decisions made",
		"Important context, constraints, or user preferences",
		"What remains to be done",
		"critical data, examples, or references",
		"exact paths",
		"commands or tests and their outcomes",
	} {
		if !strings.Contains(Prompt, required) {
			t.Fatalf("compaction prompt missing %q", required)
		}
	}
}

func TestValidateWopalCompactionSummaryRejectsAbnormallyShortOutput(t *testing.T) {
	if err := ValidateSummary("ok"); err == nil {
		t.Fatal("abnormally short compaction summary was accepted")
	}
	if err := ValidateSummary("Current progress: routing is complete. Next step: run the focused regression suite."); err != nil {
		t.Fatalf("reasonable compaction summary rejected: %v", err)
	}
}

func TestDedicatedCompactionModelOverridesRequestedModel(t *testing.T) {
	message := map[string]any{
		"type":  "response.create",
		"model": "gpt-6.1-sol",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "remember me"},
			map[string]any{"type": "compaction_trigger"},
		},
	}
	prepared, err := PrepareSummaryTurn(message, DedicatedModel, ConversationState{})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != DedicatedModel {
		t.Fatalf("compaction model = %v, want %s", body["model"], DedicatedModel)
	}
}

func TestWopalCompactionCapsuleRoundTripAndTamperDetection(t *testing.T) {
	ConfigureKey("unit-test-secret")
	sealed, err := SealCapsule(Capsule{
		Version: 1,
		ID:      "cmp-test",
		Model:   "wopal-ai/deepseek-v4.1-flash-free",
		Summary: "remember ORCHID-7319",
		Prefix:  []json.RawMessage{rawWopalAdditionalTools(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "ORCHID-7319") || strings.Contains(sealed, "open_url") {
		t.Fatal("sealed capsule leaked plaintext state")
	}
	opened, err := OpenCapsule(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != "cmp-test" || opened.Summary != "remember ORCHID-7319" || len(opened.Prefix) != 1 {
		t.Fatalf("opened capsule = %#v", opened)
	}
	if !strings.Contains(string(opened.Prefix[0]), "open_url") {
		t.Fatalf("retained tool prefix missing after capsule round trip: %s", opened.Prefix[0])
	}

	tampered := []byte(sealed)
	idx := len(tampered) - 2
	if tampered[idx] == 'A' {
		tampered[idx] = 'B'
	} else {
		tampered[idx] = 'A'
	}
	if _, err := OpenCapsule(string(tampered)); err == nil {
		t.Fatal("tampered compaction capsule was accepted")
	}
}

func TestPrepareWopalTurnRestoresFreshCompactionReplay(t *testing.T) {
	ConfigureKey("fresh-replay-secret")
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
	prepared, err := PrepareTurn(message, "wopal-ai/deepseek-v4.1-flash-free", ConversationState{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.NextCompactionID != "cmp-fresh" {
		t.Fatalf("next compaction id = %q", prepared.NextCompactionID)
	}
	if strings.Contains(string(prepared.Body), "previous_response_id") {
		t.Fatalf("stale previous_response_id survived replay expansion: %s", prepared.Body)
	}
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 2 {
		t.Fatalf("input len=%d want 2: %s", len(body.Input), prepared.Body)
	}
	if body.Input[0]["role"] != "developer" || !strings.Contains(body.Input[0]["content"].(string), "ORCHID-7319") {
		t.Fatalf("compaction summary was not restored: %#v", body.Input[0])
	}
	if body.Input[1]["id"] != "u2" {
		t.Fatalf("tail item missing: %#v", body.Input[1])
	}
}

func TestPrepareWopalTurnRestoresAdditionalToolsFromCompactionCapsule(t *testing.T) {
	ConfigureKey("tool-replay-secret")
	sealed, err := SealCapsule(Capsule{
		Version: 1, ID: "cmp-tools", Model: "wopal-ai/deepseek-v4.1-flash-free",
		Summary: "Continue the browser task.",
		Prefix:  []json.RawMessage{rawWopalAdditionalTools(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4.1-flash-free",
		"previous_response_id": "resp-before-reconnect",
		"input": []any{
			map[string]any{"type": "compaction", "id": "cmp-tools", "encrypted_content": sealed},
			map[string]any{"type": "message", "id": "u-tools", "role": "user", "content": "Open the page."},
		},
	}
	prepared, err := PrepareTurn(message, "wopal-ai/deepseek-v4.1-flash-free", ConversationState{})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 3 {
		t.Fatalf("input len=%d want 3: %s", len(body.Input), prepared.Body)
	}
	if body.Input[0]["type"] != "additional_tools" || !strings.Contains(string(prepared.Body), "open_url") {
		t.Fatalf("additional tools were not restored before the summary: %s", prepared.Body)
	}
	if !strings.Contains(string(prepared.Body), "Continue the browser task.") || body.Input[2]["id"] != "u-tools" {
		t.Fatalf("summary/tail missing after tool replay: %s", prepared.Body)
	}
}

func TestPrepareWopalTurnReusesSameCompactionWithoutResettingPostCompactHistory(t *testing.T) {
	ConfigureKey("repeat-replay-secret")
	item := testWopalCompactionItem(t, "cmp-repeat", "The important token is ORCHID-7319.")
	base := map[string]any{
		"model":  "wopal-ai/deepseek-v4.1-flash-free",
		"stream": true,
		"input": []any{
			summaryMessage(Capsule{Version: 1, ID: "cmp-repeat", Summary: "The important token is ORCHID-7319."}),
			map[string]any{"type": "message", "id": "u3", "role": "user", "content": "after compact one"},
		},
	}
	baseRaw, _ := json.Marshal(base)
	state := ConversationState{
		LastRequest:        baseRaw,
		LastResponseOutput: []byte(`[{"type":"message","id":"a3","role":"assistant","content":[{"type":"output_text","text":"post compact answer"}]}]`),
		LastResponseID:     "r3",
		ActiveCompactionID: "cmp-repeat",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4.1-flash-free", "previous_response_id": "r3",
		"input": []any{
			item,
			map[string]any{"type": "message", "id": "u4", "role": "user", "content": "after compact two"},
		},
	}
	prepared, err := PrepareTurn(message, "wopal-ai/deepseek-v4.1-flash-free", state)
	if err != nil {
		t.Fatal(err)
	}
	body := string(prepared.Body)
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
