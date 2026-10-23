package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
)

func TestWopalWebsocketRemoteCompactionV2ClosedLoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configureWopalCompactionKey("bridge-compaction-secret")
	logger, err := activity.New(t.TempDir(), config.Logs{})
	if err != nil {
		t.Fatal(err)
	}

	const model = "wopal-ai/deepseek-v4.1-flash-free"
	var posts atomic.Int32
	var summaryRequest map[string]any
	var firstReplayRequest map[string]any
	var secondReplayRequest map[string]any

	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		serveWopalWebsocketBridge(c, logger, model)
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		call := posts.Add(1)
		c.Header("Content-Type", "text/event-stream")
		switch call {
		case 1:
			_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"a1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"I will remember the token.\"}]}]}}\n\n"))
		case 2:
			summaryRequest = body
			_, _ = c.Writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"The important token is ORCHID-7319.\"}\n\n"))
			_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"summary-r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"summary-a1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"The important token is ORCHID-7319.\"}]}],\"usage\":{\"input_tokens\":100,\"output_tokens\":10,\"total_tokens\":110}}}\n\n"))
		case 3:
			firstReplayRequest = body
			_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r3\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"a3\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ORCHID-7319\"}]}]}}\n\n"))
		case 4:
			secondReplayRequest = body
			_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r4\",\"status\":\"completed\",\"output\":[]}}\n\n"))
		default:
			t.Errorf("unexpected upstream request #%d", call)
			c.Status(http.StatusInternalServerError)
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	readCompleted := func() map[string]any {
		t.Helper()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read websocket event: %v", err)
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatalf("decode websocket event: %v: %s", err, raw)
			}
			if event["type"] == "error" {
				t.Fatalf("unexpected websocket error: %s", raw)
			}
			if event["type"] == "response.completed" {
				return event
			}
		}
	}

	first := map[string]any{
		"type": "response.create", "model": model,
		"input": []any{
			testWopalAdditionalTools(),
			map[string]any{
				"type": "message", "id": "u1", "role": "user",
				"content": "PRECOMPACT_ONLY_MARKER. Remember ORCHID-7319.",
			},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatal(err)
	}
	readCompleted()

	trigger := map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r1",
		"input": []any{map[string]any{"type": "compaction_trigger"}},
	}
	if err := conn.WriteJSON(trigger); err != nil {
		t.Fatal(err)
	}
	compactCompleted := readCompleted()
	response, _ := compactCompleted["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("compaction output len=%d want 1: %#v", len(output), response["output"])
	}
	compactionItem, _ := output[0].(map[string]any)
	if compactionItem["type"] != "compaction" {
		t.Fatalf("compaction output type=%v", compactionItem["type"])
	}
	encrypted, _ := compactionItem["encrypted_content"].(string)
	if !strings.HasPrefix(encrypted, wopalCompactionCapsulePrefix) || strings.Contains(encrypted, "ORCHID-7319") {
		t.Fatalf("invalid opaque compaction payload: %q", encrypted)
	}
	compactionResponseID, _ := response["id"].(string)
	if compactionResponseID == "" {
		t.Fatal("compaction response id is empty")
	}

	replay := map[string]any{
		"type": "response.create", "model": model, "previous_response_id": compactionResponseID,
		"input": []any{
			compactionItem,
			map[string]any{"type": "message", "id": "u3", "role": "user", "content": "What token must we remember?"},
		},
	}
	if err := conn.WriteJSON(replay); err != nil {
		t.Fatal(err)
	}
	readCompleted()

	secondReplay := map[string]any{
		"type": "response.create", "model": model, "previous_response_id": "r3",
		"input": []any{
			compactionItem,
			map[string]any{"type": "message", "id": "u4", "role": "user", "content": "Continue after the compacted turn."},
		},
	}
	if err := conn.WriteJSON(secondReplay); err != nil {
		t.Fatal(err)
	}
	readCompleted()

	if posts.Load() != 4 {
		t.Fatalf("upstream post count=%d want 4", posts.Load())
	}
	summaryJSON, _ := json.Marshal(summaryRequest)
	if strings.Contains(string(summaryJSON), "compaction_trigger") {
		t.Fatalf("compaction trigger leaked to summary model: %s", summaryJSON)
	}
	if !strings.Contains(string(summaryJSON), "PRECOMPACT_ONLY_MARKER") || !strings.Contains(string(summaryJSON), "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("summary request did not contain full history + summary prompt: %s", summaryJSON)
	}
	if _, exists := summaryRequest["tools"]; exists {
		t.Fatalf("summary request should disable top-level tools: %s", summaryJSON)
	}
	if strings.Contains(string(summaryJSON), "additional_tools") || strings.Contains(string(summaryJSON), "open_url") {
		t.Fatalf("summary request should not expose retained browser tools: %s", summaryJSON)
	}
	openedCapsule, err := openWopalCompactionCapsule(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if len(openedCapsule.Prefix) != 1 || !strings.Contains(string(openedCapsule.Prefix[0]), "open_url") {
		t.Fatalf("compaction capsule did not retain browser tools: %#v", openedCapsule.Prefix)
	}

	firstReplayJSON, _ := json.Marshal(firstReplayRequest)
	if strings.Contains(string(firstReplayJSON), "PRECOMPACT_ONLY_MARKER") {
		t.Fatalf("pre-compaction transcript leaked after replay: %s", firstReplayJSON)
	}
	if !strings.Contains(string(firstReplayJSON), "ORCHID-7319") || !strings.Contains(string(firstReplayJSON), `"id":"u3"`) {
		t.Fatalf("replayed summary/tail missing: %s", firstReplayJSON)
	}
	if strings.Count(string(firstReplayJSON), "additional_tools") != 1 || !strings.Contains(string(firstReplayJSON), "open_url") {
		t.Fatalf("retained browser tools missing or duplicated after replay: %s", firstReplayJSON)
	}
	if strings.Contains(string(firstReplayJSON), `"type":"compaction"`) {
		t.Fatalf("opaque compaction item reached chat-backed upstream: %s", firstReplayJSON)
	}

	secondReplayJSON, _ := json.Marshal(secondReplayRequest)
	for _, want := range []string{"ORCHID-7319", `"id":"u3"`, `"id":"a3"`, `"id":"u4"`} {
		if !strings.Contains(string(secondReplayJSON), want) {
			t.Fatalf("post-compaction continuation lost %s: %s", want, secondReplayJSON)
		}
	}
	if strings.Count(string(secondReplayJSON), "wopal_compaction_summary_") != 1 {
		t.Fatalf("compaction summary duplicated on repeated replay: %s", secondReplayJSON)
	}
	if strings.Count(string(secondReplayJSON), "additional_tools") != 1 || !strings.Contains(string(secondReplayJSON), "open_url") {
		t.Fatalf("retained browser tools missing or duplicated on repeated replay: %s", secondReplayJSON)
	}
}
