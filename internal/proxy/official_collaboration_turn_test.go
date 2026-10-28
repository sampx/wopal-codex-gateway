package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func optimizedCollaborationRequest() map[string]any {
	var request map[string]any
	_ = json.Unmarshal([]byte(collaborationReplay), &request)
	return request
}

func TestOfficialCollaborationRejectedCreates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"response.failed", "error"} {
		for _, scenario := range []string{"rejected_with_id", "rejected_without_id", "active_failure", "ambiguous_failure"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				active := scenario == "active_failure" || scenario == "ambiguous_failure"
				ambiguous := scenario == "ambiguous_failure"
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					read := func() bool { _, _, err := conn.ReadMessage(); return err == nil }
					created := func(id string) {
						_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
					}
					if active {
						if !read() {
							return
						}
						created("active")
					}
					if !read() {
						return
					}
					if !active && !read() {
						return
					}
					failure := map[string]any{"type": kind, "error": map[string]any{"code": "invalid_request_error", "message": "synthetic rejected create"}}
					id := "rejected"
					if active {
						id = "active"
					}
					if scenario == "rejected_without_id" || ambiguous {
						id = ""
					}
					if id != "" {
						if kind == "error" {
							failure["response_id"] = id
						} else {
							failure["response"] = map[string]any{"id": id}
						}
					}
					_ = conn.WriteJSON(failure)
					if ambiguous {
						_, _, readErr := conn.ReadMessage()
						if readErr == nil {
							t.Error("ambiguous socket accepted more client data")
						}
						if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
							t.Error("gateway did not close upstream after ambiguous failure")
						}
						return
					}
					created("good")
					_ = conn.WriteJSON(map[string]any{"type": "response.output_item.done", "response_id": "good", "item": map[string]any{"type": "function_call", "namespace": "collaboration-optimize", "name": "followup_task", "arguments": "{}"}})
					_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "good"}})
				}))
				defer upstream.Close()
				target, _ := url.Parse(upstream.URL)
				engine := gin.New()
				engine.Use(routingMiddlewareWithTargets(nil, target, target))
				gateway := httptest.NewServer(engine)
				defer gateway.Close()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"X-Codex-Routing-Hint": {"model=gpt-6-luna"}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if active {
					if err := conn.WriteJSON(optimizedCollaborationRequest()); err != nil {
						t.Fatal(err)
					}
					var event map[string]any
					if err := conn.ReadJSON(&event); err != nil {
						t.Fatal(err)
					}
				}
				if err := conn.WriteJSON(conflictingCollaborationRequest()); err != nil {
					t.Fatal(err)
				}
				if !active {
					if err := conn.WriteJSON(optimizedCollaborationRequest()); err != nil {
						t.Fatal(err)
					}
				}
				var failure map[string]any
				if err := conn.ReadJSON(&failure); err != nil {
					t.Fatal(err)
				}
				if failure["type"] != kind {
					t.Fatalf("native failure was lost: %v", failure)
				}
				if ambiguous {
					_, _, err := conn.ReadMessage()
					if err == nil {
						t.Fatal("ambiguous ownership continued on the socket")
					}
					if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						t.Fatal("socket was not closed after ambiguous failure")
					}
					return
				}
				want := "collaboration"
				if active {
					want = "collaboration-optimize"
				}
				sawTool := false
				for {
					var event map[string]any
					if err := conn.ReadJSON(&event); err != nil {
						t.Fatal(err)
					}
					if item, ok := event["item"].(map[string]any); ok {
						sawTool = true
						if item["namespace"] != want {
							t.Errorf("next accepted response inherited rejected/active mode: namespace=%v want=%s", item["namespace"], want)
						}
					}
					if event["type"] == "response.completed" {
						if !sawTool {
							t.Fatal("next response lost its tool call")
						}
						return
					}
				}
			})
		}
	}
}

func conflictingCollaborationRequest() map[string]any {
	var request map[string]any
	_ = json.Unmarshal([]byte(`{"type":"response.create","model":"gpt-6-luna","tools":[{"type":"namespace","name":"collaboration-optimize","tools":[{"type":"function","name":"followup_task","description":"Client-owned tool"}]}],"input":[]}`), &request)
	return request
}

func incrementalCollaborationRequest(previous int) map[string]any {
	return map[string]any{"type": "response.create", "model": "gpt-6-luna", "previous_response_id": fmt.Sprintf("resp_%d", previous), "input": []any{}}
}

// A later request can already be queued when an earlier response's tool call
// arrives. Scope identities by the corresponding create, not the last request.
func TestOfficialCollaborationWebsocketTurnScopes(t *testing.T) {
	for _, pipelined := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipelined=%t", pipelined), func(t *testing.T) {
			requests := []map[string]any{optimizedCollaborationRequest(), incrementalCollaborationRequest(1), conflictingCollaborationRequest(), incrementalCollaborationRequest(3), optimizedCollaborationRequest(), incrementalCollaborationRequest(5)}
			wantNamespaces := []string{"collaboration", "collaboration", "collaboration-optimize", "collaboration-optimize", "collaboration", "collaboration"}
			if pipelined {
				requests = []map[string]any{optimizedCollaborationRequest(), conflictingCollaborationRequest(), incrementalCollaborationRequest(1)}
				wantNamespaces = []string{"collaboration", "collaboration-optimize", "collaboration"}
			}
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				read := func() bool {
					var p map[string]any
					if conn.ReadJSON(&p) != nil {
						return false
					}
					return true
				}
				emit := func(index int) {
					id := fmt.Sprintf("resp_%d", index+1)
					_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
					_ = conn.WriteJSON(map[string]any{"type": "response.output_item.added", "response_id": id, "item": map[string]any{"type": "function_call", "namespace": "collaboration-optimize", "name": "followup_task", "arguments": "{}"}})
					_ = conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "namespace": "collaboration-optimize", "name": "followup_task", "arguments": "{}"}})
					_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": id}})
				}
				for i := 0; i < len(requests); i++ {
					if !read() {
						return
					}
					if pipelined && i == 0 {
						if !read() {
							return
						}
						emit(0)
						emit(1)
						i++
						continue
					}
					emit(i)
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(routingMiddlewareWithTargets(nil, target, target))
			gateway := httptest.NewServer(engine)
			defer gateway.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"X-Codex-Routing-Hint": {"model=gpt-6-luna"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			readTurn := func(index int) {
				sawTool := false
				for {
					var event map[string]any
					if err := conn.ReadJSON(&event); err != nil {
						t.Fatal(err)
					}
					if item, ok := event["item"].(map[string]any); ok {
						sawTool = true
						if item["namespace"] != wantNamespaces[index] {
							t.Errorf("turn %d namespace=%v want %s", index+1, item["namespace"], wantNamespaces[index])
						}
					}
					if event["type"] == "response.completed" {
						if !sawTool {
							t.Fatal("response lost its tool call")
						}
						return
					}
				}
			}
			for i, p := range requests {
				if err := conn.WriteJSON(p); err != nil {
					t.Fatal(err)
				}
				if pipelined && i == 0 {
					continue
				}
				if pipelined && i == 1 {
					readTurn(0)
				}
				readTurn(i)
			}
		})
	}
}
