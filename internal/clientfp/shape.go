package clientfp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Shape is the part of a request that tells the gateway how Codex talks to it.
// It is deliberately small: field names, tool names and counts, never content.
//
// The fields exist because a Codex release can change any of them without
// breaking a build, and a proxy that only logs failures learns about the change
// after the fact:
//
//   - ToolSurface moves between "direct" and "code-mode". Codex serves the
//     official models with exec under a `functions` namespace delivered as
//     additional_tools, and external models with direct tools such as
//     exec_command at the top level. A model switch inside one thread rebuilds
//     the surface, and history still replaying the previous shape is exactly
//     the class of bug that produces "unsupported custom tool call".
//   - InputTypes shows how history is replayed.
//   - Keys shows which request parameters the release started or stopped
//     sending.
type Shape struct {
	// Model is the requested model, recorded so a surface change can be
	// attributed to a model switch rather than a client upgrade.
	Model string
	// ToolSurface is "code_mode", "direct" or "none".
	ToolSurface string
	// ToolCount is how many tools the request declared in total.
	ToolCount int
	// ToolDigest is a stable hash of the sorted tool names, so the log can show
	// that the set changed without carrying hundreds of names.
	ToolDigest string
	// Functions lists the tools inside the `functions` namespace. It is the
	// shortest signal that distinguishes exec-based from direct tool sets, so it
	// is kept verbatim.
	Functions []string
	// InputTypes counts history item types, e.g. "message=12".
	InputTypes []string
	// Keys lists the top-level request fields.
	Keys []string
}

// ShapeFromBody summarizes a decoded request body. A body that is not an object
// yields an empty Shape; summarization never fails a request.
func ShapeFromBody(body map[string]any) Shape {
	shape := Shape{}
	if body == nil {
		return shape
	}
	shape.Model = stringOr(body["model"])
	shape.Keys = sortedKeys(body)

	var names []string
	topTools := toolsOf(body["tools"])
	for _, tool := range topTools {
		names = append(names, tool.name)
	}
	var namespaced []string
	for _, tool := range additionalTools(body["input"]) {
		namespaced = append(namespaced, tool.name)
	}

	switch {
	case len(namespaced) > 0:
		shape.ToolSurface = "code_mode"
	case len(topTools) > 0:
		shape.ToolSurface = "direct"
	default:
		shape.ToolSurface = "none"
	}

	names = append(names, namespaced...)
	shape.ToolCount = len(names)
	shape.ToolDigest = digest(names)
	shape.Functions = functionsNamespace(body["input"])
	shape.InputTypes = inputTypeCounts(body["input"])
	return shape
}

// Fields returns the shape as log fields.
func (s Shape) Fields() map[string]any {
	fields := map[string]any{
		"request_tool_surface": s.ToolSurface,
		"request_tool_count":   s.ToolCount,
		"request_tool_digest":  s.ToolDigest,
	}
	if s.Model != "" {
		fields["request_model"] = s.Model
	}
	if len(s.Functions) > 0 {
		fields["request_functions"] = s.Functions
	}
	if len(s.InputTypes) > 0 {
		fields["request_input_types"] = s.InputTypes
	}
	if len(s.Keys) > 0 {
		fields["request_keys"] = s.Keys
	}
	return fields
}

type toolEntry struct {
	name string
	raw  map[string]any
}

func toolsOf(raw any) []toolEntry {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	entries := make([]toolEntry, 0, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entries = append(entries, toolEntry{name: stringOr(object["name"]), raw: object})
	}
	return entries
}

// additionalTools returns the namespaces Codex delivers inside the input list
// instead of the top-level tools array.
func additionalTools(input any) []toolEntry {
	list, ok := input.([]any)
	if !ok {
		return nil
	}
	var entries []toolEntry
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok || stringOr(object["type"]) != "additional_tools" {
			continue
		}
		entries = append(entries, toolsOf(object["tools"])...)
	}
	return entries
}

// functionsNamespace lists the tools inside the `functions` namespace. This is
// where code-mode exposes exec and wait.
func functionsNamespace(input any) []string {
	list, ok := input.([]any)
	if !ok {
		return nil
	}
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok || stringOr(object["type"]) != "additional_tools" {
			continue
		}
		for _, tool := range toolsOf(object["tools"]) {
			entry := tool.raw
			if stringOr(entry["type"]) != "namespace" || stringOr(entry["name"]) != "functions" {
				continue
			}
			var names []string
			for _, inner := range toolsOf(entry["tools"]) {
				if inner.name != "" {
					names = append(names, inner.name)
				}
			}
			sort.Strings(names)
			return names
		}
	}
	return nil
}

func inputTypeCounts(input any) []string {
	list, ok := input.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	counts := make(map[string]int, 8)
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := stringOr(object["type"])
		if name == "" {
			name = "unknown"
		}
		counts[name]++
	}
	types := make([]string, 0, len(counts))
	for name := range counts {
		types = append(types, name)
	}
	sort.Strings(types)
	out := make([]string, 0, len(types))
	for _, name := range types {
		out = append(out, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return out
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func digest(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:8])
}

func stringOr(raw any) string {
	switch value := raw.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}
