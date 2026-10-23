package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	wopalCompactionCapsulePrefix   = "wgcp1."
	wopalCompactionMaxOutput       = 12000
	wopalCompactionMinSummaryBytes = 8
	dedicatedWopalCompactionModel  = "wopal-ai/deepseek-v4.1-flash"
)

const wopalCompactionPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

For coding and agent work, preserve concrete details that can affect continuation: exact paths, names, IDs, and config values when relevant; changes already made; commands or tests and their outcomes; important tool-call results; current state; unresolved issues; and prior authorizations that remain relevant. Do not invent facts. Do not call tools. Output only the continuation summary. Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

var (
	wopalCompactionKeyMu sync.RWMutex
	wopalCompactionKey   = sha256.Sum256([]byte("wopal-codex-gateway/compaction/test-default/v1"))
)

type wopalCompactionCapsule struct {
	Version int               `json:"v"`
	ID      string            `json:"id"`
	Model   string            `json:"model,omitempty"`
	Summary string            `json:"summary"`
	Prefix  []json.RawMessage `json:"prefix,omitempty"`
}

type wopalCompactionReplay struct {
	Found    bool
	ID       string
	Expanded []any
	Tail     []any
}

func configureWopalCompactionKey(secret string) {
	derived := sha256.Sum256([]byte("wopal-codex-gateway/compaction/v1\x00" + secret))
	wopalCompactionKeyMu.Lock()
	wopalCompactionKey = derived
	wopalCompactionKeyMu.Unlock()
}

func currentWopalCompactionKey() [32]byte {
	wopalCompactionKeyMu.RLock()
	key := wopalCompactionKey
	wopalCompactionKeyMu.RUnlock()
	return key
}

func sealWopalCompactionCapsule(capsule wopalCompactionCapsule) (string, error) {
	if capsule.Version == 0 {
		capsule.Version = 1
	}
	if capsule.Version != 1 || strings.TrimSpace(capsule.ID) == "" || strings.TrimSpace(capsule.Summary) == "" {
		return "", fmt.Errorf("invalid compaction capsule")
	}
	payload, err := json.Marshal(capsule)
	if err != nil {
		return "", err
	}
	key := currentWopalCompactionKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, payload, []byte(wopalCompactionCapsulePrefix))
	blob := make([]byte, 0, len(nonce)+len(sealed))
	blob = append(blob, nonce...)
	blob = append(blob, sealed...)
	return wopalCompactionCapsulePrefix + base64.RawURLEncoding.EncodeToString(blob), nil
}

func openWopalCompactionCapsule(encoded string) (wopalCompactionCapsule, error) {
	if !strings.HasPrefix(encoded, wopalCompactionCapsulePrefix) {
		return wopalCompactionCapsule{}, fmt.Errorf("foreign compaction capsule")
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, wopalCompactionCapsulePrefix))
	if err != nil {
		return wopalCompactionCapsule{}, fmt.Errorf("decode compaction capsule: %w", err)
	}
	key := currentWopalCompactionKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return wopalCompactionCapsule{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return wopalCompactionCapsule{}, err
	}
	if len(blob) < aead.NonceSize() {
		return wopalCompactionCapsule{}, fmt.Errorf("compaction capsule is truncated")
	}
	payload, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], []byte(wopalCompactionCapsulePrefix))
	if err != nil {
		return wopalCompactionCapsule{}, fmt.Errorf("open compaction capsule: %w", err)
	}
	var capsule wopalCompactionCapsule
	if err := json.Unmarshal(payload, &capsule); err != nil {
		return wopalCompactionCapsule{}, fmt.Errorf("parse compaction capsule: %w", err)
	}
	if capsule.Version != 1 || strings.TrimSpace(capsule.ID) == "" || strings.TrimSpace(capsule.Summary) == "" {
		return wopalCompactionCapsule{}, fmt.Errorf("invalid compaction capsule payload")
	}
	return capsule, nil
}

const wopalCompactionSummaryIDPrefix = "msg_wopal_compaction_summary_"

func wopalCompactionSummaryID(id any) string {
	raw := strings.TrimSpace(fmt.Sprint(id))
	candidate := wopalCompactionSummaryIDPrefix + raw
	if len(candidate) <= 64 {
		return candidate
	}
	sum := sha256.Sum256([]byte(raw))
	return wopalCompactionSummaryIDPrefix + fmt.Sprintf("%x", sum[:16])
}

func wopalCompactionSummaryMessage(capsule wopalCompactionCapsule) map[string]any {
	return map[string]any{
		"type": "message",
		"id":   wopalCompactionSummaryID(capsule.ID),
		"role": "developer",
		"content": "The earlier conversation was compacted. Continue from this preserved conversation context:\n\n" +
			strings.TrimSpace(capsule.Summary),
	}
}

func isWopalCompactionRetainedItem(value any) bool {
	item, ok := value.(map[string]any)
	return ok && strings.TrimSpace(fmt.Sprint(item["type"])) == "additional_tools"
}

func wopalCompactionRetainedPrefix(requestRaw []byte) ([]json.RawMessage, error) {
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		return nil, fmt.Errorf("decode compaction source request: %w", err)
	}
	prefix := make([]json.RawMessage, 0, 1)
	for _, raw := range request.Input {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil || strings.TrimSpace(fmt.Sprint(item["type"])) != "additional_tools" {
			continue
		}
		prefix = append(prefix, json.RawMessage(append([]byte(nil), raw...)))
	}
	return prefix, nil
}

func wopalCompactionPrefixValues(prefix []json.RawMessage) ([]any, error) {
	values := make([]any, 0, len(prefix))
	for _, raw := range prefix {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("decode retained compaction prefix: %w", err)
		}
		values = append(values, value)
	}
	return values, nil
}

func wopalCompactionExpandedBase(capsule wopalCompactionCapsule) ([]any, error) {
	base, err := wopalCompactionPrefixValues(capsule.Prefix)
	if err != nil {
		return nil, err
	}
	base = append(base, wopalCompactionSummaryMessage(capsule))
	return base, nil
}

func withoutWopalCompactionRetainedItems(input any) any {
	items, ok := input.([]any)
	if !ok {
		return input
	}
	filtered := make([]any, 0, len(items))
	for _, value := range items {
		if isWopalCompactionRetainedItem(value) {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func expandWopalCompactionReplayInput(input []any) (wopalCompactionReplay, error) {
	replay := wopalCompactionReplay{
		Expanded: make([]any, 0, len(input)),
		Tail:     make([]any, 0, len(input)),
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(item["type"])) != "compaction" {
			replay.Expanded = append(replay.Expanded, raw)
			if replay.Found {
				replay.Tail = append(replay.Tail, raw)
			}
			continue
		}
		if replay.Found {
			return wopalCompactionReplay{}, fmt.Errorf("multiple compaction items are not supported")
		}
		encoded, _ := item["encrypted_content"].(string)
		capsule, err := openWopalCompactionCapsule(strings.TrimSpace(encoded))
		if err != nil {
			return wopalCompactionReplay{}, fmt.Errorf("unsupported_compaction_capsule: %w", err)
		}
		replay.Found = true
		replay.ID = capsule.ID
		base, err := wopalCompactionExpandedBase(capsule)
		if err != nil {
			return wopalCompactionReplay{}, fmt.Errorf("unsupported_compaction_capsule: %w", err)
		}
		// A replay may itself carry an updated additional_tools item before the
		// compaction capsule. Prefer the client's current declaration; otherwise
		// restore the declaration sealed into the capsule.
		hasCurrentTools := false
		for _, existing := range replay.Expanded {
			if isWopalCompactionRetainedItem(existing) {
				hasCurrentTools = true
				break
			}
		}
		if hasCurrentTools && len(capsule.Prefix) > 0 {
			base = base[len(capsule.Prefix):]
		}
		replay.Expanded = append(replay.Expanded, base...)
	}
	return replay, nil
}

func wopalInputHasCompactionTrigger(input []any) bool {
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if ok && strings.TrimSpace(fmt.Sprint(item["type"])) == "compaction_trigger" {
			return true
		}
	}
	return false
}

func prepareWopalCompactionSummaryTurn(message map[string]any, model string, state wopalConversationState) (wopalPreparedTurn, error) {
	input, ok := message["input"].([]any)
	if !ok {
		return wopalPreparedTurn{}, fmt.Errorf("compaction request requires array field: input")
	}
	filtered := make([]any, 0, len(input)+1)
	triggerCount := 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if ok && strings.TrimSpace(fmt.Sprint(item["type"])) == "compaction_trigger" {
			triggerCount++
			continue
		}
		filtered = append(filtered, raw)
	}
	if triggerCount != 1 {
		return wopalPreparedTurn{}, fmt.Errorf("expected exactly one compaction_trigger, got %d", triggerCount)
	}
	if len(state.lastRequest) == 0 && len(filtered) == 0 {
		return wopalPreparedTurn{}, fmt.Errorf("cannot compact an empty conversation")
	}
	filtered = append(filtered, map[string]any{
		"type":    "message",
		"role":    "user",
		"content": wopalCompactionPrompt,
	})
	request := cloneMessage(message)
	request["model"] = model
	request["input"] = filtered
	request["max_output_tokens"] = wopalCompactionMaxOutput
	prepared, err := prepareWopalWebsocketTurn(request, model, state)
	if err != nil {
		return wopalPreparedTurn{}, err
	}
	var body map[string]any
	if err := json.Unmarshal(prepared.body, &body); err != nil {
		return wopalPreparedTurn{}, err
	}
	delete(body, "tools")
	delete(body, "tool_choice")
	delete(body, "parallel_tool_calls")
	delete(body, "text")
	body["input"] = withoutWopalCompactionRetainedItems(body["input"])
	body["max_output_tokens"] = wopalCompactionMaxOutput
	prepared.body, err = json.Marshal(body)
	if err != nil {
		return wopalPreparedTurn{}, err
	}
	return prepared, nil
}

func buildWopalCompactedStateRequest(summaryRequest []byte, capsule wopalCompactionCapsule) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(summaryRequest, &request); err != nil {
		return nil, fmt.Errorf("decode summary request: %w", err)
	}
	base, err := wopalCompactionExpandedBase(capsule)
	if err != nil {
		return nil, err
	}
	request["input"] = base
	delete(request, "max_output_tokens")
	delete(request, "previous_response_id")
	request["stream"] = true
	return json.Marshal(request)
}

func validateWopalCompactionSummary(summary string) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return fmt.Errorf("compaction model returned an empty summary")
	}
	if len(summary) < wopalCompactionMinSummaryBytes {
		return fmt.Errorf("compaction model returned an abnormally short summary")
	}
	return nil
}

func extractWopalCompactionSummary(output []byte, streamedText string) string {
	var items []map[string]any
	if json.Unmarshal(output, &items) == nil {
		parts := make([]string, 0, 4)
		for _, item := range items {
			if strings.TrimSpace(fmt.Sprint(item["type"])) != "message" {
				continue
			}
			switch content := item["content"].(type) {
			case string:
				if text := strings.TrimSpace(content); text != "" {
					parts = append(parts, text)
				}
			case []any:
				for _, rawPart := range content {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					partType := strings.TrimSpace(fmt.Sprint(part["type"]))
					if partType != "output_text" && partType != "input_text" && partType != "text" {
						continue
					}
					if text := strings.TrimSpace(fmt.Sprint(part["text"])); text != "" && text != "<nil>" {
						parts = append(parts, text)
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	return strings.TrimSpace(streamedText)
}
