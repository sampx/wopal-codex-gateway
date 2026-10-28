package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/routing"
)

// Every websocket turn log must name the provider that actually served the
// turn. A fixed "wopal-ai" label makes routing evidence unusable: a request
// that reached an external provider would still look like Wopal AI traffic.
func TestWebsocketTurnLogsNameTheServingProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logsDir := t.TempDir()
	logger, err := activity.New(logsDir, config.Logs{})
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		serveExternalWebsocketBridge(c, logger, "vendor/model", "vendor", func(model string) (routing.Route, error) {
			return routing.Route{Kind: routing.External, ProviderID: "vendor"}, nil
		})
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"))
	})
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	headers := http.Header{
		"Authorization":        {"Bearer " + config.LocalAccessKey},
		"X-Codex-Routing-Hint": {"model=vendor/model"},
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": "vendor/model",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read: %v", errRead)
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		if event["type"] == "response.completed" {
			break
		}
	}
	_ = conn.Close()
	logger.Close()

	data, err := os.ReadFile(filepath.Join(logsDir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	var started, completed string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		switch entry["event"] {
		case "turn_started":
			started, _ = entry["provider"].(string)
		case "turn_completed":
			completed, _ = entry["provider"].(string)
		}
	}
	if started != "vendor" {
		t.Errorf("turn_started provider = %q, want %q", started, "vendor")
	}
	if completed != "vendor" {
		t.Errorf("turn_completed provider = %q, want %q", completed, "vendor")
	}
}
