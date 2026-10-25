package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/clientfp"
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
)

func TestWopalWebsocketBridgeKeepsSocketOpenAfterUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger, err := activity.New(t.TempDir(), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	_ = filepath.Separator

	var posts atomic.Int32
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		serveWopalWebsocketBridge(c, logger, "wopal-ai/claude-haiku-5.5")
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		if posts.Add(1) == 1 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "new_api_error", "code": "get_channel_failed", "message": "all channels exhausted"}})
			return
		}
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\"}}\n\n"))
		_, _ = c.Writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\"}}\n\n"))
	})
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	sendCreate := func(text string) {
		t.Helper()
		payload := map[string]any{"type": "response.create", "model": "wopal-ai/claude-haiku-5.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": text}}}
		if err := conn.WriteJSON(payload); err != nil {
			t.Fatal(err)
		}
	}
	readType := func() string {
		t.Helper()
		_, raw, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read websocket event: %v", errRead)
		}
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("decode websocket event: %v: %s", err, raw)
		}
		typeName, _ := event["type"].(string)
		return typeName
	}

	sendCreate("first")
	if got := readType(); got != "error" {
		t.Fatalf("first event = %q, want error", got)
	}

	sendCreate("second")
	seenCompleted := false
	for i := 0; i < 4; i++ {
		if got := readType(); got == "response.completed" {
			seenCompleted = true
			break
		}
	}
	if !seenCompleted {
		t.Fatal("second turn did not complete on the same websocket")
	}

	sendCreate("third")
	seenCompleted = false
	for i := 0; i < 4; i++ {
		if got := readType(); got == "response.completed" {
			seenCompleted = true
			break
		}
	}
	if !seenCompleted {
		t.Fatal("third turn did not start immediately after the second completion")
	}
}

func TestWopalWebsocketBridgeExpandsIncrementalHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger, err := activity.New(t.TempDir(), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}

	var posts atomic.Int32
	var secondBody map[string]any
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) { serveWopalWebsocketBridge(c, logger, "wopal-ai/deepseek-v4-flash") })
	router.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		call := posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n"))
			_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"a1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"first-assistant\"}]}]}}\n\n"))
			return
		}
		secondBody = body
		_, _ = c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\"}}\n\n"))
		_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	})
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	first := map[string]any{"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "input": []any{map[string]any{"type": "message", "id": "u1", "role": "user", "content": "first-user"}}}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] == "response.completed" {
			break
		}
	}
	second := map[string]any{"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": "r1", "input": []any{map[string]any{"type": "message", "id": "u2", "role": "user", "content": "second-user"}}}
	if err := conn.WriteJSON(second); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] == "response.completed" {
			break
		}
	}

	if secondBody == nil {
		t.Fatal("second upstream request was not captured")
	}
	if _, exists := secondBody["previous_response_id"]; exists {
		t.Fatalf("previous_response_id leaked to chat-backed HTTP request: %#v", secondBody)
	}
	input, ok := secondBody["input"].([]any)
	if !ok {
		t.Fatalf("second input type = %T", secondBody["input"])
	}
	if len(input) != 3 {
		t.Fatalf("merged input len = %d, want 3: %#v", len(input), input)
	}
	ids := []string{}
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok {
			ids = append(ids, strings.TrimSpace(fmt.Sprint(item["id"])))
		}
	}
	if strings.Join(ids, ",") != "u1,a1,u2" {
		t.Fatalf("merged ids = %v, want [u1 a1 u2]", ids)
	}
}

func TestWopalWebsocketBridgeCarriesPrewarmInputIntoNextTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger, err := activity.New(t.TempDir(), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	var posted map[string]any
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) { serveWopalWebsocketBridge(c, logger, "wopal-ai/deepseek-v4-flash") })
	router.POST("/v1/responses", func(c *gin.Context) {
		if err := c.ShouldBindJSON(&posted); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r2\"}}\n\n"))
		_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	})
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	prewarm := map[string]any{"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "generate": false, "input": []any{map[string]any{"type": "message", "id": "warm", "role": "user", "content": "warmup"}}}
	if err := conn.WriteJSON(prewarm); err != nil {
		t.Fatal(err)
	}
	prewarmID := ""
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] == "response.completed" {
			if r, ok := e["response"].(map[string]any); ok {
				prewarmID, _ = r["id"].(string)
			}
			break
		}
	}
	if prewarmID == "" {
		t.Fatal("prewarm response id is empty")
	}
	next := map[string]any{"type": "response.create", "model": "wopal-ai/deepseek-v4-flash", "previous_response_id": prewarmID, "input": []any{map[string]any{"type": "message", "id": "next", "role": "user", "content": "next"}}}
	if err := conn.WriteJSON(next); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] == "response.completed" {
			break
		}
	}
	input, ok := posted["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("prewarm merged input = %#v", posted["input"])
	}
}

// The Wopal websocket bridge is a client of the same gateway over HTTP. That
// internal hop must not be mistaken for a new Codex build or a contract change:
// it carries the original client's non-secret fingerprint headers, but only the
// outer websocket request may enter the compatibility watcher.
func TestWopalWebsocketSelfLoopKeepsClientContractWatcherOnOriginalRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	compaction.ConfigureKey("self-loop-watcher-test")
	var posts atomic.Int32
	loopHeaders := make(chan http.Header, 4)
	engine := ginTestEngine()
	engine.Use(routingMiddlewareWithTargets(logger, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1")))
	engine.GET("/v1/responses", func(c *gin.Context) {
		if c.GetHeader("X-Codex-Routing-Hint") == "" {
			t.Error("websocket reached the route handler without a model hint")
		}
		c.Status(http.StatusInternalServerError)
	})
	engine.POST("/v1/responses", func(c *gin.Context) {
		loopHeaders <- c.Request.Header.Clone()
		if c.GetHeader("Authorization") != "Bearer "+config.LocalAccessKey || c.GetHeader("ChatGPT-Account-ID") != "" {
			t.Errorf("self-loop credential boundary changed: auth=%q account=%q", c.GetHeader("Authorization"), c.GetHeader("ChatGPT-Account-ID"))
		}
		call := posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		fmt.Fprintf(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r%d\",\"status\":\"completed\",\"output\":[]}}\n\n", call)
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	header := http.Header{}
	header.Set("version", "0.166.0-alpha.1")
	header.Set("originator", "Codex Desktop")
	header.Set("User-Agent", "Codex Desktop/0.166.0-alpha.1")
	header.Set("x-codex-beta-features", "remote_compaction_v2")
	header.Set("X-Codex-Routing-Hint", "model=wopal-ai/test")
	header.Set(selfLoopTokenHeader, "spoofed-shape-without-nonce")
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		if response != nil {
			t.Fatalf("dial status=%d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	sendAndRead := func(value map[string]any) {
		t.Helper()
		if err := conn.WriteJSON(value); err != nil {
			t.Fatal(err)
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			_ = json.Unmarshal(raw, &event)
			if event["type"] == "error" {
				t.Fatalf("unexpected websocket error: %s", raw)
			}
			if event["type"] == "response.completed" {
				return
			}
		}
	}

	// Two original frames with the same tool surface must not trigger drift.
	// The generated HTTP request has an id-less tool envelope and must not be
	// mistaken for a client change either.
	first := map[string]any{
		"type": "response.create", "model": "wopal-ai/test",
		"input": []any{
			testWopalAdditionalTools(),
			map[string]any{"type": "message", "id": "u1", "role": "user", "content": "first"},
		},
	}
	sendAndRead(first)
	second := map[string]any{"type": "response.create", "model": "wopal-ai/test",
		"input": []any{
			testWopalAdditionalTools(),
			map[string]any{"type": "message", "id": "u2", "role": "user", "content": "second"},
		},
	}
	sendAndRead(second)

	var internalHeader http.Header
	select {
	case internalHeader = <-loopHeaders:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the self-loop request snapshot")
	}
	if internalHeader.Get("version") != "0.166.0-alpha.1" || internalHeader.Get("originator") != "Codex Desktop" || internalHeader.Get("x-codex-beta-features") != "remote_compaction_v2" {
		t.Fatalf("self-loop lost whitelisted fingerprint headers: %#v", internalHeader)
	}
	if strings.Contains(internalHeader.Get("User-Agent"), "Go-http-client") {
		t.Fatalf("self-loop downgraded User-Agent: %q", internalHeader.Get("User-Agent"))
	}

	file, err := os.Open(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	foundExternal := false
	changed := 0
	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry["event"] == "client_request_contract_changed" {
			changed++
		}
		if entry["event"] == "route_selected" && entry["client_version"] == "0.166.0-alpha.1" {
			foundExternal = true
		}
	}
	if !foundExternal {
		t.Fatal("original websocket request was not recorded as an external client")
	}
	if changed != 0 {
		t.Fatalf("self-loop generated %d contract-change entries", changed)
	}

	// A genuine external shape change is still observable. A watcher that was
	// simply disabled would fail this second check. The first frame has tools at
	// the top level even though its surface is the same; this flips to code mode
	// only on the next frame.
	direct := map[string]any{
		"type": "response.create", "model": "wopal-ai/test",
		"tools": []any{map[string]any{"type": "function", "name": "exec_command"}},
		"input": []any{map[string]any{"type": "message", "id": "u3", "role": "user", "content": "third"}},
	}
	sendAndRead(direct)
	// logger.Close flushes the writer before the second read.
	logger.Close()
	file.Close()
	file, err = os.Open(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner = bufio.NewScanner(file)
	foundDrift := false
	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry["event"] == "client_request_contract_changed" && entry["client_version"] == "0.166.0-alpha.1" {
			foundDrift = true
		}
	}
	if !foundDrift {
		t.Fatalf("a genuine external tool-surface change was not reported; log=%s", readLogText(t, filepath.Join(root, "logs", "gateway.log")))
	}
}

// Official quota failover uses the Wopal websocket bridge, so it creates the
// same loopback HTTP request. Its original handshake still owns the client
// contract; the internally generated request must not impersonate a new client
// or produce a contract-change entry.
func TestOfficialQuotaFailoverWebsocketSelfLoopKeepsOriginalFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	guard := quota.NewGuard(logger)
	guard.ObserveRateLimitPayload("acct-failover", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
	loopHeaders := make(chan http.Header, 1)
	engine := ginTestEngine()
	engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(logger, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1"), guard))
	engine.POST("/v1/responses", func(c *gin.Context) {
		loopHeaders <- c.Request.Header.Clone()
		c.Header("Content-Type", "text/event-stream")
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-failover")
	header.Set("version", "0.166.0-alpha.2")
	header.Set("originator", "codex_exec")
	header.Set("User-Agent", "Codex CLI/0.166.0-alpha.2")
	header.Set("x-codex-beta-features", "remote_compaction_v2")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-6.1-sol", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		if event["type"] == "response.completed" {
			break
		}
	}
	var internal http.Header
	select {
	case internal = <-loopHeaders:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failover self-loop request")
	}
	if internal.Get("version") != "0.166.0-alpha.2" || internal.Get("originator") != "codex_exec" || internal.Get("x-codex-beta-features") != "remote_compaction_v2" {
		t.Fatalf("failover self-loop lost fingerprint: %#v", internal)
	}
	if internal.Get("ChatGPT-Account-ID") != "" || internal.Get("Authorization") != "Bearer "+config.LocalAccessKey {
		t.Fatalf("failover self-loop leaked official credentials: auth=%q account=%q", internal.Get("Authorization"), internal.Get("ChatGPT-Account-ID"))
	}

	entries := readLogText(t, filepath.Join(root, "logs", "gateway.log"))
	if !strings.Contains(entries, `"client_version":"0.166.0-alpha.2"`) {
		t.Fatalf("original failover handshake missing from log: %s", entries)
	}
	if strings.Contains(entries, "client_request_contract_changed") {
		t.Fatalf("failover self-loop polluted contract watcher: %s", entries)
	}
	if strings.Count(entries, `"event":"route_selected"`) == 0 {
		t.Fatalf("original failover route was not recorded: %s", entries)
	}
}

// A compaction turn is generated by the gateway, but it still runs through the
// same Wopal websocket session. It must retain the original non-secret
// fingerprint on its loopback request and must not add a synthetic contract
// event for the compactor's own shape.
func TestWopalCompactionSelfLoopKeepsOriginalFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	compaction.ConfigureKey("compaction-self-loop-test")

	var posts atomic.Int32
	loopHeaders := make(chan http.Header, 4)
	engine := ginTestEngine()
	engine.Use(routingMiddlewareWithTargets(logger, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1")))
	engine.POST("/v1/responses", func(c *gin.Context) {
		loopHeaders <- c.Request.Header.Clone()
		call := posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"compaction summary text\"}\n\n")
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"compaction summary text\"}]}]}}\n\n")
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	header := http.Header{}
	header.Set("version", "0.166.0-alpha.3")
	header.Set("originator", "Codex Desktop")
	header.Set("User-Agent", "Codex Desktop/0.166.0-alpha.3")
	header.Set("x-codex-beta-features", "remote_compaction_v2")
	header.Set("X-Codex-Routing-Hint", "model=wopal-ai/test")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	first := map[string]any{"type": "response.create", "model": "wopal-ai/test", "input": []any{map[string]any{"type": "message", "id": "u1", "role": "user", "content": "remember COMPACTION-LOOP-TOKEN"}}}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		if event["type"] == "response.completed" {
			break
		}
	}
	trigger := map[string]any{"type": "response.create", "model": "wopal-ai/test", "previous_response_id": "r1", "input": []any{map[string]any{"type": "compaction_trigger"}}}
	if err := conn.WriteJSON(trigger); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		if event["type"] == "error" {
			t.Fatalf("compaction failed: %s", raw)
		}
		if event["type"] == "response.completed" {
			break
		}
	}

	for i := 0; i < 2; i++ {
		var internal http.Header
		select {
		case internal = <-loopHeaders:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for compaction self-loop request")
		}
		if internal.Get("version") != "0.166.0-alpha.3" || internal.Get("originator") != "Codex Desktop" || internal.Get("x-codex-beta-features") != "remote_compaction_v2" {
			t.Fatalf("compaction self-loop lost fingerprint: %#v", internal)
		}
		if internal.Get("Authorization") != "Bearer "+config.LocalAccessKey || internal.Get("ChatGPT-Account-ID") != "" {
			t.Fatalf("compaction self-loop credential boundary changed: auth=%q account=%q", internal.Get("Authorization"), internal.Get("ChatGPT-Account-ID"))
		}
	}

	logger.Close()
	entries := readLogText(t, filepath.Join(root, "logs", "gateway.log"))
	if strings.Contains(entries, "client_request_contract_changed") {
		t.Fatalf("compactor request polluted contract watcher: %s", entries)
	}
	if !strings.Contains(entries, `"client_version":"0.166.0-alpha.3"`) {
		t.Fatalf("original compaction handshake missing from log: %s", entries)
	}
}

// Incremental response frames may omit the model. Contract observations must
// use the model already bound to that socket instead of an empty watcher key,
// otherwise two different sessions share one shape history.
func TestWopalWebsocketContractObservationUsesBoundModelForIncrementalFrame(t *testing.T) {
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	watcher := clientfp.NewWatcher(8)
	header := http.Header{}
	header.Set("version", "0.166.0-alpha.4")

	codeMode := clientfp.ShapeFromBody(map[string]any{
		"input": []any{testWopalAdditionalTools()},
	})
	direct := clientfp.ShapeFromBody(map[string]any{
		"tools": []any{map[string]any{"type": "function", "name": "exec_command"}},
	})
	observeWebsocketClientContract(logger, watcher, header, "wopal-ai/model-a", "response.append", codeMode)
	observeWebsocketClientContract(logger, watcher, header, "wopal-ai/model-b", "response.append", direct)
	logger.Write(map[string]any{"event": "probe"})
	logger.Close()

	if log := readLogText(t, filepath.Join(root, "logs", "gateway.log")); strings.Contains(log, "client_request_contract_changed") {
		t.Fatalf("different bound model keys shared an empty-model contract history: %s", log)
	}

	secondRoot := t.TempDir()
	secondLogger, err := activity.New(filepath.Join(secondRoot, "logs"), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	secondWatcher := clientfp.NewWatcher(8)
	observeWebsocketClientContract(secondLogger, secondWatcher, header, "wopal-ai/model-a", "response.create", codeMode)
	observeWebsocketClientContract(secondLogger, secondWatcher, header, "wopal-ai/model-a", "response.append", direct)
	secondLogger.Write(map[string]any{"event": "probe"})
	secondLogger.Close()
	log := readLogText(t, filepath.Join(secondRoot, "logs", "gateway.log"))
	if !strings.Contains(log, `"request_model":"wopal-ai/model-a"`) {
		t.Fatalf("incremental frame did not use the socket model as watcher key: %s", log)
	}
}

func readLogText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
