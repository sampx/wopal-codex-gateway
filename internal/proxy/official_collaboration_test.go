package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestOfficialCollaborationStreamingDoesNotWaitForTurnEnd(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	body := &officialCollaborationBody{ReadCloser: reader, reader: bufio.NewReader(reader), stream: true}
	event := `event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added",` + "\n" + `data: "item":{"type":"function_call","namespace":"collaboration-optimize","name":"spawn_agent"}}` + "\n\n"
	go func() { _, _ = io.WriteString(writer, event) }()
	result := make(chan []byte, 1)
	go func() { p := make([]byte, 4096); n, _ := body.Read(p); result <- p[:n] }()
	select {
	case got := <-result:
		if !bytes.Contains(got, []byte(`"namespace":"collaboration"`)) || !bytes.Contains(got, []byte("event: response.output_item.added")) {
			t.Fatalf("bad event: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compatibility reader buffered until upstream EOF")
	}
	unchanged := []byte(": ping\r\nevent: response.output_text.delta\r\ndata: {\"delta\":\"hello\"}\r\n\r\n")
	if !bytes.Equal(restoreOfficialCollaborationEvent(unchanged), unchanged) {
		t.Fatal("unrelated SSE bytes changed")
	}
}

const collaborationReplay = `{"type":"response.create","model":"gpt-6-luna","stream":true,"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]},{"type":"function_call","namespace":"collaboration","name":"followup_task","call_id":"call1","arguments":"{\"message\":\"collaboration must stay verbatim\"}"},{"type":"function_call_output","call_id":"call1","output":"collaboration must stay verbatim"}]}`

// Model the real server's reserved-schema check, rather than merely asserting
// that encrypted disappeared (which was the test that missed this regression).
func validateOfficialCollaboration(t *testing.T, raw []byte) (string, bool) {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Error(err)
		return "", false
	}
	tools := p["tools"].([]any)
	ns := tools[0].(map[string]any)
	name := ns["name"].(string)
	tool := ns["tools"].([]any)[0].(map[string]any)
	message := tool["parameters"].(map[string]any)["properties"].(map[string]any)["message"].(map[string]any)
	if name == "collaboration" && message["encrypted"] != true {
		return name, false
	}
	if _, present := message["encrypted"]; present {
		t.Error("delegated messages would still be encrypted")
		return name, false
	}
	input := p["input"].([]any)
	additional := input[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	call := input[1].(map[string]any)
	if additional["name"] != name || call["namespace"] != name || !strings.Contains(call["arguments"].(string), "collaboration must stay verbatim") {
		t.Errorf("history/schema mismatch or argument mutation: %s", raw)
		return name, false
	}
	return name, true
}

func TestOfficialCollaborationRoundTrip(t *testing.T) {
	for _, transport := range []string{"websocket", "sse", "sse-text-plain", "json"} {
		t.Run(transport, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var raw []byte
				var conn *websocket.Conn
				if transport == "websocket" {
					var err error
					conn, err = upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					_, raw, err = conn.ReadMessage()
					if err != nil {
						return
					}
				} else {
					raw, _ = io.ReadAll(r.Body)
				}
				name, valid := validateOfficialCollaboration(t, raw)
				if !valid {
					if conn != nil {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"message":"reserved tool must match configured schema"}}`))
					} else {
						http.Error(w, "reserved tool must match configured schema", 400)
					}
					return
				}
				item := map[string]any{"type": "function_call", "namespace": name, "name": "followup_task", "call_id": "call2", "arguments": `{"message":"collaboration must stay verbatim"}`}
				event, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": item})
				if conn != nil {
					_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_probe"}})
					_ = conn.WriteMessage(websocket.TextMessage, event)
					return
				}
				if strings.HasPrefix(transport, "sse") {
					w.Header().Set("Content-Type", "text/event-stream")
					if transport == "sse-text-plain" {
						w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					}
					fmt.Fprintf(w, "event: response.output_item.added\ndata: %s\n\ndata: [DONE]\n\n", event)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"output": []any{item}})
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(routingMiddlewareWithTargets(nil, target, target))
			gateway := httptest.NewServer(engine)
			defer gateway.Close()
			var response []byte
			if transport == "websocket" {
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"X-Codex-Routing-Hint": {"model=gpt-6-luna"}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, []byte(collaborationReplay)); err != nil {
					t.Fatal(err)
				}
				for {
					_, response, err = conn.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					var event map[string]any
					_ = json.Unmarshal(response, &event)
					if event["type"] != "response.created" {
						break
					}
				}
			} else {
				resp, err := http.Post(gateway.URL+"/v1/responses", "application/json", strings.NewReader(collaborationReplay))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				response, _ = io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					t.Fatalf("status=%d body=%s", resp.StatusCode, response)
				}
			}
			if strings.Contains(string(response), `"type":"error"`) || !strings.Contains(string(response), `"namespace":"collaboration"`) || !strings.Contains(string(response), "collaboration must stay verbatim") {
				t.Fatalf("Codex cannot dispatch restored tool call: %s", response)
			}
		})
	}
}
