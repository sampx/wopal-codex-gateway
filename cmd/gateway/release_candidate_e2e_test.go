package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
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
	var responses atomic.Int32

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
			// client, so the fake speaks SSE like a real provider. Each call
			// answers with its own response id so a test can tell two turns
			// apart instead of seeing one repeated answer.
			responseID := fmt.Sprintf("resp-%d", responses.Add(1))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"model\":\"resp-model\"}}\n\n", responseID)
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"model\":\"resp-model\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"RESP_OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", responseID)
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
			"models":   []any{map[string]any{"name": "other", "protocol": "openai-chat"}},
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
		"modelRoles": map[string]any{
			"compaction": map[string]any{"provider": "vendor", "model": "resp-model"},
		},
		"logs":  map[string]any{"maxSizeMb": 10, "maxBackups": 5},
		"quota": map[string]any{"maxStepCap": 5},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := startTestGateway(t, root, configPath, "e2e", port)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	t.Run("catalog exposes the external model", func(t *testing.T) {
		models := getModels(t, base+"/v1/models")
		for _, want := range []string{"vendor/chat-model", "vendor/resp-model"} {
			if !contains(models, want) {
				t.Errorf("catalog missing %q: %v", want, models)
			}
		}
		// The catalog must come from the same snapshot the route table came
		// from, so an external model cannot be advertised without a route.
		for _, model := range []string{"vendor/chat-model", "vendor/resp-model"} {
			if _, err := snapshot.routes.Resolve(model); err != nil {
				t.Errorf("snapshot has no route for advertised model %q: %v", model, err)
			}
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
		seen := map[string]bool{}
		for turn := 1; turn <= 2; turn++ {
			before := len(hits)
			if err := conn.WriteJSON(map[string]any{
				"type": "response.create", "model": "vendor/resp-model", "stream": true,
				"input": []any{map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("turn-%d", turn)}},
			}); err != nil {
				t.Fatalf("turn %d write: %v", turn, err)
			}
			responseID := waitForCompletion(t, conn, turn)
			if seen[responseID] {
				t.Errorf("turn %d reused response id %q", turn, responseID)
			}
			seen[responseID] = true
			assertReached(t, hits[before:], "/vendor/openai/responses")
		}
	})

	t.Run("websocket closes when the model switches provider", func(t *testing.T) {
		// Ask the snapshot which model crosses a provider boundary, so this
		// assertion follows the real route table instead of a hardcoded name.
		other := ""
		for _, model := range []string{"wopal-ai/other", "official-model"} {
			route, err := snapshot.routes.Resolve(model)
			if err != nil || route.ProviderID == "vendor" {
				continue
			}
			other = model
			break
		}
		if other == "" {
			t.Fatal("snapshot exposed no route on a second provider")
		}
		conn := dialGatewayWebsocket(t, base, "vendor/chat-model")
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{
			"type": "response.create", "model": other, "stream": true, "input": []any{},
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

// startTestGateway runs the release candidate through the assembly serve uses,
// not a copy of it. That matters: the daemon builds its runtime truth in
// loadGatewaySnapshot -- resolved models, provider registry, official baseline
// and artifact verifier, collision checks, model roles, and the exact route
// table -- and a test that hand-built those would still pass while that
// builder was broken.
//
// The official baseline is seeded with a throwaway native artifact, which keeps
// the run hermetic without weakening it: the snapshot loader is the same code,
// so a regression in route or role derivation fails this gate.
func startTestGateway(t *testing.T, root, configPath, instance string, port int) *gatewaySnapshot {
	t.Helper()

	codex := filepath.Join(root, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Restore the package-level root afterwards: the snapshot loader sets it as
	// part of serving, and a test must not leave it pointing at a temp dir.
	t.Cleanup(config.SetProjectRootForTest(root))
	identity, err := catalog.ComputeNativeArtifactIdentity(codex)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveOfficialModelsBaseline(&catalog.OfficialModelsBaseline{
		Artifact: identity, Slugs: []string{"official-model"}, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Credentials are the only thing the test supplies: the release gate must
	// not read the operator's Keychain.
	restoreWopal := replaceRuntimeWopalKey(func() (string, error) { return "test-wopal-key", nil })
	defer restoreWopal()
	restoreProviders := replaceRuntimeProviderKeys(func(cfg *config.GatewayConfig) (map[string]string, error) {
		keys := make(map[string]string, len(cfg.Providers))
		for providerID := range cfg.Providers {
			keys[providerID] = "test-provider-key"
		}
		return keys, nil
	})
	defer restoreProviders()

	daemonLogger, err := activity.New(config.LogsDirFor(root), config.Logs{})
	if err != nil {
		t.Fatalf("initialize daemon logger: %v", err)
	}
	t.Cleanup(daemonLogger.Close)

	reloadStatus := newRuntimeReloadStatus()
	switcher := newBackendSwitcher(nil)
	backend, snapshot, err := startGatewayBackendWithLoggerAndResolver(root, configPath, instance, daemonLogger, reloadStatus, switcher.Resolve)
	if err != nil {
		t.Fatalf("start gateway backend: %v", err)
	}
	switcher.Swap(backend)
	t.Cleanup(switcher.Close)

	listen := snapshot.gateway.Listen
	if listen.Port != port {
		t.Fatalf("snapshot listen port = %d, want %d", listen.Port, port)
	}
	front := &http.Server{
		Addr:    net.JoinHostPort(listen.Host, strconv.Itoa(listen.Port)),
		Handler: switcher,
	}
	frontErr := make(chan error, 1)
	go func() { frontErr <- front.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = front.Shutdown(ctx)
		select {
		case <-frontErr:
		case <-time.After(5 * time.Second):
		}
	})
	waitForHealth(t, fmt.Sprintf("http://%s/_health", front.Addr))
	return snapshot
}

// replaceRuntimeWopalKey points the production assembly at a fake Wopal key so
// the gate never reads the operator's Keychain.
func replaceRuntimeWopalKey(replacement func() (string, error)) func() {
	previous := runtimeGetWopalAPIKey
	runtimeGetWopalAPIKey = replacement
	return func() { runtimeGetWopalAPIKey = previous }
}

// replaceRuntimeProviderKeys does the same for external provider credentials.
func replaceRuntimeProviderKeys(replacement func(*config.GatewayConfig) (map[string]string, error)) func() {
	previous := runtimeLoadProviderKeys
	runtimeLoadProviderKeys = replacement
	return func() { runtimeLoadProviderKeys = previous }
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

// waitForCompletion requires the turn to actually succeed. A terminal failure
// is still a terminal event, so accepting any of them would let a socket that
// breaks on the second turn pass a test named "usable across turns".
func waitForCompletion(t *testing.T, conn *websocket.Conn, turn int) string {
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
		case "response.completed":
			response, _ := event["response"].(map[string]any)
			responseID, _ := response["id"].(string)
			return responseID
		case "response.failed", "error", "response.incomplete":
			t.Fatalf("turn %d ended as %v: %s", turn, event["type"], raw)
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
