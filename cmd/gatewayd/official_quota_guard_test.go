package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestOfficialQuotaGuardThresholdsAndReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := newOfficialQuotaGuard(nil)

	guard.observeRateLimitPayload("acct-five", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	blocked, reason := guard.blocked("acct-five", now)
	if !blocked || reason != "five_hour" {
		t.Fatalf("five-hour guard = blocked:%v reason:%q", blocked, reason)
	}

	guard.observeRateLimitPayload("acct-week", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":97,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":99,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	blocked, reason = guard.blocked("acct-week", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("weekly guard = blocked:%v reason:%q", blocked, reason)
	}

	guard.observeRateLimitPayload("acct-open", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":97,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	if blocked, _ := guard.blocked("acct-open", now); blocked {
		t.Fatal("guard blocked below both thresholds")
	}

	if blocked, _ := guard.blocked("acct-five", now.Add(301*time.Second)); blocked {
		t.Fatal("five-hour guard did not clear after reset")
	}
}

func TestOfficialQuotaGuardReadsHeadersByWindowLength(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := newOfficialQuotaGuard(nil)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "99")
	headers.Set("X-Codex-Primary-Window-Minutes", "10080")
	headers.Set("X-Codex-Primary-Reset-At", "1800100000")
	headers.Set("X-Codex-Secondary-Used-Percent", "97")
	headers.Set("X-Codex-Secondary-Window-Minutes", "300")
	headers.Set("X-Codex-Secondary-Reset-At", "1800000300")

	guard.observeHeaders("acct", headers, now)
	blocked, reason := guard.blocked("acct", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("header guard = blocked:%v reason:%q", blocked, reason)
	}
}

func TestRoutingQuotaGuardBlocksOfficialButAllowsWopal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := newOfficialQuotaGuard(nil)
	now := time.Now()
	guard.observeRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"provider": "wopal"})
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	post := func(model string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", bytes.NewBufferString(`{"model":"`+model+`","input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("ChatGPT-Account-ID", "acct")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	status, body := post("gpt-6.1-sol")
	if status != http.StatusConflict || !strings.Contains(body, "official_quota_guard") {
		t.Fatalf("official guard response = %d %s", status, body)
	}
	if !strings.Contains(body, "gpt-5.6-luna") {
		t.Fatalf("official guard response did not advertise Luna recovery: %s", body)
	}
	if upstreamHits != 0 {
		t.Fatalf("protected official request reached upstream %d times", upstreamHits)
	}

	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "codex-auto-review"} {
		status, body = post(model)
		if status != http.StatusOK {
			t.Fatalf("quota-exempt model %s was blocked: %d %s", model, status, body)
		}
	}
	if upstreamHits != 3 {
		t.Fatalf("quota-exempt official models reached upstream %d times, want 3", upstreamHits)
	}

	status, body = post("wopal-ai/deepseek-v4.1-flash-free")
	if status != http.StatusOK {
		t.Fatalf("wopal request was blocked: %d %s", status, body)
	}
}

func TestOfficialQuotaGuardModelExemptions(t *testing.T) {
	now := time.Now()
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	for _, model := range []string{"gpt-6-luna", "codex-auto-review"} {
		if blocked, reason := guard.blockedModel("acct", model, now); blocked {
			t.Fatalf("quota-exempt model %s blocked with reason %q", model, reason)
		}
	}
	if blocked, reason := guard.blockedModel("acct", "gpt-6.1-sol", now); !blocked || reason != "five_hour" {
		t.Fatalf("metered model guard = blocked:%v reason:%q", blocked, reason)
	}
}

func TestNativeCompactionRestoreStopsWhenQuotaProtected(t *testing.T) {
	now := time.Now()
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":99,"window_minutes":10080,"reset_after_seconds":3600}}}`), now)
	bridge := &nativeCompactionBridge{quotaGuard: guard}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	if _, err := bridge.summarize(req.Context(), req, []any{}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("quota reserve")) {
		t.Fatalf("protected restore error = %v", err)
	}
}

func TestOfficialWebsocketQuotaGuardStopsNextTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := newOfficialQuotaGuard(nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamConnected := make(chan struct{}, 1)
	upstreamFrames := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		upstreamConnected <- struct{}{}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, payload, err := conn.ReadMessage(); err == nil {
			upstreamFrames <- payload
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")

	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	header := http.Header{}
	header.Set("ChatGPT-Account-ID", "acct-ws")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-upstreamConnected:
	case <-time.After(time.Second):
		t.Fatal("official upstream websocket did not connect")
	}

	guard.observeRateLimitPayload("acct-ws", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-6.1-sol", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("guard close = %T %v", err, err)
	}
	if !strings.Contains(closeErr.Text, "gpt-5.6-luna") {
		t.Fatalf("guard close did not advertise Luna recovery: %q", closeErr.Text)
	}
	select {
	case payload := <-upstreamFrames:
		t.Fatalf("protected websocket turn reached upstream: %s", payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOfficialWebsocketQuotaGuardAllowsLuna(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct-luna", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamFrames := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, payload, err := conn.ReadMessage(); err == nil {
			upstreamFrames <- payload
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")

	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	header := http.Header{}
	header.Set("ChatGPT-Account-ID", "acct-luna")
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-luna")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.6-luna", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-upstreamFrames:
		if !bytes.Contains(payload, []byte(`"model":"gpt-5.6-luna"`)) {
			t.Fatalf("unexpected Luna upstream frame: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("quota-exempt Luna websocket turn did not reach upstream")
	}
}

func TestOfficialQuotaGuardPersistsProtectedStateAcrossRestart(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	statePath := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := newOfficialQuotaGuardWithState(nil, statePath, now)
	guard.observeRateLimitPayload("private-account-id", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600},"secondary":{"used_percent":53,"window_minutes":10080,"reset_after_seconds":7200}}}`), now)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-account-id")) {
		t.Fatal("quota persistence leaked the raw ChatGPT account id")
	}

	reloaded := newOfficialQuotaGuardWithState(nil, statePath, now.Add(time.Second))
	if blocked, reason := reloaded.blockedModel("private-account-id", "gpt-6.1-sol", now.Add(time.Second)); !blocked || reason != "five_hour" {
		t.Fatalf("reloaded guard = blocked:%v reason:%q", blocked, reason)
	}
	if blocked, reason := reloaded.blockedModel("another-account-after-restart", "gpt-6.1-sol", now.Add(time.Second)); blocked {
		t.Fatalf("persisted quota state leaked across accounts: reason=%q", reason)
	}
	if blocked, _ := reloaded.blockedModel("private-account-id", "gpt-6.1-sol", now.Add(3601*time.Second)); blocked {
		t.Fatal("persisted five-hour protection survived its reset time")
	}
}

func TestOfficialQuotaGuardStatusUsesHashedAccountState(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("private-account-id", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)
	status := guard.status(now)
	if status["state"] != "protected" || status["reason"] != "five_hour" {
		t.Fatalf("quota status = %#v", status)
	}
}

func TestQuotaExemptOfficialModelsIncludeCurrentLuna(t *testing.T) {
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "codex-auto-review"} {
		if !quotaExemptOfficialModel(model) {
			t.Fatalf("model %q should bypass quota reserve", model)
		}
	}
	if quotaExemptOfficialModel("gpt-6.1-sol") {
		t.Fatal("gpt-6.1-sol should remain quota protected")
	}
}
