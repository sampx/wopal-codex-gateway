package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestCmbReasoningSanitizedAcrossTransports(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			raw := []byte(`{"type":"response.create","model":"gpt-6-luna","input":[{"type":"reasoning","id":"rs_cmb-abc_0","encrypted_content":""},{"type":"reasoning","id":"rs_native","encrypted_content":"keep-me"},{"type":"message","role":"user","content":"hello"}]}`)
			if transport == "http" {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
				rewriteOfficialRequestBody(req, nil)
				raw, _ = io.ReadAll(req.Body)
			} else {
				if !containsOfficialSanitizeMarker(raw) {
					t.Fatal("cmb reasoning bypasses websocket sanitizer")
				}
				var ok bool
				raw, ok = sanitizeOfficialWebsocketFrame(raw)
				if !ok {
					t.Fatal("frame not sanitized")
				}
			}
			if strings.Contains(string(raw), "rs_cmb-") || !strings.Contains(string(raw), "keep-me") {
				t.Fatalf("wrong reasoning sanitization: %s", raw)
			}
		})
	}
}

func TestNativeCompactionRestoreStopsAfterCompletedEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "wopal-codex-gateway-models.json"), []byte(`{"models":[{"slug":"gpt-native","visibility":"list","priority":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"restored summary\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"restored summary\"}]}]}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")
	bridge := &nativeCompactionBridge{chatgptTarget: target, openaiTarget: target, summaries: make(map[[32]byte]string)}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-test")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	summary, err := bridge.summarize(ctx, req, []any{map[string]any{"type": "compaction", "id": "cmp-native", "encrypted_content": "official-ciphertext"}})
	if err != nil {
		t.Fatalf("completed restore must return before stream EOF: %v", err)
	}
	if summary != "restored summary" {
		t.Fatalf("summary = %q", summary)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("restore waited for stream EOF after completion: %s", elapsed)
	}
}

func TestNativeCompactionModelSurvivesCodexCacheRefresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"wopal-ai/test","visibility":"list"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "wopal-codex-gateway-models.json"), []byte(`{"models":[{"slug":"wopal-ai/test","visibility":"list","priority":-200},{"slug":"gpt-native","visibility":"list","priority":1}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	model, err := nativeCompactionModel()
	if err != nil || model != "gpt-native" {
		t.Fatalf("lost native catalog after refresh: %q %v", model, err)
	}
}

// A native compaction capsule is opaque to third-party models. Expand it with
// its owning provider before switching, never discard it or endlessly retry it.
func TestNativeCompactionModelSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-test-native","visibility":"list","priority":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var summaries atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer official-token" || r.Header.Get("ChatGPT-Account-ID") != "acct-test" {
			t.Error("native summary lost official auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt-test-native" {
			t.Errorf("summary model: %v", body["model"])
		}
		encoded, _ := json.Marshal(body)
		if !bytes.Contains(encoded, []byte("official-ciphertext")) {
			t.Error("native capsule not sent to its owner")
		}
		if body["tools"] != nil {
			t.Error("summary must not execute tools")
		}
		summaries.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-summary\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Remember ORCHID-7319\"}]}]}}\n\n")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")
	var posts atomic.Int32
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(nil, target, target))
	engine.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		_ = c.ShouldBindJSON(&body)
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte("official-ciphertext")) || !bytes.Contains(raw, []byte("ORCHID-7319")) || !bytes.Contains(raw, []byte("continue-user")) {
			t.Errorf("third-party history not restored: %s", raw)
		}
		if c.GetHeader("Authorization") != "Bearer "+localAccessKey || c.GetHeader("ChatGPT-Account-ID") != "" {
			t.Error("official credentials leaked to third-party handler")
		}
		posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"wopal-result\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	for _, transport := range []string{"websocket", "http"} {
		t.Run(transport, func(t *testing.T) {
			message := map[string]any{"type": "response.create", "model": "wopal-ai/test", "input": []any{
				map[string]any{"type": "compaction", "id": "cmp-native", "encrypted_content": "official-ciphertext"},
				map[string]any{"type": "message", "role": "user", "content": "continue-user"},
			}}
			headers := http.Header{"Authorization": {"Bearer official-token"}, "Chatgpt-Account-Id": {"acct-test"}, "X-Codex-Routing-Hint": {"model=wopal-ai/test"}}
			if transport == "websocket" {
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(message); err != nil {
					t.Fatal(err)
				}
				for {
					var event map[string]any
					if err := conn.ReadJSON(&event); err != nil {
						t.Fatal(err)
					}
					if event["type"] == "error" {
						t.Fatalf("switch rejected: %v", event)
					}
					if event["type"] == "response.completed" {
						break
					}
				}
			} else {
				delete(message, "type")
				raw, _ := json.Marshal(message)
				req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(raw))
				req.Header = headers
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != 200 || !bytes.Contains(body, []byte("response.completed")) {
					t.Fatalf("HTTP switch failed: %d %s", resp.StatusCode, body)
				}
			}
		})
	}
	if posts.Load() != 2 {
		t.Fatalf("third-party calls=%d", posts.Load())
	}
	if summaries.Load() != 1 {
		t.Fatalf("same native capsule summarized %d times, want one cached conversion", summaries.Load())
	}
}

func TestPostRoutingUsesActualModelOverStaleHint(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","input":[]}`))
	req.Header.Set("X-Codex-Routing-Hint", "model=wopal-ai/old")
	model, err := detectRequestModel(req)
	if err != nil || model != "gpt-6-luna" {
		t.Fatalf("routed by stale handshake hint: %q %v", model, err)
	}
}

func TestWopalHTTPCompactionAndReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configureWopalCompactionKey("http-compaction-test")
	var posts atomic.Int32
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(nil, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1")))
	engine.POST("/v1/responses/compact", func(c *gin.Context) {
		t.Error("compact fell through to upstream's nonexistent compact model")
		c.Status(503)
	})
	engine.POST("/v1/responses", func(c *gin.Context) {
		var message map[string]any
		if err := c.ShouldBindJSON(&message); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(message)
		if bytes.Contains(raw, []byte("wgcp1.")) || bytes.Contains(raw, []byte("compaction_trigger")) {
			t.Error("opaque state reached chat translator")
		}
		posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"summary\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Remember HTTP-TOKEN\"}]}]}}\n\n")
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	for _, path := range []string{"/v1/responses/compact", "/v1/responses"} {
		input := []any{map[string]any{"type": "message", "role": "user", "content": "Remember HTTP-TOKEN"}}
		if path == "/v1/responses" {
			input = append(input, map[string]any{"type": "compaction_trigger"})
		}
		raw, _ := json.Marshal(map[string]any{"model": "wopal-ai/test", "input": input, "stream": true})
		resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("compact %s: %d %s", path, resp.StatusCode, body)
		}
		var completed map[string]any
		if path == "/v1/responses/compact" {
			_ = json.Unmarshal(body, &completed)
		} else {
			_ = readSSE(bytes.NewReader(body), func(data []byte) error {
				var event map[string]any
				_ = json.Unmarshal(data, &event)
				if event["type"] == "response.completed" {
					completed, _ = event["response"].(map[string]any)
				}
				return nil
			})
		}
		output, _ := completed["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("expected exactly one compaction output item, got %s", body)
		}
		item, _ := output[0].(map[string]any)
		capsule, err := openWopalCompactionCapsule(wopalEncryptedContent(item))
		if err != nil || !strings.Contains(capsule.Summary, "HTTP-TOKEN") {
			t.Fatalf("unreadable summary: %v", err)
		}
		replay, _ := json.Marshal(map[string]any{"model": "wopal-ai/test", "input": []any{item, map[string]any{"type": "message", "role": "user", "content": "continue"}}})
		resp, err = http.Post(server.URL+"/v1/responses", "application/json", bytes.NewReader(replay))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("replay HTTP %d", resp.StatusCode)
		}
	}
	if posts.Load() != 4 {
		t.Fatalf("unexpected HTTP upstream calls=%d", posts.Load())
	}
}

func TestModelOmittedAfterIntraWopalSwitchUsesLatestModel(t *testing.T) {
	state := wopalConversationState{lastRequest: []byte(`{"model":"wopal-ai/second","input":[]}`), lastResponseID: "r1"}
	prepared, err := prepareWopalWebsocketTurn(map[string]any{"type": "response.append", "previous_response_id": "r1", "input": []any{}}, "wopal-ai/first", state)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(prepared.body, &body)
	if body["model"] != "wopal-ai/second" {
		t.Fatalf("inherited stale handshake model: %s", prepared.body)
	}
}

func TestNativeHistoryFailureDoesNotForwardOrDiscardContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(nil, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1")))
	engine.POST("/v1/responses", func(c *gin.Context) { t.Error("unrestored history must not reach third party"); c.Status(200) })
	server := httptest.NewServer(engine)
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"wopal-ai/test","input":[{"type":"compaction","encrypted_content":"native-secret"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("unrecoverable history returned retryable status %d", resp.StatusCode)
	}
}
