package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"wopal-codex-gateway/internal/config"
)

type externalUpstreamObservation struct {
	Path      string
	RawQuery  string
	Auth      string
	GoogKey   string
	Cookie    string
	AccountID string
	Session   string
	Static    string
}

func TestExternalExecutorsReachProtocolSpecificEndpoints(t *testing.T) {
	var mu sync.Mutex
	var observations []externalUpstreamObservation
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		observations = append(observations, externalUpstreamObservation{
			Path:      r.URL.Path,
			RawQuery:  r.URL.RawQuery,
			Auth:      r.Header.Get("Authorization"),
			GoogKey:   r.Header.Get("x-goog-api-key"),
			Cookie:    r.Header.Get("Cookie"),
			AccountID: r.Header.Get("ChatGPT-Account-ID"),
			Session:   r.Header.Get("x-provider-session"),
			Static:    r.Header.Get("x-provider-static"),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/openai/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case "/openai/responses":
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if stream, _ := payload["stream"].(bool); stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-test\",\"model\":\"responses-model\"}}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\",\"status\":\"completed\",\"model\":\"responses-model\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp-test","object":"response","status":"completed","model":"responses-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		case "/anthropic/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"messages-model\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case "/google/v1beta/models/gemini-model:generateContent":
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"unexpected path %s"}}`, r.URL.Path)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	gateway := config.DefaultGatewayConfig()
	gateway.WopalAI.BaseURL = upstream.URL + "/wopal"
	gateway.WopalAI.Models = nil
	gateway.Listen.Port = freePort(t)
	gateway.Providers["multi"] = config.ProviderConfig{
		BaseURL: upstream.URL + "/openai",
		BaseURLs: map[string]string{
			config.ProtocolAnthropicMessages: upstream.URL + "/anthropic",
			config.ProtocolGemini:            upstream.URL + "/google",
		},
		Headers: map[string]string{
			"x-provider-session": "$CPA-SESSION-ID",
			"x-provider-static":  "literal",
		},
		Models: []config.ExternalModelConfig{
			{Name: "chat-model", Protocol: config.ProtocolOpenAIChat},
			{Name: "responses-model", Alias: "short-responses", Protocol: config.ProtocolOpenAIResponses},
			{Name: "messages-model", Protocol: config.ProtocolAnthropicMessages},
			{Name: "gemini-model", Protocol: config.ProtocolGemini},
		},
	}
	cliCfg, cliPath, err := config.BuildCLIProxyConfig(root, gateway, "unused-wopal-key", map[string]string{"multi": "provider-secret"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := cliproxy.NewBuilder().WithConfig(cliCfg).WithConfigPath(cliPath).Build()
	if err != nil {
		t.Fatalf("build CLIProxyAPI service: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("CLIProxyAPI service did not stop")
		}
	}()
	waitForTCP(t, fmt.Sprintf("127.0.0.1:%d", gateway.Listen.Port))

	base := fmt.Sprintf("http://127.0.0.1:%d/v1/responses", gateway.Listen.Port)
	aliases := []string{"multi/chat-model", "short-responses", "multi/messages-model", "multi/gemini-model"}
	for _, alias := range aliases {
		status, responseBody := postExternalExecutorRequest(t, base, alias)
		if status != http.StatusOK {
			t.Fatalf("model %s status=%d body=%s", alias, status, responseBody)
		}
	}

	mu.Lock()
	got := append([]externalUpstreamObservation(nil), observations...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("upstream observations = %d, want 4: %+v", len(got), got)
	}
	wantPaths := []string{
		"/openai/chat/completions",
		"/openai/responses",
		"/anthropic/v1/messages",
		"/google/v1beta/models/gemini-model:generateContent",
	}
	for index, observation := range got {
		if observation.Path != wantPaths[index] {
			t.Errorf("request %d path=%q want=%q", index, observation.Path, wantPaths[index])
		}
		if observation.Cookie != "" || observation.AccountID != "" {
			t.Errorf("request %d leaked client routing headers: %+v", index, observation)
		}
		if strings.Contains(observation.Auth, config.LocalAccessKey) || observation.GoogKey == config.LocalAccessKey || observation.GoogKey == "client-key-must-not-leak" {
			t.Errorf("request %d leaked gateway client credential: %+v", index, observation)
		}
		if observation.Static != "literal" {
			t.Errorf("request %d static header=%q", index, observation.Static)
		}
		if observation.Session == "" || observation.Session == "$CPA-SESSION-ID" {
			t.Errorf("request %d session header was not expanded: %q", index, observation.Session)
		}
	}
	if got[0].Auth != "Bearer provider-secret" || got[1].Auth != "Bearer provider-secret" || got[2].Auth != "Bearer provider-secret" {
		t.Fatalf("Bearer provider auth mismatch: %+v", got)
	}
	if !strings.Contains(got[2].RawQuery, "beta=true") {
		t.Fatalf("Anthropic messages query = %q, want beta=true", got[2].RawQuery)
	}
	if got[3].GoogKey != "provider-secret" && got[3].Auth != "Bearer provider-secret" {
		t.Fatalf("Gemini provider credential missing: %+v", got[3])
	}
}

func waitForTCP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("service never listened on %s", address)
}

func postExternalExecutorRequest(t *testing.T, url, model string) (int, string) {
	t.Helper()
	payload := fmt.Sprintf(`{"model":%q,"input":"say OK","stream":false}`, model)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
	req.Header.Set("Cookie", "client-cookie-must-not-leak")
	req.Header.Set("ChatGPT-Account-ID", "client-account-must-not-leak")
	req.Header.Set("x-goog-api-key", "client-key-must-not-leak")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", model, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
