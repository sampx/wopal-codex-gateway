package main

import (
	"bytes"
	"encoding/json"
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

func TestWopalTranscriptPrefixIsByteStableAcrossTurns(t *testing.T) {
	model := "wopal-ai/deepseek-v4-flash"
	first, err := prepareWopalWebsocketTurn(map[string]any{
		"type": "response.create", "model": model,
		"input": []any{map[string]any{"role": "user", "type": "message", "id": "u1", "content": "first"}},
	}, model, wopalConversationState{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := prepareWopalWebsocketTurn(map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r1",
		"input": []any{map[string]any{"content": "second", "id": "u2", "type": "message", "role": "user"}},
	}, model, wopalConversationState{
		lastRequest:        first.nextRequest,
		lastResponseOutput: []byte(`[{"role":"assistant","id":"a1","type":"message","content":[{"text":"answer one","type":"output_text"}]}]`),
		lastResponseID:     "r1",
	})
	if err != nil {
		t.Fatal(err)
	}

	third, err := prepareWopalWebsocketTurn(map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r2",
		"input": []any{map[string]any{"type": "message", "role": "user", "id": "u3", "content": "third"}},
	}, model, wopalConversationState{
		lastRequest:        second.nextRequest,
		lastResponseOutput: []byte(`[{"type":"message","role":"assistant","id":"a2","content":[{"type":"output_text","text":"answer two"}]}]`),
		lastResponseID:     "r2",
	})
	if err != nil {
		t.Fatal(err)
	}

	secondInput := inputRawItems(t, second.body)
	thirdInput := inputRawItems(t, third.body)
	if len(thirdInput) <= len(secondInput) {
		t.Fatalf("third input len=%d, second=%d", len(thirdInput), len(secondInput))
	}
	for i := range secondInput {
		if !bytes.Equal(secondInput[i], thirdInput[i]) {
			t.Fatalf("historical item %d changed bytes\nsecond=%s\nthird =%s", i, secondInput[i], thirdInput[i])
		}
	}
}

func TestWopalSessionReleaseDropsTranscriptReferences(t *testing.T) {
	s := &wopalWebsocketSession{state: wopalConversationState{
		lastRequest:        []byte("request-bytes"),
		lastResponseOutput: []byte("output-bytes"),
		lastResponseID:     "r1",
	}}
	want := len(s.state.lastRequest) + len(s.state.lastResponseOutput)
	if got := s.releaseState(); got != want {
		t.Fatalf("released=%d want=%d", got, want)
	}
	if s.state.lastRequest != nil || s.state.lastResponseOutput != nil || s.state.lastResponseID != "" {
		t.Fatalf("state retained after release: %#v", s.state)
	}
}

func TestWopalSessionStateSizeGuard(t *testing.T) {
	if err := validateWopalSessionStateSize(maxWopalSessionStateBytes-1, 1); err != nil {
		t.Fatalf("boundary state rejected: %v", err)
	}
	if err := validateWopalSessionStateSize(maxWopalSessionStateBytes, 1); err == nil {
		t.Fatal("oversized session state was accepted")
	}
}
