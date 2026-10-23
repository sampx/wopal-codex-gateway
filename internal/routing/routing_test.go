package routing

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"gpt-6.1-sol":                  Official,
		"wopal-ai/deepseek-v4.1-flash": Wopal,
		"wopal-ai/x":                   Wopal,
		"ollama/llama3":                Disabled,
		"ollama/qwen3":                 Disabled,
		"other/model":                  Disabled,
		"":                             Unknown,
	}
	for model, want := range cases {
		if got := Classify(model); got != want {
			t.Fatalf("Classify(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestJoinURLPath(t *testing.T) {
	cases := []struct{ base, suffix, want string }{
		{"/backend-api/codex", "/responses", "/backend-api/codex/responses"},
		{"/backend-api/codex/", "responses", "/backend-api/codex/responses"},
		{"", "/v1/responses", "/v1/responses"},
	}
	for _, tc := range cases {
		if got := JoinURLPath(tc.base, tc.suffix); got != tc.want {
			t.Fatalf("JoinURLPath(%q, %q) = %q, want %q", tc.base, tc.suffix, got, tc.want)
		}
	}
}

func TestParseRoutingHintModel(t *testing.T) {
	if got := ParseRoutingHintModel("model=wopal-ai/glm-5.3-flash;tier=auto"); got != "wopal-ai/glm-5.3-flash" {
		t.Fatalf("model = %q", got)
	}
	if got := ParseRoutingHintModel("tier=auto"); got != "" {
		t.Fatalf("model = %q", got)
	}
}

func TestInputHasCompactionTrigger(t *testing.T) {
	if !InputHasCompactionTrigger([]any{map[string]any{"type": "compaction_trigger"}}) {
		t.Fatal("trigger was not detected")
	}
	if InputHasCompactionTrigger([]any{map[string]any{"type": "message"}}) {
		t.Fatal("message was treated as a trigger")
	}
}

func TestCloneMessageAndPreviousResponseID(t *testing.T) {
	original := map[string]any{"model": "gpt-6.1-sol", "previous_response_id": " r1 "}
	cloned := CloneMessage(original)
	cloned["model"] = "changed"
	if original["model"] != "gpt-6.1-sol" {
		t.Fatal("CloneMessage mutated the source map")
	}
	if got := PreviousResponseID(original); got != "r1" {
		t.Fatalf("previous response id = %q", got)
	}
}

func TestReadSSEJoinsDataLines(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"a\"}\n\n" +
		"data: first\ndata: second\n\n" +
		"data: [DONE]\n\n"
	var got []string
	if err := ReadSSE(strings.NewReader(stream), func(data []byte) error {
		got = append(got, string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"type":"a"}`, "first\nsecond", "[DONE]"}
	if len(got) != len(want) {
		t.Fatalf("events = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResponseOutputFromRawEvent(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"id":"r1","output":[{"type":"message"}]}}`)
	output, id := ResponseOutputFromRawEvent(event, nil)
	if id != "r1" || !bytes.Contains(output, []byte(`"type":"message"`)) {
		t.Fatalf("output=%s id=%q", output, id)
	}

	empty, id := ResponseOutputFromRawEvent([]byte(`{"response":{"id":"r2","output":[]}}`), []json.RawMessage{json.RawMessage(`{"type":"a"}`)})
	if id != "r2" || !bytes.Contains(empty, []byte(`"type":"a"`)) {
		t.Fatalf("collected output=%s id=%q", empty, id)
	}
}

func TestRawOutputItemFromEvent(t *testing.T) {
	item := RawOutputItemFromEvent([]byte(`{"item":{"type":"message","id":"m1"}}`))
	if !bytes.Contains(item, []byte(`"id":"m1"`)) {
		t.Fatalf("item = %s", item)
	}
	if item := RawOutputItemFromEvent([]byte(`{"item":null}`)); item != nil {
		t.Fatalf("null item = %s", item)
	}
}

func TestStringOrEmpty(t *testing.T) {
	if StringOrEmpty("value") != "value" || StringOrEmpty(42) != "" {
		t.Fatal("StringOrEmpty did not preserve its contract")
	}
}

func TestMarshalRawMessages(t *testing.T) {
	got := MarshalRawMessages([]json.RawMessage{json.RawMessage(`{"a":1}`), json.RawMessage(`{"b":2}`)})
	if string(got) != `[{"a":1},{"b":2}]` {
		t.Fatalf("marshal = %s", got)
	}
}
