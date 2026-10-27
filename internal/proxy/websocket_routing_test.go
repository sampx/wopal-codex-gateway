package proxy

import (
	"encoding/json"
	"fmt"
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
	"wopal-codex-gateway/internal/routing"
)

// Model changes must never send a request through the previous provider's
// socket. Codex retries a closed transport with a fresh handshake and full input.
func TestWebsocketModelSwitchReconnectsBeforeForwarding(t *testing.T) {
	for _, initial := range []string{"gpt-6-luna", "wopal-ai/deepseek-v4.1-flash"} {
		for _, prewarm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prewarm=%v", initial, prewarm), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				var officialCalls, wopalCalls atomic.Int32
				var responseNumber atomic.Int32
				completed := func(model string) []byte {
					payload, _ := json.Marshal(map[string]any{
						"type":     "response.completed",
						"response": map[string]any{"id": fmt.Sprintf("r%d", responseNumber.Add(1)), "status": "completed", "model": model, "output": []any{}},
					})
					return payload
				}
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer official-token" || r.Header.Get("ChatGPT-Account-ID") != "acct-1" {
						t.Error("official connection lost its original authentication")
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						var message map[string]any
						if err := conn.ReadJSON(&message); err != nil {
							return
						}
						officialCalls.Add(1)
						model, _ := message["model"].(string)
						if routing.Classify(model) != routing.Official {
							t.Errorf("third-party model leaked to official upstream: %q", model)
						}
						_ = conn.WriteMessage(websocket.TextMessage, completed(model))
					}
				}))
				defer upstream.Close()
				target, _ := url.Parse(upstream.URL + "/backend-api/codex")
				engine := gin.New()
				engine.Use(routingMiddlewareWithTargets(nil, target, target))
				engine.POST("/v1/responses", func(c *gin.Context) {
					wopalCalls.Add(1)
					if c.GetHeader("Authorization") != "Bearer "+config.LocalAccessKey || c.GetHeader("ChatGPT-Account-ID") != "" {
						t.Error("third-party request used official authentication")
					}
					var message map[string]any
					if err := c.ShouldBindJSON(&message); err != nil {
						t.Error(err)
						return
					}
					model, _ := message["model"].(string)
					if routing.Classify(model) != routing.Wopal {
						t.Errorf("official model leaked to third-party upstream: %q", model)
					}
					c.Header("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", completed(model))
				})
				downstream := httptest.NewServer(engine)
				defer downstream.Close()
				dial := func(model string) *websocket.Conn {
					t.Helper()
					header := http.Header{}
					header.Set("Authorization", "Bearer official-token")
					header.Set("ChatGPT-Account-ID", "acct-1")
					header.Set("X-Codex-Routing-Hint", "model="+model)
					conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", header)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
					return conn
				}
				request := func(model string) map[string]any {
					return map[string]any{"type": "response.create", "model": model, "input": []any{map[string]any{"type": "message", "role": "user", "content": "preserved context"}}}
				}
				readCompleted := func(conn *websocket.Conn) {
					t.Helper()
					for {
						var event map[string]any
						if err := conn.ReadJSON(&event); err != nil {
							t.Fatal(err)
						}
						if event["type"] == "response.completed" {
							return
						}
						if event["type"] == "error" {
							t.Fatalf("unexpected error: %#v", event)
						}
					}
				}
				conn := dial(initial)
				if err := conn.WriteJSON(request(initial)); err != nil {
					t.Fatal(err)
				}
				readCompleted(conn)
				next := "wopal-ai/deepseek-v4.1-flash"
				if routing.Classify(initial) == routing.Wopal {
					next = "gpt-6-luna"
				}
				before := officialCalls.Load() + wopalCalls.Load()
				message := request(next)
				if prewarm {
					message["generate"] = false
				}
				if err := conn.WriteJSON(message); err != nil {
					t.Fatal(err)
				}
				_, payload, err := conn.ReadMessage()
				if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
					t.Fatalf("switch should close for reconnect before forwarding; got payload=%s err=%v", payload, err)
				}
				if got := officialCalls.Load() + wopalCalls.Load(); got != before {
					t.Fatalf("switched request reached upstream before reconnect: calls %d -> %d", before, got)
				}
				conn = dial(next)
				if err := conn.WriteJSON(request(next)); err != nil {
					t.Fatal(err)
				}
				readCompleted(conn)
				if officialCalls.Load() != 1 || wopalCalls.Load() != 1 {
					t.Fatalf("unexpected upstream calls: official=%d wopal=%d", officialCalls.Load(), wopalCalls.Load())
				}
			})
		}
	}
}

func TestWebsocketExternalProviderSwitchReconnectsByProviderIdentity(t *testing.T) {
	table := routing.NewTable(map[string]routing.Route{
		"one/model-a":  {Kind: routing.External, ProviderID: "one"},
		"one/model-a2": {Kind: routing.External, ProviderID: "one"},
		"two/model-b":  {Kind: routing.External, ProviderID: "two"},
	}, nil, nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if reconnectForWebsocketRouteChange(conn, "one", payload, nil, table.Resolve) {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"kept"}`))
	}))
	defer server.Close()

	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	same := dial()
	if err := same.WriteJSON(map[string]any{"type": "response.create", "model": "one/model-a2"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := same.ReadMessage()
	_ = same.Close()
	if err != nil || !strings.Contains(string(payload), "kept") {
		t.Fatalf("same-provider message payload=%s err=%v", payload, err)
	}

	switched := dial()
	if err := switched.WriteJSON(map[string]any{"type": "response.create", "model": "two/model-b"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err = switched.ReadMessage()
	_ = switched.Close()
	if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("cross-provider switch payload=%s err=%v, want reconnect close", payload, err)
	}
}

func TestWebsocketRouteChangeFailsClosedWhenCurrentRouteDisappears(t *testing.T) {
	tableA := routing.NewTable(map[string]routing.Route{
		"old-alias": {Kind: routing.External, ProviderID: "one"},
	}, nil, nil)
	tableB := routing.NewTable(nil, nil, nil)
	var current atomic.Pointer[routing.Table]
	current.Store(tableA)
	resolve := func(model string) (routing.Route, error) {
		return current.Load().Resolve(model)
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if reconnectForWebsocketRouteChange(conn, "one", payload, nil, resolve) {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"kept"}`))
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "old-alias"}); err != nil {
		t.Fatal(err)
	}
	if _, payload, err := conn.ReadMessage(); err != nil || !strings.Contains(string(payload), "kept") {
		t.Fatalf("current route payload=%s err=%v", payload, err)
	}

	current.Store(tableB)
	if err := conn.WriteJSON(map[string]any{"type": "response.append", "model": "old-alias"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("deleted route must fail closed, payload=%s err=%v", payload, err)
	}
}
