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
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
)

const e2ePromptMarker = "SECRET_PROMPT_MARKER_E2E"

// TestServedGatewayNeverWritesPromptBodies is the end-to-end regression for the
// request-body privacy boundary. It runs the real embedded CLIProxyAPI service
// through the production assembly (buildGatewayService), drives one successful
// turn and one failing turn, and then proves that neither produced a request
// body, response body, or SDK error log on disk.
//
// The unit tests only prove the factory returns nil. This test proves the nil
// factory actually removes the middleware in the service that serve() builds,
// which is the property the privacy claim depends on.
func TestServedGatewayNeverWritesPromptBodies(t *testing.T) {
	root := t.TempDir()

	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "force-upstream-failure") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic upstream failure","type":"server_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":"qwen3-coder","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
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

	service, _, err := buildGatewayService(root, gateway, "test-wopal-key", "e2e")
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

	// Success path: the SDK returns a normal completion.
	status, body := postJSON(t, base+"/v1/responses", map[string]any{
		"model":  "wopal-ai/qwen3-coder",
		"input":  e2ePromptMarker + " success",
		"stream": false,
	})
	if status != http.StatusOK {
		t.Fatalf("success turn status=%d body=%s", status, body)
	}

	// Failure path: an upstream 5xx is exactly what forces an error-only log
	// when a request logger exists.
	status, body = postJSON(t, base+"/v1/responses", map[string]any{
		"model":  "wopal-ai/qwen3-coder",
		"input":  e2ePromptMarker + " force-upstream-failure",
		"stream": false,
	})
	if status == http.StatusOK {
		t.Fatalf("failure turn unexpectedly succeeded: %s", body)
	}

	// Truly malformed body: not valid JSON, so it is rejected before being
	// forwarded upstream while still traversing the downstream SDK chain,
	// where a request logger would capture it. This is distinct from the
	// upstream-failure path above.
	status, body = postRaw(t, base+"/v1/responses", "{"+e2ePromptMarker)
	if status < 400 || status >= 500 {
		t.Fatalf("malformed body status=%d, want a 4xx: %s", status, body)
	}

	if upstreamHits == 0 {
		t.Fatal("fake upstream was never called; the test proved nothing")
	}

	assertNoPromptArtifactsUnder(t, root)
	assertStatusReportsBodyLoggingDisabled(t, base+"/_status")
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForHealth(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("gateway never became healthy at %s", url)
}

// postRaw sends a body that is not valid JSON and asserts nothing about how
// the gateway rejects it beyond the status class.
func postRaw(t *testing.T, url, raw string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func postJSON(t *testing.T, url string, payload map[string]any) (int, string) {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// assertNoPromptArtifactsUnder scans the whole runtime tree, not just the SDK's
// chosen log directory, so a future bypass path cannot hide behind a different
// location.
func assertNoPromptArtifactsUnder(t *testing.T, root string) {
	t.Helper()
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		for _, prefix := range []string{"request-log", "request-body", "response-body", "error-"} {
			if strings.HasPrefix(name, prefix) {
				offenders = append(offenders, path+" (name)")
			}
		}
		if info.Size() == 0 || info.Size() > 8<<20 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(content), e2ePromptMarker) {
			offenders = append(offenders, path+" (contains prompt)")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("prompt artifacts reached disk: %v", offenders)
	}

	if _, statErr := os.Stat(filepath.Join(root, "runtime", "cliproxy")); statErr == nil {
		t.Fatal("runtime/cliproxy exists; the pinned-writable-path mechanism is still active")
	}
}

func assertStatusReportsBodyLoggingDisabled(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		RequestBodyLogging string `json:"request_body_logging"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.RequestBodyLogging != requestBodyLoggingState {
		t.Fatalf("request_body_logging = %q, want %q", status.RequestBodyLogging, requestBodyLoggingState)
	}
}
