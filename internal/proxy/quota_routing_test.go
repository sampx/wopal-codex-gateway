package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

func TestRoutingQuotaGuardFailsOverOfficialAndAllowsExemptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := quota.NewGuard(nil)
	now := time.Now()
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

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

// Official quota protection is the only path allowed to replace an official
// request with officialFallback. These HTTP cases run while the guard is open,
// so returning the native upstream status is the full contract; an external
// handler hit would mean a 429, auth error, or server error became a fallback.
func TestOfficialNonQuotaHTTPFailuresNeverFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("native failure"))
			}))
			defer upstream.Close()

			var fallbackHits atomic.Int32
			engine := gin.New()
			engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, mustURL(upstream.URL), mustURL(upstream.URL), quota.NewGuard(nil)))
			engine.POST("/v1/responses", func(c *gin.Context) {
				fallbackHits.Add(1)
				c.Status(http.StatusNoContent)
			})
			downstream := httptest.NewServer(engine)
			defer downstream.Close()

			req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", bytes.NewBufferString(`{"model":"gpt-6.1-sol","input":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer official-token")
			req.Header.Set("ChatGPT-Account-ID", "acct-native-failure")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != status || string(body) != "native failure" {
				t.Fatalf("native response = %d %q, want %d %q", resp.StatusCode, body, status, "native failure")
			}
			if hits := fallbackHits.Load(); hits != 0 {
				t.Fatalf("non-quota HTTP failure reached external fallback %d times", hits)
			}
		})
	}
}

func TestOfficialConnectionFailureNeverFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := mustURL(unreachable.URL)
	unreachable.Close()

	var fallbackHits atomic.Int32
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, quota.NewGuard(nil)))
	engine.POST("/v1/responses", func(c *gin.Context) {
		fallbackHits.Add(1)
		c.Status(http.StatusNoContent)
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", bytes.NewBufferString(`{"model":"gpt-6.1-sol","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-connection-failure")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("connection failure status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("connection failure reached external fallback %d times", hits)
	}
}

func TestOfficialFailureAfterOutputNeverFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already visible\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n")
	}))
	defer upstream.Close()

	var fallbackHits atomic.Int32
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, mustURL(upstream.URL), mustURL(upstream.URL), quota.NewGuard(nil)))
	engine.POST("/v1/responses", func(c *gin.Context) {
		fallbackHits.Add(1)
		c.Status(http.StatusNoContent)
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", bytes.NewBufferString(`{"model":"gpt-6.1-sol","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer official-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-post-output-failure")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	for _, want := range []string{"already visible", "response.failed"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("native stream missing %q: %s", want, body)
		}
	}
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("failure after output reached external fallback %d times", hits)
	}
}

func TestOfficialWebsocketHandshakeFailureNeverFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "handshake rejected", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}

	var fallbackHits atomic.Int32
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, quota.NewGuard(nil)))
	engine.POST("/v1/responses", func(c *gin.Context) {
		fallbackHits.Add(1)
		c.Status(http.StatusNoContent)
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-ws-handshake-failure")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
	if conn != nil {
		conn.Close()
		t.Fatal("gateway upgraded after upstream handshake failure")
	}
	if err == nil {
		t.Fatal("gateway dial unexpectedly succeeded")
	}
	if response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("handshake failure response = %#v, want 502", response)
	}
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("WebSocket handshake failure reached external fallback %d times", hits)
	}
}
func TestOfficialWebsocketQuotaGuardStopsCurrentSocketBeforeFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := quota.NewGuard(nil)
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

	guard.ObserveRateLimitPayload("acct-ws", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
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
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-luna", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

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
func TestQuotaProtectedOfficialHTTPFallsBackToWopal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-failover", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

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
		gotModel = strings.TrimSpace(routing.StringOrEmpty(body["model"]))
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
	if gotAuth != "Bearer "+config.LocalAccessKey || gotAccount != "" {
		t.Fatalf("official credentials leaked into fallback: auth=%q account=%q", gotAuth, gotAccount)
	}
}

func TestQuotaFailoverWebsocketFrameChecksEverySamplingStep(t *testing.T) {
	now := time.Now()
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-step", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	for _, messageType := range []string{"response.create", "response.append"} {
		raw := []byte(`{"type":"` + messageType + `","model":"gpt-6.1-sol","input":[]}`)
		rewritten, failback, changed := rewriteQuotaFailoverWebsocketFrame(raw, guard, "acct-step", "gpt-6.1-sol", "vendor/fallback", now)
		if failback || !changed {
			t.Fatalf("%s failover decision = failback:%v changed:%v", messageType, failback, changed)
		}
		if !bytes.Contains(rewritten, []byte(`"model":"vendor/fallback"`)) {
			t.Fatalf("%s was not rewritten to fallback: %s", messageType, rewritten)
		}
	}

	guard.ObserveRateLimitPayload("acct-step", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":3600}}}`), now.Add(time.Second))
	raw := []byte(`{"type":"response.append","model":"gpt-6.1-sol","input":[]}`)
	_, failback, changed := rewriteQuotaFailoverWebsocketFrame(raw, guard, "acct-step", "gpt-6.1-sol", "vendor/fallback", now.Add(time.Second))
	if !failback || changed {
		t.Fatalf("quota reset decision = failback:%v changed:%v", failback, changed)
	}
}

func TestProtectedOfficialWebsocketUsesConfiguredExternalFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-external", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

	table := routing.NewTable(map[string]routing.Route{
		"vendor/fallback": {Kind: routing.External, ProviderID: "vendor"},
	}, []string{"gpt-6.1-sol"}, nil)
	roles := RuntimeRoles{
		OfficialFallback: RoleTarget{ProviderID: "vendor", ClientModel: "vendor/fallback", Ready: true},
		Compaction:       RoleTarget{ProviderID: "wopal-ai", ClientModel: "wopal-ai/deepseek-v4.1-flash", Ready: true},
	}
	models := make(chan string, 1)
	engine := gin.New()
	engine.Use(routingMiddlewareWithRuntime(nil, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1"), guard, table.Resolve, roles))
	engine.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		models <- strings.TrimSpace(routing.StringOrEmpty(body["model"]))
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fallback-result\",\"status\":\"completed\",\"output\":[]}}\n\n"))
		c.Writer.Flush()
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-external")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-6.1-sol", "input": []any{map[string]any{"type": "message", "role": "user", "content": "full replay"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("configured fallback did not serve the turn: %v", err)
		}
		if event["type"] == "response.completed" {
			break
		}
	}
	select {
	case model := <-models:
		if model != "vendor/fallback" {
			t.Fatalf("fallback model = %q", model)
		}
	case <-time.After(time.Second):
		t.Fatal("configured fallback did not receive the turn")
	}
}

func TestProtectedOfficialWebsocketReconnectUsesWopalAndFailsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-reconnect", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())

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
		wopalModels <- strings.TrimSpace(routing.StringOrEmpty(body["model"]))
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

	guard.ObserveRateLimitPayload("acct-reconnect", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
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
