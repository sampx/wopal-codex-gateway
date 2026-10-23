package compaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"wopal-codex-gateway/internal/routing"
)

type ConversationState struct {
	LastRequest        []byte
	LastResponseOutput []byte
	LastResponseID     string
	ActiveCompactionID string
}

type PreparedTurn struct {
	Body             []byte
	NextRequest      []byte
	NextCompactionID string
}

type mergeItem struct {
	raw      json.RawMessage
	itemType string
	id       string
	callID   string
}

func PrepareTurn(message map[string]any, model string, state ConversationState) (PreparedTurn, error) {
	requestType, _ := message["type"].(string)
	requestType = strings.TrimSpace(requestType)
	if requestType != "response.create" && requestType != "response.append" {
		return PreparedTurn{}, fmt.Errorf("unsupported websocket request type: %s", requestType)
	}
	inheritRequestFields(message, state.LastRequest, model)
	input, ok := message["input"].([]any)
	if !ok {
		return PreparedTurn{}, fmt.Errorf("websocket request requires array field: input")
	}
	replay, err := ExpandReplayInput(input)
	if err != nil {
		return PreparedTurn{}, err
	}
	if replay.Found {
		if len(state.LastRequest) > 0 && state.ActiveCompactionID == replay.ID {
			mergedInput, errMerge := mergeTranscript(state.LastRequest, state.LastResponseOutput, replay.Tail)
			if errMerge != nil {
				return PreparedTurn{}, errMerge
			}
			normalized := routing.CloneMessage(message)
			delete(normalized, "type")
			delete(normalized, "generate")
			delete(normalized, "previous_response_id")
			inheritRequestFields(normalized, state.LastRequest, model)
			normalized["stream"] = true
			normalized["input"] = mergedInput
			body, errMarshal := json.Marshal(normalized)
			if errMarshal != nil {
				return PreparedTurn{}, errMarshal
			}
			return PreparedTurn{Body: body, NextRequest: bytes.Clone(body), NextCompactionID: replay.ID}, nil
		}

		normalized := routing.CloneMessage(message)
		delete(normalized, "type")
		delete(normalized, "generate")
		delete(normalized, "previous_response_id")
		inheritRequestFields(normalized, state.LastRequest, model)
		normalized["stream"] = true
		normalized["input"] = replay.Expanded
		body, errMarshal := json.Marshal(normalized)
		if errMarshal != nil {
			return PreparedTurn{}, errMarshal
		}
		return PreparedTurn{Body: body, NextRequest: bytes.Clone(body), NextCompactionID: replay.ID}, nil
	}

	if len(state.LastRequest) == 0 {
		if routing.PreviousResponseID(message) != "" {
			return PreparedTurn{}, fmt.Errorf("previous_response_not_found")
		}
		normalized := routing.CloneMessage(message)
		delete(normalized, "type")
		delete(normalized, "generate")
		normalized["stream"] = true
		body, err := json.Marshal(normalized)
		if err != nil {
			return PreparedTurn{}, err
		}
		return PreparedTurn{Body: body, NextRequest: bytes.Clone(body)}, nil
	}

	if shouldReplaceTranscript(message, input) {
		normalized, err := normalizeReplacement(message, state.LastRequest, model)
		if err != nil {
			return PreparedTurn{}, err
		}
		return PreparedTurn{Body: normalized, NextRequest: bytes.Clone(normalized)}, nil
	}

	mergedInput, err := mergeTranscript(state.LastRequest, state.LastResponseOutput, input)
	if err != nil {
		return PreparedTurn{}, err
	}
	normalized := routing.CloneMessage(message)
	delete(normalized, "type")
	delete(normalized, "generate")
	delete(normalized, "previous_response_id")
	inheritRequestFields(normalized, state.LastRequest, model)
	normalized["stream"] = true
	normalized["input"] = mergedInput
	body, err := json.Marshal(normalized)
	if err != nil {
		return PreparedTurn{}, err
	}
	return PreparedTurn{Body: body, NextRequest: bytes.Clone(body), NextCompactionID: state.ActiveCompactionID}, nil
}
func normalizeReplacement(message map[string]any, lastRequest []byte, model string) ([]byte, error) {
	normalized := routing.CloneMessage(message)
	delete(normalized, "type")
	delete(normalized, "generate")
	delete(normalized, "previous_response_id")
	inheritRequestFields(normalized, lastRequest, model)
	normalized["stream"] = true
	return json.Marshal(normalized)
}

func inheritRequestFields(message map[string]any, lastRequest []byte, model string) {
	var previous map[string]any
	_ = json.Unmarshal(lastRequest, &previous)
	if strings.TrimSpace(routing.StringOrEmpty(message["model"])) == "" {
		if prior := strings.TrimSpace(routing.StringOrEmpty(previous["model"])); prior != "" {
			message["model"] = prior
		} else {
			message["model"] = model
		}
	}
	if _, exists := message["instructions"]; !exists {
		if instructions, ok := previous["instructions"]; ok {
			message["instructions"] = instructions
		}
	}
}

func shouldReplaceTranscript(message map[string]any, input []any) bool {
	if routing.PreviousResponseID(message) != "" {
		return false
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(fmt.Sprint(item["type"]))
		switch typeName {
		case "compaction", "compaction_summary", "function_call", "custom_tool_call":
			return true
		case "message":
			if strings.TrimSpace(fmt.Sprint(item["role"])) == "assistant" {
				return true
			}
		}
	}
	return false
}

func mergeTranscript(lastRequest, lastResponseOutput []byte, appendInput []any) (json.RawMessage, error) {
	var previous struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(lastRequest, &previous); err != nil {
		return nil, fmt.Errorf("invalid previous request input: %w", err)
	}
	items := make([]mergeItem, 0, len(previous.Input)+len(appendInput)+8)
	for _, raw := range previous.Input {
		items = append(items, parseMergeItem(raw))
	}
	trimmed := bytes.TrimSpace(lastResponseOutput)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var output []json.RawMessage
		if err := json.Unmarshal(trimmed, &output); err != nil {
			return nil, fmt.Errorf("invalid previous response output: %w", err)
		}
		for _, raw := range output {
			items = append(items, parseMergeItem(raw))
		}
	}
	for _, value := range appendInput {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("invalid request input: %w", err)
		}
		items = append(items, parseMergeItem(raw))
	}
	items = dedupeFunctionCalls(items)
	items = dedupeItemsByID(items)
	return marshalMergeItems(items), nil
}

func marshalMergeItems(items []mergeItem) json.RawMessage {
	length := 2
	if len(items) > 1 {
		length += len(items) - 1
	}
	for _, item := range items {
		length += len(item.raw)
	}
	out := make([]byte, 0, length)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item.raw...)
	}
	out = append(out, ']')
	return json.RawMessage(out)
}

func parseMergeItem(raw json.RawMessage) mergeItem {
	item := mergeItem{raw: bytes.Clone(raw)}
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil {
		return item
	}
	item.itemType = strings.TrimSpace(fmt.Sprint(metadata["type"]))
	item.id = strings.TrimSpace(fmt.Sprint(metadata["id"]))
	item.callID = strings.TrimSpace(fmt.Sprint(metadata["call_id"]))
	if item.id == "<nil>" {
		item.id = ""
	}
	if item.callID == "<nil>" {
		item.callID = ""
	}
	return item
}

func isToolCallType(value string) bool {
	return value == "function_call" || value == "custom_tool_call"
}

func isToolOutputType(value string) bool {
	return value == "function_call_output" || value == "custom_tool_call_output"
}

func dedupeFunctionCalls(items []mergeItem) []mergeItem {
	seen := make(map[string]struct{}, len(items))
	filtered := items[:0]
	for _, item := range items {
		if isToolCallType(item.itemType) && item.callID != "" {
			if _, exists := seen[item.callID]; exists {
				continue
			}
			seen[item.callID] = struct{}{}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func dedupeItemsByID(items []mergeItem) []mergeItem {
	referenced := make(map[string]struct{}, len(items))
	for _, item := range items {
		if isToolOutputType(item.itemType) && item.callID != "" {
			referenced[item.callID] = struct{}{}
		}
	}
	keep := make(map[string]int, len(items))
	keepReferenced := make(map[string]bool, len(items))
	for index, item := range items {
		if item.id == "" {
			continue
		}
		_, isReferenced := referenced[item.callID]
		isReferenced = isReferenced && item.callID != ""
		if _, exists := keep[item.id]; !exists || isReferenced || !keepReferenced[item.id] {
			keep[item.id] = index
			keepReferenced[item.id] = isReferenced
		}
	}
	filtered := items[:0]
	for index, item := range items {
		if item.id != "" && keep[item.id] != index {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}
