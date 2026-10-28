package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/config"
)

// codexMultiAgentUserAgent is the identity Codex sends. The SDK only applies its
// multi-agent compatibility rewrites to an official Codex client, so a test that
// omits this header would exercise the path a browser takes instead.
const codexMultiAgentUserAgent = "Codex Desktop/0.164.0-alpha.11.1 (Mac OS 26.6.2; arm64) unknown (Codex Desktop; 26.1021.31345)"

var codexMultiAgentUserAgents = []string{
	codexMultiAgentUserAgent,
	"codex_cli_rs/0.164.0-alpha.11.1 (Mac OS 26.6.2; arm64)",
}

const subAgentChatCompletion = "{\"id\":\"chatcmpl-subagent\",\"object\":\"chat.completion\",\"created\":1,\"model\":\"subagent-model\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}"

// subAgentChatCompletionStream is the streaming shape the gateway asks for on the
// loopback hop a websocket turn replays to.
const subAgentChatCompletionStream = "data: {\"id\":\"chatcmpl-subagent\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"subagent-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-subagent\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"subagent-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-subagent\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"subagent-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// replyAsChatCompletion answers in the shape the caller asked for. A websocket
// turn replays over HTTP with a streaming accept header, so returning JSON there
// would fail the turn before the assertion ever runs.
func replyAsChatCompletion(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(subAgentChatCompletionStream))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(subAgentChatCompletion))
}

type capturedRequest struct {
	Path string
	Body map[string]any
}

// TestWopalSubAgentTaskReachesChatCompletions is the regression for a delegated
// task that never arrived. A Codex parent passes the task inside an
// agent_message whose body sits in encrypted_content, and marks the
// collaboration tools message argument as encrypted. The chat-completions
// translator understands neither shape, so without the gateway compatibility
// switch the upstream receives the conversation with the task silently removed.
func TestWopalSubAgentTaskReachesChatCompletions(t *testing.T) {
	const taskBody = "Summarize the repository layout and report the packages you found."

	root := t.TempDir()
	var mu sync.Mutex
	var captured []capturedRequest

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, capturedRequest{Path: r.URL.Path, Body: body})
		mu.Unlock()
		replyAsChatCompletion(w, r)
	}))
	defer upstream.Close()

	gateway := config.DefaultGatewayConfig()
	gateway.Version = 1
	gateway.Listen.Port = freePort(t)
	gateway.WopalAI.BaseURL = upstream.URL + "/v1"
	gateway.WopalAI.Models = []config.ExternalModelConfig{{Name: "subagent-model", Protocol: config.ProtocolOpenAIChat}}
	configPath := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gatewayData, err := json.MarshalIndent(gateway, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, gatewayData, 0o600); err != nil {
		t.Fatal(err)
	}

	startTestGateway(t, root, configPath, "e2e-subagent", gateway.Listen.Port)
	base := "http://127.0.0.1:" + strconv.Itoa(gateway.Listen.Port)

	for index, userAgent := range codexMultiAgentUserAgents {
		t.Run(strings.Split(userAgent, "/")[0], func(t *testing.T) {
			payload := map[string]any{
				"model":  "wopal-ai/subagent-model",
				"stream": false,
				"input": []any{
					map[string]any{
						"type": "message", "role": "user",
						"content": []any{map[string]any{"type": "input_text", "text": "parent context"}},
					},
					map[string]any{
						"type": "agent_message", "author": "/root", "recipient": "/root/child",
						"content": []any{
							map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
							map[string]any{"type": "encrypted_content", "encrypted_content": taskBody},
						},
					},
					map[string]any{
						"type": "additional_tools",
						"tools": []any{
							map[string]any{
								"type": "namespace", "name": "collaboration",
								"tools": []any{
									collaborationMessageTool("spawn_agent"),
									collaborationMessageTool("send_message"),
									collaborationMessageTool("followup_task"),
								},
							},
						},
					},
				},
			}

			status, body := postJSONWithHeaders(t, base+"/v1/responses", payload, map[string]string{
				"User-Agent": userAgent,
			})
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(captured) != index+1 {
				t.Fatalf("upstream requests=%d, want %d; the assertion would be vacuous", len(captured), index+1)
			}
			chat := captured[index]
			if !strings.HasSuffix(chat.Path, "/chat/completions") {
				t.Fatalf("upstream path = %q, want a chat completions request", chat.Path)
			}

			messages, _ := chat.Body["messages"].([]any)
			if !messagesContainText(messages, taskBody) {
				t.Fatalf("delegated task never reached chat completions for %q; messages=%v", userAgent, messages)
			}

			// The compatibility rewrite must keep every collaboration tool reachable.
			// Chat completions has no namespaces, so the translator flattens collaboration
			// into collaboration__<tool>; the rewrite must not drop or rename the tool
			// beyond that, or the parent would call something Codex cannot route back.
			names := collaborationToolNames(t, chat.Body)
			for _, want := range []string{"spawn_agent", "send_message", "followup_task"} {
				if !names[want] && !names["collaboration__"+want] {
					t.Errorf("collaboration tool %q disappeared from the upstream schema; names=%v", want, names)
				}
			}
			for _, tool := range collaborationTools(t, chat.Body) {
				if hasEncryptedMessageFlag(tool) {
					t.Errorf("tool %v still marks message.encrypted; an official model would emit ciphertext for the task", tool["name"])
				}
			}
		})
	}
}

func collaborationToolNames(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, tool := range collaborationTools(t, body) {
		name := stringOrEmpty(tool["name"])
		if name == "" {
			// A chat completions tool wraps its name in a function object.
			fn, _ := tool["function"].(map[string]any)
			name = stringOrEmpty(fn["name"])
		}
		if name != "" {
			names[name] = true
		}
	}
	return names
}

func collaborationMessageTool(name string) map[string]any {
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

func collaborationTools(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	var visit func(tools any)
	visit = func(tools any) {
		list, _ := tools.([]any)
		for _, value := range list {
			tool, _ := value.(map[string]any)
			if tool == nil {
				continue
			}
			if nested, ok := tool["tools"]; ok {
				visit(nested)
				continue
			}
			// Chat completions wraps each tool in a function object.
			if stringOrEmpty(tool["name"]) != "" || tool["function"] != nil {
				out = append(out, tool)
			}
		}
	}
	visit(body["tools"])
	for _, value := range asList(body["input"]) {
		item, _ := value.(map[string]any)
		if item != nil && stringOrEmpty(item["type"]) == "additional_tools" {
			visit(item["tools"])
		}
	}
	return out
}

func hasEncryptedMessageFlag(tool map[string]any) bool {
	// Chat completions nests the schema under a function object.
	if fn, ok := tool["function"].(map[string]any); ok {
		tool = fn
	}
	parameters, _ := tool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	message, _ := properties["message"].(map[string]any)
	_, ok := message["encrypted"]
	return ok
}

func messagesContainText(messages []any, want string) bool {
	for _, value := range messages {
		encoded, err := json.Marshal(value)
		if err == nil && strings.Contains(string(encoded), want) {
			return true
		}
	}
	return false
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

func stringOrEmpty(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func postJSONWithHeaders(t *testing.T, url string, payload map[string]any, headers map[string]string) (int, string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestWopalSubAgentTaskReachesChatCompletionsOverWebsocket proves the same fix
// holds on the websocket path. Codex opens the sub-agent turn over websocket and
// the gateway replays it to its own HTTP endpoint, so the client identity has to
// survive that loopback hop; if it did not, the SDK would treat the request as a
// browser and skip the rewrites again.
func TestWopalSubAgentTaskReachesChatCompletionsOverWebsocket(t *testing.T) {
	const taskBody = "Count the test files and report the total."

	root := t.TempDir()
	var mu sync.Mutex
	var captured []capturedRequest

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, capturedRequest{Path: r.URL.Path, Body: body})
		mu.Unlock()
		replyAsChatCompletion(w, r)
	}))
	defer upstream.Close()

	gateway := config.DefaultGatewayConfig()
	gateway.Version = 1
	gateway.Listen.Port = freePort(t)
	gateway.WopalAI.BaseURL = upstream.URL + "/v1"
	gateway.WopalAI.Models = []config.ExternalModelConfig{{Name: "subagent-model", Protocol: config.ProtocolOpenAIChat}}
	configPath := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gatewayData, err := json.MarshalIndent(gateway, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, gatewayData, 0o600); err != nil {
		t.Fatal(err)
	}

	startTestGateway(t, root, configPath, "e2e-subagent-ws", gateway.Listen.Port)
	base := "http://127.0.0.1:" + strconv.Itoa(gateway.Listen.Port)

	for index, userAgent := range codexMultiAgentUserAgents {
		t.Run(strings.Split(userAgent, "/")[0], func(t *testing.T) {
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/v1/responses", http.Header{
				"Authorization":        {"Bearer " + config.LocalAccessKey},
				"X-Codex-Routing-Hint": {"model=wopal-ai/subagent-model"},
				"User-Agent":           {userAgent},
			})
			if err != nil {
				t.Fatalf("dial websocket: %v", err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			if err := conn.WriteJSON(map[string]any{
				"type": "response.create", "model": "wopal-ai/subagent-model", "stream": true,
				"input": []any{
					map[string]any{
						"type": "agent_message", "author": "/root", "recipient": "/root/child",
						"content": []any{
							map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"},
							map[string]any{"type": "encrypted_content", "encrypted_content": taskBody},
						},
					},
					map[string]any{
						"type": "additional_tools",
						"tools": []any{
							map[string]any{
								"type": "namespace", "name": "collaboration",
								"tools": []any{collaborationMessageTool("spawn_agent")},
							},
						},
					},
				},
			}); err != nil {
				t.Fatal(err)
			}
			for {
				_, raw, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Fatalf("read: %v", errRead)
				}
				var event map[string]any
				if json.Unmarshal(raw, &event) != nil {
					continue
				}
				if event["type"] == "error" {
					t.Fatalf("websocket error: %s", raw)
				}
				if event["type"] == "response.completed" {
					break
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(captured) != index+1 {
				t.Fatalf("upstream requests=%d, want %d over the websocket path", len(captured), index+1)
			}
			messages, _ := captured[index].Body["messages"].([]any)
			if !messagesContainText(messages, taskBody) {
				t.Fatalf("delegated task never reached chat completions over websocket for %q; messages=%v", userAgent, messages)
			}
		})
	}
}
