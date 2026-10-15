package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareWopalTurnMergesToolHistory(t *testing.T) {
	state := wopalConversationState{
		lastRequest:        []byte(`{"model":"wopal-ai/deepseek-v4-flash","stream":true,"input":[{"type":"message","id":"u1","role":"user","content":"run tool"}]}`),
		lastResponseOutput: []byte(`[{"type":"function_call","id":"fc1","call_id":"call1","name":"lookup","arguments":"{}"}]`),
		lastResponseID:     "r1",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": "r1",
		"input": []any{
			map[string]any{"type": "function_call_output", "id": "out1", "call_id": "call1", "output": "ok"},
			map[string]any{"type": "message", "id": "u2", "role": "user", "content": "continue"},
		},
	}
	prepared, err := prepareWopalWebsocketTurn(message, "wopal-ai/deepseek-v4-flash", state)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(prepared.body, &body); err != nil {
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
	state := wopalConversationState{
		lastRequest:        []byte(`{"model":"wopal-ai/deepseek-v4-flash","stream":true,"input":[{"type":"message","id":"old","role":"user","content":"old"}]}`),
		lastResponseOutput: []byte(`[{"type":"message","id":"old-a","role":"assistant","content":[{"type":"output_text","text":"old"}]}]`),
		lastResponseID:     "r1",
	}
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash",
		"input": []any{
			map[string]any{"type": "message", "id": "new-u", "role": "user", "content": "new"},
			map[string]any{"type": "message", "id": "new-a", "role": "assistant", "content": "summary"},
		},
	}
	prepared, err := prepareWopalWebsocketTurn(message, "wopal-ai/deepseek-v4-flash", state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prepared.body), `"id":"old"`) || strings.Contains(string(prepared.body), `"id":"old-a"`) {
		t.Fatalf("stale transcript leaked into replacement: %s", prepared.body)
	}
}

func TestPrepareWopalTurnRejectsPreviousIDOnFreshSocket(t *testing.T) {
	message := map[string]any{
		"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": "r1",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "next"}},
	}
	_, err := prepareWopalWebsocketTurn(message, "wopal-ai/deepseek-v4-flash", wopalConversationState{})
	if err == nil || !strings.Contains(err.Error(), "previous_response_not_found") {
		t.Fatalf("err=%v", err)
	}
}
