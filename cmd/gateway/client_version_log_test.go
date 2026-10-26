package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
)

// TestServedGatewayLogsClientVersionAndToolSurface proves the operator can tell
// which Codex build sent a request and what tool surface it declared.
//
// Codex ships several times a week and a release can change the request
// contract without failing to build, so the gateway records the client version
// and the observed shape next to the routing decision. Without this, a
// regression after an upgrade can only be diagnosed by diffing captures that
// were never taken.
func TestServedGatewayLogsClientVersionAndToolSurface(t *testing.T) {
	root := t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-cfp","object":"chat.completion","created":1,"model":"qwen3-coder","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	gateway := config.DefaultGatewayConfig()
	gateway.Version = 1
	gateway.Listen.Port = freePort(t)
	gateway.WopalAI.BaseURL = upstream.URL + "/v1"
	gateway.WopalAI.Models = []config.ExternalModelConfig{{Name: "qwen3-coder"}}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}

	service, _, err := buildGatewayService(root, gateway, "test-wopal-key", nil, "clientfp")
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("service did not stop after context cancel")
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", gateway.Listen.Port)
	waitForHealth(t, base+"/_health")

	// Two requests from different versions with different tool surfaces, the
	// exact transition that used to break a thread on a model switch.
	sendCodexTurn(t, base+"/v1/responses", "0.164.0-alpha.11.1", "Codex Desktop",
		map[string]any{
			"model": "wopal-ai/qwen3-coder",
			"input": []any{
				map[string]any{"type": "additional_tools", "tools": []any{
					map[string]any{"type": "namespace", "name": "functions", "tools": []any{
						map[string]any{"type": "custom", "name": "exec"},
					}},
				}},
				map[string]any{"type": "message", "role": "user", "content": "hi"},
			},
		})

	sendCodexTurn(t, base+"/v1/responses", "0.165.0-alpha.2", "Codex Desktop",
		map[string]any{
			"model": "wopal-ai/qwen3-coder",
			"tools": []any{
				map[string]any{"type": "function", "name": "exec_command"},
			},
			"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
		})

	entries := readGatewayLog(t, filepath.Join(root, "logs", "gateway.log"))

	codeMode := findRouteEntry(t, entries, "0.164.0-alpha.11.1")
	if codeMode["client_originator"] != "Codex Desktop" {
		t.Fatalf("originator = %v", codeMode["client_originator"])
	}
	if codeMode["request_tool_surface"] != "code_mode" {
		t.Fatalf("tool surface = %v, want code_mode", codeMode["request_tool_surface"])
	}
	functions, _ := codeMode["request_functions"].([]any)
	if len(functions) != 1 || functions[0] != "exec" {
		t.Fatalf("functions = %v, want [exec]", codeMode["request_functions"])
	}

	direct := findRouteEntry(t, entries, "0.165.0-alpha.2")
	if direct["request_tool_surface"] != "direct" {
		t.Fatalf("tool surface = %v, want direct", direct["request_tool_surface"])
	}
	if _, ok := direct["request_functions"]; ok {
		t.Fatalf("direct request reported functions: %v", direct["request_functions"])
	}
	if direct["request_tool_count"].(float64) != 1 {
		t.Fatalf("tool count = %v, want 1", direct["request_tool_count"])
	}

	// The digest must differ, which is what makes an unnoticed surface change
	// visible in the log.
	if codeMode["request_tool_digest"] == direct["request_tool_digest"] {
		t.Fatal("code-mode and direct requests produced the same tool digest")
	}
}

func sendCodexTurn(t *testing.T, url, version, originator string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("version", version)
	req.Header.Set("originator", originator)
	req.Header.Set("User-Agent", "Codex Desktop/"+version+" (Mac OS 26.6.2; arm64) dumb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn status = %d", resp.StatusCode)
	}
}

func readGatewayLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open gateway log: %v", err)
	}
	defer file.Close()
	var entries []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// findRouteEntry returns the route_selected entry for one client version.
// The logger writes synchronously before the request is forwarded, so the
// entry is already on disk when the client sees a response.
func findRouteEntry(t *testing.T, entries []map[string]any, version string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["event"] == "route_selected" && entry["client_version"] == version {
			return entry
		}
	}
	t.Fatalf("no route_selected entry for version %q in %d entries", version, len(entries))
	return nil
}
