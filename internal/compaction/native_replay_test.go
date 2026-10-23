package compaction

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/quota"
)

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
	bridge := &NativeBridge{chatgptTarget: target, openaiTarget: target, summaries: make(map[[32]byte]string)}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer official-token")
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

func TestNativeCompactionRestoreUsesWebsocketForChatGPT(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "wopal-codex-gateway-models.json"), []byte(`{"models":[{"slug":"gpt-6.1-sol","visibility":"list","priority":1}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var sawWebsocket atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "chatgpt restore must use websocket", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("restore path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer official-token" || r.Header.Get("ChatGPT-Account-ID") != "acct-test" {
			t.Error("restore websocket lost official auth headers")
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		sawWebsocket.Store(true)
		var message map[string]any
		if err := conn.ReadJSON(&message); err != nil {
			t.Error(err)
			return
		}
		if message["type"] != "response.create" || message["model"] != "gpt-6.1-sol" {
			t.Errorf("unexpected restore frame: %#v", message)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "restored over websocket"})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "r1", "status": "completed", "output": []any{map[string]any{
				"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "restored over websocket"}},
			}},
		}})
		<-r.Context().Done()
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")
	bridge := &NativeBridge{chatgptTarget: target, openaiTarget: target, summaries: make(map[[32]byte]string)}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-test")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	summary, err := bridge.summarize(ctx, req, []any{map[string]any{"type": "compaction", "id": "cmp-native", "encrypted_content": "official-ciphertext"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "restored over websocket" || !sawWebsocket.Load() {
		t.Fatalf("summary=%q websocket=%v", summary, sawWebsocket.Load())
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
	model, err := NativeModel()
	if err != nil || model != "gpt-native" {
		t.Fatalf("lost native catalog after refresh: %q %v", model, err)
	}
}

func TestNativeCompactionRestoreTimeoutAllowsSlowOfficialMaterialization(t *testing.T) {
	started := time.Now()
	ctx, cancel := restoreContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("native compaction restore context has no deadline")
	}
	got := deadline.Sub(started)
	if got < 179*time.Second || got > 181*time.Second {
		t.Fatalf("native compaction restore timeout = %s, want about 3m", got)
	}
}

func TestNativeCompactionRestoreStopsWhenQuotaProtected(t *testing.T) {
	now := time.Now()
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":99,"window_minutes":10080,"reset_after_seconds":3600}}}`), now)
	bridge := &NativeBridge{quotaGuard: guard}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	if _, err := bridge.summarize(req.Context(), req, []any{}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("quota reserve")) {
		t.Fatalf("protected restore error = %v", err)
	}
}
