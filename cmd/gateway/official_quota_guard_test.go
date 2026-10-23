package main

import (
	"bytes"
	"encoding/json"
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

	guard.observeRateLimitPayload("acct-week", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":99,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	blocked, reason = guard.blocked("acct-week", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("weekly guard = blocked:%v reason:%q", blocked, reason)
	}

	guard.observeRateLimitPayload("acct-open", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
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
	headers.Set("X-Codex-Secondary-Used-Percent", "96")
	headers.Set("X-Codex-Secondary-Window-Minutes", "300")
	headers.Set("X-Codex-Secondary-Reset-At", "1800000300")

	guard.observeHeaders("acct", headers, now)
	blocked, reason := guard.blocked("acct", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("header guard = blocked:%v reason:%q", blocked, reason)
	}
}

func TestRoutingQuotaGuardFailsOverOfficialAndAllowsExemptions(t *testing.T) {
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
	if status != http.StatusOK {
		t.Fatalf("official failover response = %d %s", status, body)
	}
	if upstreamHits != 0 {
		t.Fatalf("protected official request reached upstream %d times", upstreamHits)
	}

	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		status, body = post(model)
		if status != http.StatusOK {
			t.Fatalf("quota-exempt model %s was blocked: %d %s", model, status, body)
		}
	}
	if upstreamHits != 2 {
		t.Fatalf("quota-exempt official models reached upstream %d times, want 2", upstreamHits)
	}

	// The internal approval reviewer is metered, so it must not reach the
	// official provider while the reserve is protected.
	status, body = post("codex-auto-review")
	if status != http.StatusOK {
		t.Fatalf("protected auto-review response = %d %s", status, body)
	}
	if upstreamHits != 2 {
		t.Fatalf("protected auto-review reached official upstream; hits = %d", upstreamHits)
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

	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		if blocked, reason := guard.blockedModel("acct", model, now); blocked {
			t.Fatalf("quota-exempt model %s blocked with reason %q", model, reason)
		}
	}
	if blocked, reason := guard.blockedModel("acct", "codex-auto-review", now); !blocked || reason != "five_hour" {
		t.Fatalf("auto-review guard = blocked:%v reason:%q", blocked, reason)
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

func TestOfficialWebsocketQuotaGuardStopsCurrentSocketBeforeFailover(t *testing.T) {
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
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		if !quotaExemptOfficialModel(model) {
			t.Fatalf("model %q should bypass quota reserve", model)
		}
	}
	for _, model := range []string{"gpt-6.1-sol", "codex-auto-review"} {
		if quotaExemptOfficialModel(model) {
			t.Fatalf("metered model %q must not bypass quota reserve", model)
		}
	}
}

func TestQuotaProtectedOfficialHTTPFallsBackToWopal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct-failover", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	var gotModel, gotAuth, gotAccount string
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
	engine.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		gotModel = strings.TrimSpace(stringOrEmpty(body["model"]))
		gotAuth = c.GetHeader("Authorization")
		gotAccount = c.GetHeader("ChatGPT-Account-ID")
		c.JSON(http.StatusOK, gin.H{"provider": "wopal"})
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewBufferString(`{"model":"gpt-6.1-sol","input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-failover")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("protected request did not fail over: %d %s", resp.StatusCode, body)
	}
	if upstreamHits != 0 {
		t.Fatalf("protected request reached official upstream %d times", upstreamHits)
	}
	if gotModel != "wopal-ai/deepseek-v4.1-flash" {
		t.Fatalf("fallback model = %q", gotModel)
	}
	if gotAuth != "Bearer "+localAccessKey || gotAccount != "" {
		t.Fatalf("official credentials leaked into fallback: auth=%q account=%q", gotAuth, gotAccount)
	}
}

func TestQuotaFailoverWebsocketFrameChecksEverySamplingStep(t *testing.T) {
	now := time.Now()
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct-step", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	for _, messageType := range []string{"response.create", "response.append"} {
		raw := []byte(`{"type":"` + messageType + `","model":"gpt-6.1-sol","input":[]}`)
		rewritten, failback, changed := rewriteQuotaFailoverWebsocketFrame(raw, guard, "acct-step", "gpt-6.1-sol", now)
		if failback || !changed {
			t.Fatalf("%s failover decision = failback:%v changed:%v", messageType, failback, changed)
		}
		if !bytes.Contains(rewritten, []byte(`"model":"wopal-ai/deepseek-v4.1-flash"`)) {
			t.Fatalf("%s was not rewritten to fallback: %s", messageType, rewritten)
		}
	}

	guard.observeRateLimitPayload("acct-step", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":3600}}}`), now.Add(time.Second))
	raw := []byte(`{"type":"response.append","model":"gpt-6.1-sol","input":[]}`)
	_, failback, changed := rewriteQuotaFailoverWebsocketFrame(raw, guard, "acct-step", "gpt-6.1-sol", now.Add(time.Second))
	if !failback || changed {
		t.Fatalf("quota reset decision = failback:%v changed:%v", failback, changed)
	}
}

func TestProtectedOfficialWebsocketReconnectUsesWopalAndFailsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := newOfficialQuotaGuard(nil)
	guard.observeRateLimitPayload("acct-reconnect", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

	officialConnections := 0
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		officialConnections++
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/backend-api/codex")

	wopalModels := make(chan string, 2)
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
	engine.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		wopalModels <- strings.TrimSpace(stringOrEmpty(body["model"]))
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"wopal-result\",\"status\":\"completed\",\"output\":[]}}\n\n"))
		c.Writer.Flush()
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-reconnect")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if response != nil {
			t.Fatalf("protected reconnect returned HTTP %d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if officialConnections != 0 {
		t.Fatalf("protected reconnect opened %d official connections", officialConnections)
	}

	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-6.1-sol", "input": []any{map[string]any{"type": "message", "role": "user", "content": "full replay"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "response.completed" {
			break
		}
	}
	select {
	case model := <-wopalModels:
		if model != "wopal-ai/deepseek-v4.1-flash" {
			t.Fatalf("fallback model = %q", model)
		}
	case <-time.After(time.Second):
		t.Fatal("protected reconnect did not reach Wopal")
	}

	guard.observeRateLimitPayload("acct-reconnect", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
	if err := conn.WriteJSON(map[string]any{"type": "response.append", "model": "gpt-6.1-sol", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("quota failback close = %T %v", err, err)
	}
	if !strings.Contains(closeErr.Text, "quota recovered") {
		t.Fatalf("unexpected failback reason %q", closeErr.Text)
	}
}

func TestOfficialQuotaGuardDoesNotLowerThresholdForLongObservationGaps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := newOfficialQuotaGuard(nil)
	observe := func(percent float64, offset time.Duration) {
		guard.observeWindow("acct", officialQuotaWindow{
			UsedPercent: percent, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: resetAt, ObservedAt: now.Add(offset),
		})
	}

	// A gap wider than the sampling window can span many turns or an idle
	// period. Its delta is not evidence about a single step.
	observe(39, 0)
	observe(65, 5*time.Minute)
	if blocked, _ := guard.blocked("acct", now); blocked {
		t.Fatal("guard lowered the threshold after an accumulated observation gap")
	}
	observe(96, 6*time.Minute)
	if blocked, _ := guard.blocked("acct", now); blocked {
		t.Fatal("guard stopped below the base 97 percent threshold")
	}
	observe(97, 7*time.Minute)
	if blocked, _ := guard.blocked("acct", now); !blocked {
		t.Fatal("guard did not stop at the base 97 percent threshold")
	}
}

func TestOfficialQuotaGuardLowersThresholdForNearbyStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := newOfficialQuotaGuard(nil)
	observe := func(percent float64, offset time.Duration) {
		guard.observeWindow("acct", officialQuotaWindow{
			UsedPercent: percent, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: resetAt, ObservedAt: now.Add(offset),
		})
	}

	observe(90, 0)
	observe(93, 10*time.Second)
	if step, _ := guard.status(now)["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("observed max step = %v, want 3", step)
	}
	if effective, _ := guard.status(now)["effective_five_hour_stop_percent"].(float64); effective != 96 {
		t.Fatalf("effective stop = %v, want 96", effective)
	}
	if blocked, _ := guard.blocked("acct", now); blocked {
		t.Fatal("guard stopped below the learned 96 percent threshold")
	}
	observe(96, 20*time.Second)
	if blocked, _ := guard.blocked("acct", now); !blocked {
		t.Fatal("guard did not stop at the learned 96 percent threshold")
	}
}

func TestOfficialQuotaGuardDiscardsOverCapLearnedStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	path := filepath.Join(t.TempDir(), "quota.json")
	legacy := map[string]any{
		"version": 1,
		"accounts": map[string]any{
			quotaAccountKey("acct"): map[string]any{
				"FiveHour": map[string]any{
					"UsedPercent": 96, "WindowMinutes": 300, "ResetAt": resetAt, "ObservedAt": now,
				},
				"five_hour_max_step": 26,
				"weekly_max_step":    4,
			},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	guard := newOfficialQuotaGuardWithState(nil, path, now)
	if step, _ := guard.status(now)["observed_five_hour_max_step"].(float64); step != 0 {
		t.Fatalf("over-cap learned step survived load: %v", step)
	}
	if blocked, _ := guard.blocked("acct", now); blocked {
		t.Fatal("guard stopped below the base threshold after loading an over-cap step")
	}
}

func TestOfficialQuotaGuardKeepsLearnedStepAcrossWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := newOfficialQuotaGuard(nil)
	guard.observeWindow("acct", officialQuotaWindow{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now,
	})
	guard.observeWindow("acct", officialQuotaWindow{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now.Add(10 * time.Second),
	})

	// The next window starts from scratch, but what a step costs is still known.
	nextReset := resetAt.Add(5 * time.Hour)
	guard.observeWindow("acct", officialQuotaWindow{
		UsedPercent: 0, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: nextReset, ObservedAt: now.Add(3 * time.Hour),
	})
	if step, _ := guard.status(now.Add(3 * time.Hour))["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("learned step lost at window boundary: %v", step)
	}
}
