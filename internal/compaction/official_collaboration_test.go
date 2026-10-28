package compaction

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
)

func newLogger(t *testing.T) *activity.Logger {
	t.Helper()
	logger, err := activity.New(t.TempDir(), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)
	return logger
}

func collaborationNamespaceTools() []any {
	return []any{
		map[string]any{
			"type": "namespace", "name": "collaboration",
			"tools": []any{
				map[string]any{
					"type": "function", "name": "spawn_agent",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"message":    map[string]any{"type": "string", "encrypted": true},
							"task_name":  map[string]any{"type": "string"},
							"agent_type": map[string]any{"type": "string"},
						},
						"required": []any{"message"},
					},
				},
				map[string]any{
					"type": "function", "name": "send_message",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"message": map[string]any{"type": "string", "encrypted": true},
						},
					},
				},
				map[string]any{
					"type": "function", "name": "followup_task",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"message": map[string]any{"type": "string", "encrypted": true},
						},
					},
				},
				map[string]any{
					"type": "function", "name": "close_agent",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
}

func syntheticOpenAICiphertext() string {
	// Fernet wire shape used by Responses API encrypted content: version byte,
	// timestamp, IV, one 16-byte ciphertext block and HMAC. The payload is
	// synthetic; only the structural shape matters to the sanitizer.
	raw := make([]byte, 73)
	raw[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(raw)
}

func directCollaborationMessageTool(name string) map[string]any {
	return map[string]any{
		"type": "function", "name": name,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "encrypted": true},
			},
		},
	}
}

func nonCollaborationNamespaceTools() []any {
	return []any{map[string]any{
		"type": "namespace", "name": "functions",
		"tools": []any{directCollaborationMessageTool("send_message")},
	}}
}

func toolMessageEncryption(tool map[string]any) (bool, bool) {
	parameters, _ := tool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	message, ok := properties["message"].(map[string]any)
	if !ok {
		return false, false
	}
	_, present := message["encrypted"]
	return present, true
}

func encryptedFlagOf(t *testing.T, payload map[string]any, namespaceIndex, toolIndex int) (bool, bool) {
	t.Helper()
	tools, _ := payload["tools"].([]any)
	namespace, _ := tools[namespaceIndex].(map[string]any)
	inner, _ := namespace["tools"].([]any)
	tool, _ := inner[toolIndex].(map[string]any)
	parameters, _ := tool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	message, ok := properties["message"].(map[string]any)
	if !ok {
		return false, false
	}
	_, present := message["encrypted"]
	return present, true
}

// The official branch bypasses CLIProxyAPI entirely, so it has to clear the
// encryption flag itself. Left set, the official parent may return the
// delegated task as ciphertext and a third-party sub-agent can only show
// gAAAAAB... instead of the task.
func TestOfficialHTTPRequestClearsCollaborationMessageEncryption(t *testing.T) {
	logger := newLogger(t)

	payload := map[string]any{
		"model": "gpt-6-luna",
		"tools": append(append(collaborationNamespaceTools(), nonCollaborationNamespaceTools()...), directCollaborationMessageTool("followup_task")),
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hello"},
		},
	}
	payload["input"] = append(payload["input"].([]any), map[string]any{
		"type":  "additional_tools",
		"tools": append(append(collaborationNamespaceTools(), nonCollaborationNamespaceTools()...), directCollaborationMessageTool("followup_task")),
	})

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: http.MethodPost,
		Body:   io.NopCloser(bytes.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	RewriteOfficialRequestBody(req, logger)

	var rewritten map[string]any
	if err := json.NewDecoder(req.Body).Decode(&rewritten); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}

	for index := range []int{0, 1, 2} {
		if present, ok := encryptedFlagOf(t, rewritten, 0, index); ok && present {
			t.Errorf("top-level collaboration tool %d still carries message.encrypted", index)
		}
	}
	if present, ok := encryptedFlagOf(t, rewritten, 0, 3); ok && present {
		t.Errorf("close_agent must keep its schema untouched")
	}
	tools, _ := rewritten["tools"].([]any)
	if present, ok := encryptedFlagOf(t, rewritten, 1, 0); !ok || !present {
		t.Errorf("a same-named tool outside the collaboration namespace was rewritten: %v", tools[1])
	}
	direct, _ := tools[2].(map[string]any)
	if present, ok := toolMessageEncryption(direct); !ok || !present {
		t.Errorf("a direct top-level tool outside the collaboration namespace was rewritten: %v", direct)
	}

	// The same tools also arrive inside input[].additional_tools.
	input, _ := rewritten["input"].([]any)
	foundAdditional := false
	for _, value := range input {
		item, _ := value.(map[string]any)
		if item == nil || item["type"] != "additional_tools" {
			continue
		}
		foundAdditional = true
		tools, _ := item["tools"].([]any)
		namespace, _ := tools[0].(map[string]any)
		inner, _ := namespace["tools"].([]any)
		for index := range []int{0, 1, 2} {
			tool, _ := inner[index].(map[string]any)
			parameters, _ := tool["parameters"].(map[string]any)
			properties, _ := parameters["properties"].(map[string]any)
			message, _ := properties["message"].(map[string]any)
			if _, present := message["encrypted"]; present {
				t.Errorf("additional_tools collaboration tool %d still carries message.encrypted", index)
			}
		}
		other, _ := tools[1].(map[string]any)
		otherInner, _ := other["tools"].([]any)
		otherTool, _ := otherInner[0].(map[string]any)
		if present, ok := toolMessageEncryption(otherTool); !ok || !present {
			t.Errorf("additional_tools non-collaboration tool was rewritten: %v", otherTool)
		}
		direct, _ := tools[2].(map[string]any)
		if present, ok := toolMessageEncryption(direct); !ok || !present {
			t.Errorf("additional_tools direct tool was rewritten: %v", direct)
		}
	}
	if !foundAdditional {
		t.Fatal("additional_tools input item disappeared")
	}
}

// A request with nothing to clean must stay byte-identical: the official
// passthrough contract is that untouched payloads keep their exact bytes.
func TestOfficialHTTPRequestLeavesUnrelatedPayloadUntouched(t *testing.T) {
	logger := newLogger(t)
	original := []byte("{\"model\":\"gpt-6-luna\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"hi\"}],\"tools\":[{\"type\":\"function\",\"name\":\"shell\",\"parameters\":{\"type\":\"object\",\"properties\":{\"command\":{\"type\":\"string\"}}}}]}")
	req := &http.Request{
		Method: http.MethodPost,
		Body:   io.NopCloser(bytes.NewReader(original)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	RewriteOfficialRequestBody(req, logger)

	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("unrelated payload was rewritten:\n got=%s\nwant=%s", got, original)
	}
}

// A websocket frame carries the same tools, so the same flag has to be cleared
// there; otherwise an official parent reached over websocket still emits
// ciphertext.
func TestOfficialWebsocketFrameClearsCollaborationMessageEncryption(t *testing.T) {
	logger := newLogger(t)
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "gpt-6-luna",
		"tools": append(append(collaborationNamespaceTools(), nonCollaborationNamespaceTools()...), directCollaborationMessageTool("send_message")),
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, ok := SanitizeWebsocketFrame(frame, logger)
	if !ok {
		t.Fatal("websocket frame was not rewritten")
	}
	var decoded map[string]any
	if err := json.Unmarshal(rewritten, &decoded); err != nil {
		t.Fatal(err)
	}
	for index := range []int{0, 1, 2} {
		if present, ok := encryptedFlagOf(t, decoded, 0, index); ok && present {
			t.Errorf("websocket collaboration tool %d still carries message.encrypted", index)
		}
	}
	tools, _ := decoded["tools"].([]any)
	if present, ok := encryptedFlagOf(t, decoded, 1, 0); !ok || !present {
		t.Errorf("websocket same-named tool outside collaboration was rewritten: %v", tools[1])
	}
	direct, _ := tools[2].(map[string]any)
	if present, ok := toolMessageEncryption(direct); !ok || !present {
		t.Errorf("websocket direct tool outside collaboration was rewritten: %v", direct)
	}
}

// A plaintext agent_message cannot cross the official branch: the upstream
// validates encrypted_content and rejects foreign content.
func TestOfficialNormalizesPlaintextAgentMessage(t *testing.T) {
	logger := newLogger(t)
	const taskBody = "Summarize the repository layout."
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "gpt-6-luna",
		"input": []any{
			map[string]any{
				"type": "agent_message", "author": "/root", "recipient": "/root/child",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
					map[string]any{"type": "encrypted_content", "encrypted_content": taskBody},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, ok := SanitizeWebsocketFrame(frame, logger)
	if !ok {
		t.Fatal("plaintext agent_message was left in place")
	}
	var decoded map[string]any
	if err := json.Unmarshal(rewritten, &decoded); err != nil {
		t.Fatal(err)
	}
	input, _ := decoded["input"].([]any)
	var found bool
	for _, value := range input {
		item, _ := value.(map[string]any)
		if item == nil {
			continue
		}
		if item["type"] == "agent_message" {
			t.Fatalf("agent_message survived the official rewrite: %v", item)
		}
		encoded, _ := json.Marshal(item)
		if bytes.Contains(encoded, []byte(taskBody)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("task body was dropped instead of converted: %v", input)
	}
}

// Genuine OpenAI ciphertext must survive: only the official upstream can read
// it, and rewriting it would discard the payload.
func TestOfficialNormalizesPlaintextAgentMessageWithCiphertextPrefix(t *testing.T) {
	logger := newLogger(t)
	taskBody := base64.RawURLEncoding.EncodeToString(append([]byte{0x80}, make([]byte, 15)...))
	if !bytes.HasPrefix([]byte(taskBody), []byte("gAAAAA")) {
		t.Fatalf("fixture does not exercise the ciphertext prefix: %q", taskBody)
	}
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "gpt-6-luna",
		"input": []any{map[string]any{
			"type": "agent_message", "author": "/root", "recipient": "/root/child",
			"content": []any{
				map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
				map[string]any{"type": "encrypted_content", "encrypted_content": taskBody},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, ok := SanitizeWebsocketFrame(frame, logger)
	if !ok {
		t.Fatal("plaintext beginning with gAAAAA was mistaken for ciphertext")
	}
	var decoded map[string]any
	if err := json.Unmarshal(rewritten, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rewritten, []byte(taskBody)) {
		t.Fatalf("prefix-colliding task body was lost: %s", rewritten)
	}
	for _, value := range decoded["input"].([]any) {
		if item, _ := value.(map[string]any); itemTypeOf(item) == "agent_message" {
			t.Fatalf("prefix-colliding agent_message survived: %v", item)
		}
	}
}

func TestOfficialKeepsGenuineCiphertextAgentMessage(t *testing.T) {
	logger := newLogger(t)
	ciphertext := syntheticOpenAICiphertext()
	frame, err := json.Marshal(map[string]any{
		"type": "response.create", "model": "gpt-6-luna",
		"input": []any{
			map[string]any{
				"type": "agent_message", "author": "/root", "recipient": "/root/child",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
					map[string]any{"type": "encrypted_content", "encrypted_content": ciphertext},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SanitizeWebsocketFrame(frame, logger); ok {
		t.Fatal("a genuine official ciphertext agent_message must be passed through untouched")
	}
}
