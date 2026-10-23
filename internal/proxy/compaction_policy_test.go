package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/routing"
)

func TestOfficialCompactionRequestsUseDedicatedWopalCompactor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		input []any
	}{
		{
			name:  "compact endpoint",
			path:  "/v1/responses/compact",
			input: []any{map[string]any{"type": "message", "role": "user", "content": "HTTP-COMPACT-TOKEN"}},
		},
		{
			name: "responses v2 trigger",
			path: "/v1/responses",
			input: []any{
				map[string]any{"type": "message", "role": "user", "content": "HTTP-COMPACT-TOKEN"},
				map[string]any{"type": "compaction_trigger"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compaction.ConfigureKey("dedicated-compactor-test")
			var officialHits atomic.Int32
			official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				officialHits.Add(1)
				w.WriteHeader(http.StatusTeapot)
			}))
			defer official.Close()
			officialURL, _ := url.Parse(official.URL)

			var compactorModels []string
			engine := ginTestEngine()
			engine.Use(routingMiddlewareWithTargets(nil, officialURL, officialURL))
			engine.POST("/v1/responses", func(c *gin.Context) {
				var request map[string]any
				if err := c.ShouldBindJSON(&request); err != nil {
					t.Error(err)
					return
				}
				compactorModels = append(compactorModels, strings.TrimSpace(fmt.Sprint(request["model"])))
				c.Header("Content-Type", "text/event-stream")
				fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"summary\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Portable HTTP-COMPACT-TOKEN summary\"}]}]}}\n\n")
			})
			server := httptest.NewServer(engine)
			defer server.Close()

			raw, _ := json.Marshal(map[string]any{"model": "gpt-6.1-sol", "input": tc.input, "stream": true})
			resp, err := http.Post(server.URL+tc.path, "application/json", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("compaction status=%d body=%s", resp.StatusCode, body)
			}
			if officialHits.Load() != 0 {
				t.Fatalf("official upstream received %d compaction requests", officialHits.Load())
			}
			if len(compactorModels) != 1 || compactorModels[0] != compaction.DedicatedModel {
				t.Fatalf("compactor models = %#v", compactorModels)
			}

			var completed map[string]any
			if tc.path == "/v1/responses/compact" {
				if err := json.Unmarshal(body, &completed); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := routing.ReadSSE(bytes.NewReader(body), func(data []byte) error {
					var event map[string]any
					_ = json.Unmarshal(data, &event)
					if event["type"] == "response.completed" {
						completed, _ = event["response"].(map[string]any)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			output, _ := completed["output"].([]any)
			if len(output) != 1 {
				t.Fatalf("compaction output = %s", body)
			}
			item, _ := output[0].(map[string]any)
			capsule, err := compaction.OpenCapsule(compaction.EncryptedContent(item))
			if err != nil {
				t.Fatal(err)
			}
			if capsule.Model != compaction.DedicatedModel {
				t.Fatalf("capsule model = %q", capsule.Model)
			}
		})
	}
}

func ginTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestOfficialWebsocketCompactionTriggerUsesDedicatedWopalCompactor(t *testing.T) {
	compaction.ConfigureKey("official-ws-dedicated-compactor-test")
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	officialFrames := make(chan []byte, 2)
	officialConnected := make(chan struct{}, 1)
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		officialConnected <- struct{}{}
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			officialFrames <- payload
		}
	}))
	defer official.Close()
	officialURL, _ := url.Parse(official.URL + "/backend-api/codex")

	compactorModels := make(chan string, 1)
	compactorInputs := make(chan string, 1)
	engine := ginTestEngine()
	engine.Use(routingMiddlewareWithTargets(nil, officialURL, officialURL))
	engine.POST("/v1/responses", func(c *gin.Context) {
		var request map[string]any
		if err := c.ShouldBindJSON(&request); err != nil {
			t.Error(err)
			return
		}
		compactorModels <- strings.TrimSpace(fmt.Sprint(request["model"]))
		input, _ := json.Marshal(request["input"])
		compactorInputs <- string(input)
		c.Header("Content-Type", "text/event-stream")
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"summary-ws\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Portable WS-COMPACT-TOKEN summary\"}]}]}}\n\n")
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	header := http.Header{}
	header.Set("ChatGPT-Account-ID", "acct-ws-compact")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-officialConnected:
	case <-time.After(time.Second):
		t.Fatal("official websocket did not connect")
	}

	// Codex normally sends only the trigger on a reused official socket. The
	// compactor cannot resolve that provider-owned response ID; retire the
	// connection so Codex replays the full history instead of reporting empty.
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-6.1-sol",
		"previous_response_id": "resp_official_previous",
		"input":                []any{map[string]any{"type": "compaction_trigger"}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, payload, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("incremental compaction must request full replay, got payload=%s err=%v", payload, err)
	}
	select {
	case <-compactorModels:
		t.Fatal("incremental compaction summarized without conversation history")
	default:
	}
	conn.Close()
	conn, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-6.1-sol", "input": []any{
			map[string]any{"type": "message", "role": "user", "content": "WS-COMPACT-TOKEN"},
			map[string]any{"type": "compaction_trigger"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var completed map[string]any
	for i := 0; i < 4; i++ {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read compaction event: %v", err)
		}
		var event map[string]any
		_ = json.Unmarshal(payload, &event)
		if event["type"] == "response.completed" {
			completed, _ = event["response"].(map[string]any)
			break
		}
	}
	if completed == nil {
		t.Fatal("websocket compaction did not complete")
	}
	select {
	case got := <-compactorModels:
		if got != compaction.DedicatedModel {
			t.Fatalf("websocket compactor model = %q", got)
		}
	default:
		t.Fatal("dedicated compactor was not called")
	}
	if input := <-compactorInputs; !strings.Contains(input, "WS-COMPACT-TOKEN") || strings.Contains(input, "compaction_trigger") {
		t.Fatalf("compactor did not receive replayed history: %s", input)
	}
	select {
	case payload := <-officialFrames:
		t.Fatalf("compaction trigger leaked to official upstream: %s", payload)
	case <-time.After(150 * time.Millisecond):
	}
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("websocket compaction output = %#v", completed["output"])
	}
	item, _ := output[0].(map[string]any)
	capsule, err := compaction.OpenCapsule(compaction.EncryptedContent(item))
	if err != nil {
		t.Fatal(err)
	}
	if capsule.Model != compaction.DedicatedModel {
		t.Fatalf("websocket capsule model = %q", capsule.Model)
	}
	// After compacting, the official model must receive both the preserved
	// summary and the new user message, without a gateway-local response ID.
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-6.1-sol",
		"previous_response_id": completed["id"],
		"input":                []any{item, map[string]any{"type": "message", "role": "user", "content": "continue officially"}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-officialFrames:
		if !strings.Contains(string(payload), "WS-COMPACT-TOKEN") || !strings.Contains(string(payload), "continue officially") || strings.Contains(string(payload), "previous_response_id") {
			t.Fatalf("official continuation lost compacted history or leaked local ID: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("official continuation never reached upstream")
	}
}
