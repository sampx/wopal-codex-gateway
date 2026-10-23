package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestWopalWebsocketBridgeKeepsSocketOpenAfterUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger, err := newActivityLogger(t.TempDir())
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
	logger, err := newActivityLogger(t.TempDir())
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
	logger, err := newActivityLogger(t.TempDir())
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
