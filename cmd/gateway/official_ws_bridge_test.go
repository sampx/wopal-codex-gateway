package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestOfficialWebsocketFrameSanitizesWopalItems(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	frames := make(chan string, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			frames <- string(payload)
		}
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

	item := testWopalCompactionItem(t, "cmp_wopal_ws-1", "remember WS-SUMMARY")
	request := map[string]any{
		"type":  "response.create",
		"model": "gpt-6.1-sol",
		"input": []any{item, testWopalReasoningItem(), map[string]any{"type": "reasoning", "id": "rs_cmb-ws_0", "encrypted_content": ""}, map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, messageType := range []int{websocket.TextMessage, websocket.BinaryMessage} {
		if err := conn.WriteMessage(messageType, payload); err != nil {
			t.Fatal(err)
		}

		var frame string
		select {
		case frame = <-frames:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for upstream frame")
		}

		if strings.Contains(frame, wopalCompactionCapsulePrefix) {
			t.Fatalf("upstream frame leaked Wopal capsule: %s", frame)
		}
		if strings.Contains(frame, wopalReasoningIDPrefix) {
			t.Fatalf("upstream frame leaked Wopal reasoning id: %s", frame)
		}
		if strings.Contains(frame, "rs_cmb-") {
			t.Fatalf("upstream frame leaked cmb reasoning: %s", frame)
		}
		if !strings.Contains(frame, "remember WS-SUMMARY") {
			t.Fatalf("upstream frame missing expanded summary: %s", frame)
		}
		if !strings.Contains(frame, `"role":"user"`) {
			t.Fatalf("upstream frame lost the user message: %s", frame)
		}
	}
}

func TestOfficialWebsocketFramePassesNativePayloadUnchanged(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	frames := make(chan string, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frames <- string(payload)
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
	header.Set("ChatGPT-Account-ID", "acct-1")
	header.Set("X-Codex-Routing-Hint", "model=gpt-6.1-sol")
	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	native := `{"type":"response.create","model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":"native"}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(native)); err != nil {
		t.Fatal(err)
	}

	select {
	case frame := <-frames:
		if frame != native {
			t.Fatalf("native frame was modified:\n got %s\nwant %s", frame, native)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream frame")
	}
}

func TestSanitizeOfficialWebsocketFrameExpandsCapsule(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	item := testWopalCompactionItem(t, "cmp_wopal_frame-1", "remember FRAME-77")
	input := []any{item, map[string]any{"type": "message", "role": "user", "content": "hi"}}
	request := map[string]any{"type": "response.create", "model": "gpt-6.1-sol", "input": input}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, ok := sanitizeOfficialWebsocketFrame(payload)
	if !ok {
		t.Fatal("expected frame to be rewritten")
	}
	if strings.Contains(string(rewritten), wopalCompactionCapsulePrefix) {
		t.Fatalf("rewritten frame still contains capsule: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), "remember FRAME-77") {
		t.Fatalf("rewritten frame missing summary: %s", rewritten)
	}
}

func TestSanitizeOfficialWebsocketFrameLeavesNativeFrameUntouched(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	native := `{"type":"response.create","model":"gpt-6.1-sol","input":[{"type":"reasoning","id":"rs_abc_0","encrypted_content":"openai-blob"}]}`
	if _, ok := sanitizeOfficialWebsocketFrame([]byte(native)); ok {
		t.Fatal("native frame should not be rewritten")
	}
}

func TestOfficialWebsocketBridgePreservesUpstreamHandshakeHeader(t *testing.T) {
	configureWopalCompactionKey("unit-test-secret")
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, http.Header{"OpenAI-Model": []string{"official-selected"}})
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
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
	if got := response.Header.Get("OpenAI-Model"); got != "official-selected" {
		t.Fatalf("upstream handshake header not forwarded: OpenAI-Model = %q", got)
	}
}
