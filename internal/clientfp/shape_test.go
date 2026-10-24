package clientfp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// codeModeBody mirrors a request captured from Codex 0.164 talking to an
// official model: tools arrive as namespaces inside the input list, and the
// `functions` namespace carries exec and wait.
const codeModeBody = `{
  "model": "gpt-6.1-sol",
  "tool_choice": "auto",
  "stream": true,
  "input": [
    {"type": "additional_tools", "tools": [
      {"type": "namespace", "name": "functions", "tools": [
        {"type": "custom", "name": "exec"}, {"type": "function", "name": "wait"}
      ]},
      {"type": "namespace", "name": "clock", "tools": [{"type": "function", "name": "sleep"}]}
    ]},
    {"type": "message", "role": "user"},
    {"type": "message", "role": "assistant"}
  ]
}`

// directBody mirrors the same release talking to a Wopal model when the catalog
// entry omits tool_mode: direct tools at the top level and no exec at all.
const directBody = `{
  "model": "wopal-ai/deepseek-v4.1-flash",
  "tool_choice": "auto",
  "stream": true,
  "tools": [
    {"type": "function", "name": "exec_command"},
    {"type": "function", "name": "apply_patch"},
    {"type": "function", "name": "view_image"}
  ],
  "input": [{"type": "message", "role": "user"}]
}`

func shapeOf(t *testing.T, raw string) Shape {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return ShapeFromBody(body)
}

func TestShapeDistinguishesCodeModeFromDirectTools(t *testing.T) {
	code := shapeOf(t, codeModeBody)
	if code.ToolSurface != "code_mode" {
		t.Fatalf("code-mode surface = %q, want code_mode", code.ToolSurface)
	}
	if !reflect.DeepEqual(code.Functions, []string{"exec", "wait"}) {
		t.Fatalf("functions = %v, want [exec wait]", code.Functions)
	}
	if code.Model != "gpt-6.1-sol" {
		t.Fatalf("model = %q", code.Model)
	}

	direct := shapeOf(t, directBody)
	if direct.ToolSurface != "direct" {
		t.Fatalf("direct surface = %q, want direct", direct.ToolSurface)
	}
	if len(direct.Functions) != 0 {
		t.Fatalf("direct surface reported functions %v, want none", direct.Functions)
	}
	if direct.ToolCount != 3 {
		t.Fatalf("direct tool count = %d, want 3", direct.ToolCount)
	}

	// The two shapes must not share a digest: that equality is what makes a
	// silent tool-surface change visible in the log.
	if code.ToolDigest == direct.ToolDigest {
		t.Fatal("code-mode and direct tool sets produced the same digest")
	}
}

func TestShapeRecordsInputAndKeys(t *testing.T) {
	code := shapeOf(t, codeModeBody)
	if !reflect.DeepEqual(code.InputTypes, []string{"additional_tools=1", "message=2"}) {
		t.Fatalf("input types = %v", code.InputTypes)
	}
	if !reflect.DeepEqual(code.Keys, []string{"input", "model", "stream", "tool_choice"}) {
		t.Fatalf("keys = %v", code.Keys)
	}
}

func TestShapeToleratesUnexpectedBodies(t *testing.T) {
	if got := ShapeFromBody(nil); got.ToolSurface != "" {
		t.Fatalf("nil body surface = %q, want empty", got.ToolSurface)
	}
	if got := ShapeFromBody(map[string]any{"model": 42}); got.Model != "42" {
		t.Fatalf("non-string model = %q, want 42", got.Model)
	}
	if got := ShapeFromBody(map[string]any{"tools": "not-a-list"}); got.ToolSurface != "none" {
		t.Fatalf("garbage tools surface = %q, want none", got.ToolSurface)
	}
}
