package proxy

import (
	"encoding/json"
	"fmt"
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
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/routing"
)

// A session can chat on one provider and summarise on another. Compaction
// lifecycle events must name the provider that ran the summary; reporting the
// foreground provider describes a request that never reached that provider.
func TestCompactionLogsNameTheCompactionProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	compaction.ConfigureKey("compaction-provider-secret")
	logsDir := t.TempDir()
	logger, err := activity.New(logsDir, config.Logs{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	const foregroundModel = "vendor/chat"
	const compactionModel = "summarizer/compact"
	target := RoleTarget{ProviderID: "summarizer", ClientModel: compactionModel, Ready: true}
	summary := strings.Repeat("the summary must be long enough to pass validation. ", 3)

	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		serveExternalWebsocketBridgeWithCompaction(c, logger, foregroundModel, "vendor",
			func(string) (routing.Route, error) {
				return routing.Route{Kind: routing.External, ProviderID: "vendor"}, nil
			}, target)
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		delta := map[string]any{"type": "response.output_text.delta", "delta": summary}
		deltaBody, _ := json.Marshal(delta)
		fmt.Fprintf(c.Writer, "data: %s\n\n", deltaBody)
		completed := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "compact-1", "status": "completed",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": summary}},
				}},
			},
		}
		doneBody, _ := json.Marshal(completed)
		fmt.Fprintf(c.Writer, "data: %s\n\n", doneBody)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{
		"Authorization":        {"Bearer " + config.LocalAccessKey},
		"X-Codex-Routing-Hint": {"model=" + foregroundModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	if err := conn.WriteJSON(map[string]any{
		"type": "response.create", "model": foregroundModel,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "compaction_trigger"},
		},
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
		if event["type"] == "error" {
			t.Fatalf("unexpected websocket error: %s", raw)
		}
		if event["type"] == "response.completed" {
			break
		}
	}
	_ = conn.Close()
	logger.Close()

	started, completed := compactionEventProviders(t, filepath.Join(logsDir, "gateway.log"))
	if started != "summarizer" {
		t.Errorf("compaction_started provider = %q, want %q", started, "summarizer")
	}
	if completed != "summarizer" {
		t.Errorf("compaction_completed provider = %q, want %q", completed, "summarizer")
	}
}

// compactionEventProviders returns the provider recorded on the compaction
// lifecycle events so a test can assert the serving provider instead of the
// mere presence of a log line.
func compactionEventProviders(t *testing.T, path string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(path)
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
		case "compaction_started":
			started, _ = entry["provider"].(string)
		case "compaction_completed":
			completed, _ = entry["provider"].(string)
		}
	}
	return started, completed
}
