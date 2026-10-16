package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type wopalConversationState struct {
	lastRequest        []byte
	lastResponseOutput []byte
	lastResponseID     string
	activeCompactionID string
}

type wopalPreparedTurn struct {
	body             []byte
	nextRequest      []byte
	nextCompactionID string
}

type wopalMergeItem struct {
	raw      json.RawMessage
	itemType string
	id       string
	callID   string
}

func prepareWopalWebsocketTurn(message map[string]any, model string, state wopalConversationState) (wopalPreparedTurn, error) {
	requestType, _ := message["type"].(string)
	requestType = strings.TrimSpace(requestType)
	if requestType != "response.create" && requestType != "response.append" {
		return wopalPreparedTurn{}, fmt.Errorf("unsupported websocket request type: %s", requestType)
	}
	inheritWopalRequestFields(message, state.lastRequest, model)
	input, ok := message["input"].([]any)
	if !ok {
		return wopalPreparedTurn{}, fmt.Errorf("websocket request requires array field: input")
	}
	replay, err := expandWopalCompactionReplayInput(input)
	if err != nil {
		return wopalPreparedTurn{}, err
	}
	if replay.Found {
		if len(state.lastRequest) > 0 && state.activeCompactionID == replay.ID {
			mergedInput, errMerge := mergeWopalTranscript(state.lastRequest, state.lastResponseOutput, replay.Tail)
			if errMerge != nil {
				return wopalPreparedTurn{}, errMerge
			}
			normalized := cloneMessage(message)
			delete(normalized, "type")
			delete(normalized, "generate")
			delete(normalized, "previous_response_id")
			inheritWopalRequestFields(normalized, state.lastRequest, model)
			normalized["stream"] = true
			normalized["input"] = mergedInput
			body, errMarshal := json.Marshal(normalized)
			if errMarshal != nil {
				return wopalPreparedTurn{}, errMarshal
			}
			return wopalPreparedTurn{body: body, nextRequest: bytes.Clone(body), nextCompactionID: replay.ID}, nil
		}

		normalized := cloneMessage(message)
		delete(normalized, "type")
		delete(normalized, "generate")
		delete(normalized, "previous_response_id")
		inheritWopalRequestFields(normalized, state.lastRequest, model)
		normalized["stream"] = true
		normalized["input"] = replay.Expanded
		body, errMarshal := json.Marshal(normalized)
		if errMarshal != nil {
			return wopalPreparedTurn{}, errMarshal
		}
		return wopalPreparedTurn{body: body, nextRequest: bytes.Clone(body), nextCompactionID: replay.ID}, nil
	}

	if len(state.lastRequest) == 0 {
		if previousResponseID(message) != "" {
			return wopalPreparedTurn{}, fmt.Errorf("previous_response_not_found")
		}
		normalized := cloneMessage(message)
		delete(normalized, "type")
		delete(normalized, "generate")
		normalized["stream"] = true
		body, err := json.Marshal(normalized)
		if err != nil {
			return wopalPreparedTurn{}, err
		}
		return wopalPreparedTurn{body: body, nextRequest: bytes.Clone(body)}, nil
	}

	if shouldReplaceWopalTranscript(message, input) {
		normalized, err := normalizeWopalReplacement(message, state.lastRequest, model)
		if err != nil {
			return wopalPreparedTurn{}, err
		}
		return wopalPreparedTurn{body: normalized, nextRequest: bytes.Clone(normalized)}, nil
	}

	mergedInput, err := mergeWopalTranscript(state.lastRequest, state.lastResponseOutput, input)
	if err != nil {
		return wopalPreparedTurn{}, err
	}
	normalized := cloneMessage(message)
	delete(normalized, "type")
	delete(normalized, "generate")
	delete(normalized, "previous_response_id")
	inheritWopalRequestFields(normalized, state.lastRequest, model)
	normalized["stream"] = true
	normalized["input"] = mergedInput
	body, err := json.Marshal(normalized)
	if err != nil {
		return wopalPreparedTurn{}, err
	}
	return wopalPreparedTurn{body: body, nextRequest: bytes.Clone(body), nextCompactionID: state.activeCompactionID}, nil
}

func previousResponseID(message map[string]any) string {
	value, _ := message["previous_response_id"].(string)
	return strings.TrimSpace(value)
}

func cloneMessage(message map[string]any) map[string]any {
	cloned := make(map[string]any, len(message))
	for key, value := range message {
		cloned[key] = value
	}
	return cloned
}

func normalizeWopalReplacement(message map[string]any, lastRequest []byte, model string) ([]byte, error) {
	normalized := cloneMessage(message)
	delete(normalized, "type")
	delete(normalized, "generate")
	delete(normalized, "previous_response_id")
	inheritWopalRequestFields(normalized, lastRequest, model)
	normalized["stream"] = true
	return json.Marshal(normalized)
}

func inheritWopalRequestFields(message map[string]any, lastRequest []byte, model string) {
	var previous map[string]any
	_ = json.Unmarshal(lastRequest, &previous)
	if strings.TrimSpace(stringOrEmpty(message["model"])) == "" {
		if prior := strings.TrimSpace(stringOrEmpty(previous["model"])); prior != "" {
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

func shouldReplaceWopalTranscript(message map[string]any, input []any) bool {
	if previousResponseID(message) != "" {
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

func mergeWopalTranscript(lastRequest, lastResponseOutput []byte, appendInput []any) (json.RawMessage, error) {
	var previous struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(lastRequest, &previous); err != nil {
		return nil, fmt.Errorf("invalid previous request input: %w", err)
	}
	items := make([]wopalMergeItem, 0, len(previous.Input)+len(appendInput)+8)
	for _, raw := range previous.Input {
		items = append(items, parseWopalMergeItem(raw))
	}
	trimmed := bytes.TrimSpace(lastResponseOutput)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var output []json.RawMessage
		if err := json.Unmarshal(trimmed, &output); err != nil {
			return nil, fmt.Errorf("invalid previous response output: %w", err)
		}
		for _, raw := range output {
			items = append(items, parseWopalMergeItem(raw))
		}
	}
	for _, value := range appendInput {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("invalid request input: %w", err)
		}
		items = append(items, parseWopalMergeItem(raw))
	}
	items = dedupeWopalFunctionCalls(items)
	items = dedupeWopalItemsByID(items)
	return marshalWopalMergeItems(items), nil
}

func marshalWopalMergeItems(items []wopalMergeItem) json.RawMessage {
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

func parseWopalMergeItem(raw json.RawMessage) wopalMergeItem {
	item := wopalMergeItem{raw: bytes.Clone(raw)}
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

func isWopalToolCallType(value string) bool {
	return value == "function_call" || value == "custom_tool_call"
}

func isWopalToolOutputType(value string) bool {
	return value == "function_call_output" || value == "custom_tool_call_output"
}

func dedupeWopalFunctionCalls(items []wopalMergeItem) []wopalMergeItem {
	seen := make(map[string]struct{}, len(items))
	filtered := items[:0]
	for _, item := range items {
		if isWopalToolCallType(item.itemType) && item.callID != "" {
			if _, exists := seen[item.callID]; exists {
				continue
			}
			seen[item.callID] = struct{}{}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func dedupeWopalItemsByID(items []wopalMergeItem) []wopalMergeItem {
	referenced := make(map[string]struct{}, len(items))
	for _, item := range items {
		if isWopalToolOutputType(item.itemType) && item.callID != "" {
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

func responseOutputFromRawEvent(data []byte, collected []json.RawMessage) ([]byte, string) {
	var envelope struct {
		Response struct {
			ID     string          `json:"id"`
			Output json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		responseID := strings.TrimSpace(envelope.Response.ID)
		trimmed := bytes.TrimSpace(envelope.Response.Output)
		if len(trimmed) > 2 && trimmed[0] == '[' {
			return bytes.Clone(trimmed), responseID
		}
		if len(collected) == 0 {
			return []byte("[]"), responseID
		}
		return marshalRawMessages(collected), responseID
	}
	return []byte("[]"), ""
}

func rawOutputItemFromEvent(data []byte) json.RawMessage {
	var envelope struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	trimmed := bytes.TrimSpace(envelope.Item)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return bytes.Clone(trimmed)
}

func marshalRawMessages(items []json.RawMessage) []byte {
	length := 2
	if len(items) > 1 {
		length += len(items) - 1
	}
	for _, item := range items {
		length += len(item)
	}
	out := make([]byte, 0, length)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	out = append(out, ']')
	return out
}
