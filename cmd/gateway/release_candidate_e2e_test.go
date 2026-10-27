package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/provider"
	"wopal-codex-gateway/internal/proxy"
	"wopal-codex-gateway/internal/routing"
)

// upstreamHit records one request that reached the fake provider, so routing is
// asserted by observation rather than by trusting a response body.
type upstreamHit struct {
	Path   string
	Auth   string
	Body   map[string]any
	Header http.Header
}

// TestReleaseCandidateServesEveryProviderPath is the release gate for a built
// gateway. Unit suites prove pieces; this proves the assembly a release ships:
// config loading, provider registry, catalog merge, route resolution, both HTTP
// protocols, the websocket frame loop, and turn logging.
//
// The upstream is a local fake, so the test is hermetic, needs no credentials
// and costs nothing.
func TestReleaseCandidateServesEveryProviderPath(t *testing.T) {
	root := t.TempDir()
	var hits []upstreamHit

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		hits = append(hits, upstreamHit{
			Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
			Body: decoded, Header: r.Header.Clone(),
		})

		switch r.URL.Path {
		case "/vendor/openai/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"CHAT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case "/vendor/openai/responses":
			// The gateway streams upstream and aggregates for a non-streaming
			// client, so the fake speaks SSE like a real provider.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"model\":\"resp-model\"}}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"model\":\"resp-model\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"RESP_OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected upstream path"}}`))
		}
	}))
	defer upstream.Close()

	configPath := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	cfg := map[string]any{
		"version": 1,
		"listen":  map[string]any{"host": "127.0.0.1", "port": port},
		"wopalAi": map[string]any{
			"baseUrl":  upstream.URL + "/wopal",
			"toolMode": "code_mode_only",
			"models":   []any{},
		},
		"providers": map[string]any{
			"vendor": map[string]any{
				"baseUrl":    upstream.URL + "/vendor/openai",
				"modelsAuth": "none",
				"protocol":   "openai-chat",
				"models": []any{
					map[string]any{"name": "chat-model", "protocol": "openai-chat"},
					map[string]any{"name": "resp-model", "protocol": "openai-responses"},
				},
			},
		},
		"modelRoles": map[string]any{},
		"logs":       map[string]any{"maxSizeMb": 10, "maxBackups": 5},
		"quota":      map[string]any{"maxStepCap": 5},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	startTestGateway(t, root, configPath, "vendor/chat-model", port)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	t.Run("catalog exposes the external model", func(t *testing.T) {
		if models := getModels(t, base+"/v1/models"); !contains(models, "vendor/chat-model") {
			t.Errorf("catalog missing vendor/chat-model: %v", models)
		}
	})

	t.Run("chat completions reaches the external provider", func(t *testing.T) {
		before := len(hits)
		status, body := postJSON(t, base+"/v1/chat/completions", map[string]any{
			"model":    "vendor/chat-model",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"stream":   false,
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if !strings.Contains(body, "CHAT_OK") {
			t.Errorf("response body missing assistant text: %s", body)
		}
		assertReached(t, hits[before:], "/vendor/openai/chat/completions")
	})

	t.Run("responses reaches the external provider", func(t *testing.T) {
		before := len(hits)
		status, body := postJSON(t, base+"/v1/responses", map[string]any{
			"model":  "vendor/resp-model",
			"input":  "hi",
			"stream": false,
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if !strings.Contains(body, "RESP_OK") {
			t.Errorf("response body missing assistant text: %s", body)
		}
		assertReached(t, hits[before:], "/vendor/openai/responses")
	})

	t.Run("websocket keeps one socket usable across turns", func(t *testing.T) {
		conn := dialGatewayWebsocket(t, base, "vendor/resp-model")
		defer conn.Close()
		for turn := 1; turn <= 2; turn++ {
			before := len(hits)
			if err := conn.WriteJSON(map[string]any{
				"type": "response.create", "model": "vendor/resp-model", "stream": true,
				"input": []any{map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("turn-%d", turn)}},
			}); err != nil {
				t.Fatalf("turn %d write: %v", turn, err)
			}
			waitForTerminal(t, conn, turn)
			assertReached(t, hits[before:], "/vendor/openai/responses")
		}
	})

	t.Run("websocket closes when the model switches provider", func(t *testing.T) {
		conn := dialGatewayWebsocket(t, base, "vendor/chat-model")
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{
			"type": "response.create", "model": "wopal-ai/other", "stream": true, "input": []any{},
		}); err != nil {
			t.Fatal(err)
		}
		assertCloseCode(t, readUntilClose(conn), websocket.CloseServiceRestart, "model route changed")
	})

	t.Run("websocket rejects an unregistered model", func(t *testing.T) {
		conn := dialGatewayWebsocket(t, base, "vendor/chat-model")
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{
			"type": "response.create", "model": "vendor/nope", "stream": true, "input": []any{},
		}); err != nil {
			t.Fatal(err)
		}
		assertCloseCode(t, readUntilClose(conn), websocket.CloseServiceRestart, "model route unavailable")
	})

	t.Run("turn logs name the serving provider and stay metadata-only", func(t *testing.T) {
		rawLog := waitForTurnLog(t, filepath.Join(root, "logs", "gateway.log"))
		// Every turn event must name the provider that served it. Checking only
		// that "vendor" appears somewhere would still pass while an event
		// kept a hardcoded label.
		var started, completed bool
		for _, line := range strings.Split(rawLog, "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			switch entry["event"] {
			case "turn_started":
				started = true
			case "turn_completed":
				completed = true
			default:
				continue
			}
			if provider, _ := entry["provider"].(string); provider != "vendor" {
				t.Errorf("%s reported provider %q, want %q", entry["event"], provider, "vendor")
			}
		}
		if !started || !completed {
			t.Errorf("missing turn events: started=%v completed=%v", started, completed)
		}
		for _, forbidden := range []string{"turn-1", "turn-2", "CHAT_OK", "RESP_OK"} {
			if strings.Contains(rawLog, forbidden) {
				t.Errorf("gateway.log contains request or response content %q", forbidden)
			}
		}
	})
}

// startTestGateway assembles and runs the production service from a config
// file the way an install does, then waits until /_health answers.
//
// It builds the provider registry from the real config but supplies its own
// route table: the shipped table is derived from the authoritative native Codex
// artifact, and requiring that binary here would turn this test into a check of
// the developer machine rather than of the gateway.
func startTestGateway(t *testing.T, root, configPath, externalModel string, port int) {
	t.Helper()
	gateway, err := config.LoadGatewayConfig(configPath)
	if err != nil {
		t.Fatalf("load gateway config: %v", err)
	}
	if err := config.SetProjectRoot(root); err != nil {
		t.Fatalf("set project root: %v", err)
	}
	registry, err := provider.Build(gateway)
	if err != nil {
		t.Fatalf("build provider registry: %v", err)
	}
	external := map[string]routing.Route{}
	for _, model := range registry.Models() {
		if model.ProviderID == provider.OfficialID {
			continue
		}
		external[model.ClientModel] = routing.Route{Kind: routing.External, ProviderID: model.ProviderID}
	}
	external["wopal-ai/other"] = routing.Route{Kind: routing.Wopal, ProviderID: provider.WopalAIID}
	table := routing.NewTable(external, nil, nil)

	keys := map[string]string{}
	for id := range gateway.Providers {
		keys[id] = "test-provider-key"
	}

	roles := proxy.RuntimeRoles{
		OfficialFallback: proxy.RoleTarget{ProviderID: provider.WopalAIID, ClientModel: externalModel, Ready: true},
		Compaction:       proxy.RoleTarget{ProviderID: provider.WopalAIID, ClientModel: externalModel, Ready: true},
	}
	service, logger, err := buildGatewayServiceWithRuntimeLogger(
		root, gateway, "test-wopal-key", keys, "e2e",
		table.Resolve, true, nil, gatewayServiceRuntime{
			Roles:         roles,
			Reload:        newRuntimeReloadStatus(),
			StatusGateway: gateway,
		})
	if err != nil {
		t.Fatalf("build gateway service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("gateway service did not stop")
		}
		logger.Close()
	})
	waitForHealth(t, fmt.Sprintf("http://127.0.0.1:%d/_health", port))
}

func dialGatewayWebsocket(t *testing.T, base, boundModel string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/v1/responses", http.Header{
		"Authorization":        {"Bearer " + config.LocalAccessKey},
		"X-Codex-Routing-Hint": {"model=" + boundModel},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	return conn
}

func getModels(t *testing.T, url string) []string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		ids = append(ids, entry.ID)
	}
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertReached(t *testing.T, hits []upstreamHit, want string) {
	t.Helper()
	for _, hit := range hits {
		if hit.Path == want {
			return
		}
	}
	paths := make([]string, 0, len(hits))
	for _, hit := range hits {
		paths = append(paths, hit.Path)
	}
	t.Errorf("upstream never received %q; saw %v", want, paths)
}

func waitForTerminal(t *testing.T, conn *websocket.Conn, turn int) {
	t.Helper()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("turn %d read: %v", turn, err)
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.completed", "response.failed", "error", "response.incomplete":
			return
		}
	}
}

func readUntilClose(conn *websocket.Conn) error {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return err
		}
	}
}

func assertCloseCode(t *testing.T, err error, wantCode int, wantText string) {
	t.Helper()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("connection error = %v, want a close error %d", err, wantCode)
	}
	if closeErr.Code != wantCode {
		t.Errorf("close code = %d, want %d", closeErr.Code, wantCode)
	}
	if wantText != "" && !strings.Contains(closeErr.Text, wantText) {
		t.Errorf("close reason = %q, want it to contain %q", closeErr.Text, wantText)
	}
}

func waitForTurnLog(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if text := string(data); len(turnLogProviders(text)) > 0 {
				return text
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no turn events appeared in %s", path)
	return ""
}

func turnLogProviders(rawLog string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(rawLog, string(rune(10))) {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		switch entry["event"] {
		case "turn_started", "turn_completed":
			if provider, ok := entry["provider"].(string); ok && provider != "" {
				seen[provider] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for provider := range seen {
		out = append(out, provider)
	}
	return out
}
