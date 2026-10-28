package compaction

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The official backend reserves collaboration's native schemas (including
// message.encrypted). Use the SDK's compatibility namespace when removing that
// property, then restore tool identities on the way back to Codex.
const officialCollaborationAlias = "collaboration-optimize"

func collaborationToolLists(payload map[string]any) [][]any {
	var lists [][]any
	if tools, ok := payload["tools"].([]any); ok {
		lists = append(lists, tools)
	}
	input, _ := payload["input"].([]any)
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && itemTypeOf(item) == "additional_tools" {
			if tools, ok := item["tools"].([]any); ok {
				lists = append(lists, tools)
			}
		}
	}
	return lists
}

// OfficialCollaborationRewriteRequired also recognizes historical definitions
// whose encrypted flag was already removed. A client-owned compatibility
// namespace is left untouched, including its responses.
func OfficialCollaborationRewriteRequired(payload map[string]any) bool {
	if OfficialCollaborationNamespaceConflict(payload) {
		return false
	}
	found := false
	for _, tools := range collaborationToolLists(payload) {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || itemTypeOf(tool) != "namespace" {
				continue
			}
			if tool["name"] == officialCollaborationNamespace {
				found = true
			}
		}
	}
	input, _ := payload["input"].([]any)
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && collaborationCallNeedsRename(item, officialCollaborationNamespace) {
			found = true
		}
	}
	return found
}

// Match the SDK's entire reserved alias surface, including flattened function
// names and names nested inside an unrelated namespace.
func OfficialCollaborationNamespaceConflict(payload map[string]any) bool {
	for _, tools := range collaborationToolLists(payload) {
		if collaborationToolsHaveAliasConflict(tools) {
			return true
		}
	}
	return false
}

func collaborationToolsHaveAliasConflict(tools []any) bool {
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		name = strings.TrimSpace(name)
		if name == officialCollaborationAlias || strings.HasPrefix(name, officialCollaborationAlias+".") || strings.HasPrefix(name, officialCollaborationAlias+"__") {
			return true
		}
		if itemTypeOf(tool) == "namespace" {
			inner, _ := tool["tools"].([]any)
			if collaborationToolsHaveAliasConflict(inner) {
				return true
			}
		}
	}
	return false
}

func collaborationCallNeedsRename(item map[string]any, from string) bool {
	if typ := itemTypeOf(item); typ != "function_call" && typ != "custom_tool_call" && typ != "function" {
		return false
	}
	name, _ := item["name"].(string)
	return item["namespace"] == from || strings.HasPrefix(name, from+".") || strings.HasPrefix(name, from+"__")
}

func renameCollaborationCall(item map[string]any, from, to string) bool {
	if !collaborationCallNeedsRename(item, from) {
		return false
	}
	if item["namespace"] == from {
		item["namespace"] = to
	}
	if name, ok := item["name"].(string); ok {
		for _, separator := range []string{".", "__"} {
			if strings.HasPrefix(name, from+separator) {
				item["name"] = to + strings.TrimPrefix(name, from)
				break
			}
		}
	}
	return true
}

func prepareOfficialCollaboration(payload map[string]any) (int, int) {
	if !OfficialCollaborationRewriteRequired(payload) {
		return 0, 0
	}
	cleared := clearCodexCollaborationEncryption(payload)
	renamed := 0
	for _, tools := range collaborationToolLists(payload) {
		for _, raw := range tools {
			if tool, ok := raw.(map[string]any); ok && itemTypeOf(tool) == "namespace" && tool["name"] == officialCollaborationNamespace {
				tool["name"] = officialCollaborationAlias
				renamed++
			}
		}
	}
	input, _ := payload["input"].([]any)
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && renameCollaborationCall(item, officialCollaborationNamespace, officialCollaborationAlias) {
			renamed++
		}
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok && renameCollaborationCall(choice, officialCollaborationNamespace, officialCollaborationAlias) {
		renamed++
	}
	return cleared, renamed
}

// RestoreOfficialCollaborationResponse changes protocol identities only; text,
// arguments, tool inputs and tool outputs are opaque and must remain verbatim.
func RestoreOfficialCollaborationResponse(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(officialCollaborationAlias)) {
		return raw
	}
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || !restoreOfficialCollaborationValue(payload) {
		return raw
	}
	restored, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return restored
}

func restoreOfficialCollaborationValue(value any) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if restoreOfficialCollaborationValue(child) {
				changed = true
			}
		}
	case map[string]any:
		name, _ := typed["name"].(string)
		changed = renameCollaborationCall(typed, officialCollaborationAlias, officialCollaborationNamespace)
		if changed && strings.HasPrefix(name, officialCollaborationAlias+".") {
			typed["namespace"] = officialCollaborationNamespace
			typed["name"] = strings.TrimPrefix(name, officialCollaborationAlias+".")
		}
		for key, child := range typed {
			if key == "arguments" || key == "input" || (key == "output" && (itemTypeOf(typed) == "function_call_output" || itemTypeOf(typed) == "custom_tool_call_output")) {
				continue
			}
			if restoreOfficialCollaborationValue(child) {
				changed = true
			}
		}
	}
	return changed
}
