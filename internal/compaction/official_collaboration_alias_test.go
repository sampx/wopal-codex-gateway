package compaction

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOfficialCollaborationSchemasWithoutInput(t *testing.T) {
	for _, flagPresent := range []bool{true, false} {
		payload := map[string]any{"tools": collaborationNamespaceTools()}
		if !flagPresent {
			clearCodexCollaborationEncryption(payload)
		}
		if !SanitizeParsedRequest(payload, nil) {
			t.Fatal("schema was not prepared")
		}
		ns := payload["tools"].([]any)[0].(map[string]any)
		if ns["name"] == "collaboration" {
			t.Fatal("modified schema still uses a reserved name")
		}
		if _, exists := payload["input"]; exists {
			t.Fatal("invented input on tools-only request")
		}
		if SanitizeParsedRequest(payload, nil) {
			t.Fatal("rewrite is not idempotent")
		}
	}
}

func TestOfficialCollaborationNamespaceConflict(t *testing.T) {
	payload := map[string]any{"tools": append(collaborationNamespaceTools(), map[string]any{"type": "namespace", "name": officialCollaborationAlias, "tools": []any{}})}
	before, _ := json.Marshal(payload)
	if OfficialCollaborationRewriteRequired(payload) {
		t.Fatal("claimed client-owned namespace")
	}
	SanitizeParsedRequest(payload, nil)
	after, _ := json.Marshal(payload)
	if !bytes.Equal(before, after) {
		t.Fatalf("namespace conflict mutated native schemas: %s", after)
	}
}

func TestOfficialCollaborationPrefixedToolConflicts(t *testing.T) {
	for _, name := range []string{"collaboration-optimize", "collaboration-optimize.tool", "collaboration-optimize__tool"} {
		for _, location := range []string{"top", "additional", "nested"} {
			t.Run(name+"/"+location, func(t *testing.T) {
				conflict := map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}
				payload := map[string]any{"tools": collaborationNamespaceTools()}
				switch location {
				case "top":
					payload["tools"] = append(payload["tools"].([]any), conflict)
				case "additional":
					payload["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{conflict}}}
				case "nested":
					payload["tools"] = append(payload["tools"].([]any), map[string]any{"type": "namespace", "name": "custom", "tools": []any{conflict}})
				}
				before, _ := json.Marshal(payload)
				if OfficialCollaborationRewriteRequired(payload) {
					t.Error("claimed client-owned alias tool")
				}
				SanitizeParsedRequest(payload, nil)
				after, _ := json.Marshal(payload)
				if !bytes.Equal(before, after) {
					t.Fatal("client-owned tool collision changed request")
				}
			})
		}
	}
}

func TestOfficialCollaborationRestoresOnlyProtocolIdentities(t *testing.T) {
	for _, name := range []string{"followup_task", "collaboration-optimize.followup_task", "collaboration-optimize__followup_task"} {
		payload := map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{
			map[string]any{"type": "function_call", "namespace": officialCollaborationAlias, "name": name, "arguments": `{"namespace":"collaboration-optimize","message":"literal"}`},
			map[string]any{"type": "function_call_output", "output": map[string]any{"type": "function_call", "namespace": officialCollaborationAlias, "name": "literal"}},
		}}}
		raw, _ := json.Marshal(payload)
		got := RestoreOfficialCollaborationResponse(raw)
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatal(err)
		}
		items := decoded["response"].(map[string]any)["output"].([]any)
		call := items[0].(map[string]any)
		if call["namespace"] != "collaboration" || call["arguments"] != `{"namespace":"collaboration-optimize","message":"literal"}` {
			t.Fatalf("bad call: %s", got)
		}
		output := items[1].(map[string]any)["output"].(map[string]any)
		if output["namespace"] != officialCollaborationAlias {
			t.Fatal("rewrote tool output data")
		}
	}
	text := []byte(`{"type":"response.output_text.delta","delta":"collaboration-optimize"}`)
	if !bytes.Equal(RestoreOfficialCollaborationResponse(text), text) {
		t.Fatal("rewrote ordinary text")
	}
}
