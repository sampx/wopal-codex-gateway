package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestOfficialWebsocketPassThroughPreservesHeadersPathAndFrames(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	seenPath := ""
	seenAuth := ""
	seenAccount := ""
	seenFrame := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenAccount = r.Header.Get("ChatGPT-Account-ID")
		conn, err := upgrader.Upgrade(w, r, http.Header{"OpenAI-Model": []string{"official-selected"}})
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		seenFrame = string(payload)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1"}}`))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(nil, target, target))
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-1")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if response != nil {
			t.Fatalf("dial failed with status %d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer conn.Close()

	request := `{"type":"response.create","model":"gpt-6.1-sol","input":[]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"response.created", "response.completed"} {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(payload), want) {
			t.Fatalf("frame %q does not contain %q", payload, want)
		}
	}

	if seenPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q", seenPath)
	}
	if seenAuth != "Bearer official-token" {
		t.Fatalf("authorization = %q", seenAuth)
	}
	if seenAccount != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %q", seenAccount)
	}
	if seenFrame != request {
		t.Fatalf("request frame changed: %q", seenFrame)
	}
}
