package compaction

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func inputRawItems(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var envelope struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return envelope.Input
}

func TestPrepareWopalTurnMergesToolHistory(t *testing.T) {
	state := ConversationState{
		LastRequest:        []byte(`{"model":"wopal-ai/deepseek-v4-flash","stream":true,"input":[{"type":"message","id":"u1","role":"user","content":"run tool"}]}`),
		LastResponseOutput: []byte(`[{"type":"function_call","id":"fc1","call_id":"call1","name":"lookup","arguments":"{}"}]`),
		LastResponseID:     "r1",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": "r1",
		"input": []any{
			map[string]any{"type": "function_call_output", "id": "out1", "call_id": "call1", "output": "ok"},
			map[string]any{"type": "message", "id": "u2", "role": "user", "content": "continue"},
		},
	}
	prepared, err := PrepareTurn(message, "wopal-ai/deepseek-v4-flash", state)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["previous_response_id"]; exists {
		t.Fatal("previous_response_id must be expanded locally")
	}
	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input type %T", body["input"])
	}
	if len(input) != 4 {
		t.Fatalf("input len=%d want 4: %#v", len(input), input)
	}
	seenCall, seenOutput := 0, 0
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "function_call":
			seenCall++
		case "function_call_output":
			seenOutput++
		}
	}
	if seenCall != 1 || seenOutput != 1 {
		t.Fatalf("tool pair counts call=%d output=%d", seenCall, seenOutput)
	}
}

func TestPrepareWopalTurnReplacesFullTranscript(t *testing.T) {
	state := ConversationState{
		LastRequest:        []byte(`{"model":"wopal-ai/deepseek-v4-flash","stream":true,"input":[{"type":"message","id":"old","role":"user","content":"old"}]}`),
		LastResponseOutput: []byte(`[{"type":"message","id":"old-a","role":"assistant","content":[{"type":"output_text","text":"old"}]}]`),
		LastResponseID:     "r1",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash",
		"input": []any{
			map[string]any{"type": "message", "id": "new-u", "role": "user", "content": "new"},
			map[string]any{"type": "message", "id": "new-a", "role": "assistant", "content": "summary"},
		},
	}
	prepared, err := PrepareTurn(message, "wopal-ai/deepseek-v4-flash", state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prepared.Body), `"id":"old"`) || strings.Contains(string(prepared.Body), `"id":"old-a"`) {
		t.Fatalf("stale transcript leaked into replacement: %s", prepared.Body)
	}
}

func TestPrepareWopalTurnRejectsPreviousIDOnFreshSocket(t *testing.T) {
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": "r1",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "next"}},
	}
	_, err := PrepareTurn(message, "wopal-ai/deepseek-v4-flash", ConversationState{})
	if err == nil || !strings.Contains(err.Error(), "previous_response_not_found") {
		t.Fatalf("err=%v", err)
	}
}

func TestWopalTranscriptPrefixIsByteStableAcrossTurns(t *testing.T) {
	model := "wopal-ai/deepseek-v4-flash"
	first, err := PrepareTurn(map[string]any{
		"type": "response.create", "model": model,
		"input": []any{map[string]any{"role": "user", "type": "message", "id": "u1", "content": "first"}},
	}, model, ConversationState{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := PrepareTurn(map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r1",
		"input": []any{map[string]any{"content": "second", "id": "u2", "type": "message", "role": "user"}},
	}, model, ConversationState{
		LastRequest:        first.NextRequest,
		LastResponseOutput: []byte(`[{"role":"assistant","id":"a1","type":"message","content":[{"text":"answer one","type":"output_text"}]}]`),
		LastResponseID:     "r1",
	})
	if err != nil {
		t.Fatal(err)
	}

	third, err := PrepareTurn(map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r2",
		"input": []any{map[string]any{"type": "message", "role": "user", "id": "u3", "content": "third"}},
	}, model, ConversationState{
		LastRequest:        second.NextRequest,
		LastResponseOutput: []byte(`[{"type":"message","role":"assistant","id":"a2","content":[{"type":"output_text","text":"answer two"}]}]`),
		LastResponseID:     "r2",
	})
	if err != nil {
		t.Fatal(err)
	}

	secondInput := inputRawItems(t, second.Body)
	thirdInput := inputRawItems(t, third.Body)
	if len(thirdInput) <= len(secondInput) {
		t.Fatalf("third input len=%d, second=%d", len(thirdInput), len(secondInput))
	}
	for i := range secondInput {
		if !bytes.Equal(secondInput[i], thirdInput[i]) {
			t.Fatalf("historical item %d changed bytes\nsecond=%s\nthird =%s", i, secondInput[i], thirdInput[i])
		}
	}
}

func TestModelOmittedAfterIntraWopalSwitchUsesLatestModel(t *testing.T) {
	state := ConversationState{LastRequest: []byte(`{"model":"wopal-ai/second","input":[]}`), LastResponseID: "r1"}
	prepared, err := PrepareTurn(map[string]any{"type": "response.append", "previous_response_id": "r1", "input": []any{}}, "wopal-ai/first", state)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(prepared.Body, &body)
	if body["model"] != "wopal-ai/second" {
		t.Fatalf("inherited stale handshake model: %s", prepared.Body)
	}
}
