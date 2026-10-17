package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	official := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-6.1-sol","input":[]}`))
	official.Header.Set("Content-Type", "application/json")
	official.Header.Set("ChatGPT-Account-ID", "acct")
	officialRecorder := httptest.NewRecorder()
	engine.ServeHTTP(officialRecorder, official)
	if officialRecorder.Code != http.StatusConflict || !bytes.Contains(officialRecorder.Body.Bytes(), []byte("official_quota_guard")) {
		t.Fatalf("official guard response = %d %s", officialRecorder.Code, officialRecorder.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("protected official request reached upstream %d times", upstreamHits)
	}

	wopal := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"wopal-ai/deepseek-v4.1-flash-free","input":[]}`))
	wopal.Header.Set("Content-Type", "application/json")
	wopal.Header.Set("ChatGPT-Account-ID", "acct")
	wopalRecorder := httptest.NewRecorder()
	engine.ServeHTTP(wopalRecorder, wopal)
	if wopalRecorder.Code != http.StatusOK {
		t.Fatalf("wopal request was blocked: %d %s", wopalRecorder.Code, wopalRecorder.Body.String())
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
	select {
	case payload := <-upstreamFrames:
		t.Fatalf("protected websocket turn reached upstream: %s", payload)
	case <-time.After(200 * time.Millisecond):
	}
}
