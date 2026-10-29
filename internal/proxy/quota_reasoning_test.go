package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	translatorbuiltin "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

func quotaFallbackHistoryFixture() map[string]any {
	return map[string]any{
		"model":     "gpt-6.1-sol",
		"reasoning": map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "first"},
			map[string]any{"type": "reasoning", "id": "rs-empty", "encrypted_content": "opaque"},
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "done"},
			map[string]any{"type": "reasoning", "id": "rs-real", "summary": []any{
				map[string]any{"type": "summary_text", "text": "real summary"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}
}

func TestSanitizeQuotaFallbackHistoryKeepsOnlyRealReasoningSummary(t *testing.T) {
	message := quotaFallbackHistoryFixture()
	sanitizeQuotaFallbackHistory(message)

	if _, ok := message["reasoning"]; ok {
		t.Fatalf("quota fallback retained session reasoning flag: %#v", message["reasoning"])
	}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("reasoning unavailable")) || bytes.Contains(raw, []byte("rs-empty")) {
		t.Fatalf("summary-less reasoning survived fallback normalization: %s", raw)
	}
	if !bytes.Contains(raw, []byte("real summary")) {
		t.Fatalf("real reasoning summary was lost: %s", raw)
	}

	input, _ := message["input"].([]any)
	var callIndex, outputIndex = -1, -1
	for index, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item["type"] == "reasoning" {
			t.Fatalf("reasoning item still reaches external translator: %#v", item)
		}
		switch item["type"] {
		case "function_call":
			if item["call_id"] != "call-1" {
				t.Fatalf("tool call identity changed: %#v", item)
			}
			callIndex = index
		case "function_call_output":
			if item["call_id"] != "call-1" {
				t.Fatalf("tool output identity changed: %#v", item)
			}
			outputIndex = index
		}
	}
	if callIndex < 0 || outputIndex <= callIndex {
		t.Fatalf("tool call/output order changed: %#v", input)
	}
}

func TestSanitizedQuotaFallbackHistoryDoesNotProduceTranslatorPlaceholder(t *testing.T) {
	message := quotaFallbackHistoryFixture()
	sanitizeQuotaFallbackHistory(message)
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	// Loading the built-in registry exercises the exact CLIProxyAPI translator
	// used by the embedded service rather than merely checking our intermediate
	// Responses shape.
	_ = translatorbuiltin.Registry()
	translated := sdktranslator.TranslateRequestByFormatName(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatOpenAI,
		"vendor/fallback",
		raw,
		true,
	)
	if len(translated) == 0 {
		t.Fatal("CLIProxyAPI translator returned an empty request")
	}
	if bytes.Contains(translated, []byte("[reasoning unavailable]")) {
		t.Fatalf("synthetic reasoning placeholder reached translated request: %s", translated)
	}
	for _, want := range []string{"real summary", "call-1", "lookup", "done"} {
		if !bytes.Contains(translated, []byte(want)) {
			t.Fatalf("translated fallback lost %q: %s", want, translated)
		}
	}
}

func TestQuotaFailoverWebsocketFrameUsesSameReasoningCompatibility(t *testing.T) {
	now := time.Now()
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-reasoning-ws", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)
	message := quotaFallbackHistoryFixture()
	message["type"] = "response.create"
	raw, _ := json.Marshal(message)

	rewritten, failback, changed := rewriteQuotaFailoverWebsocketFrame(raw, guard, "acct-reasoning-ws", "gpt-6.1-sol", "vendor/fallback", now)
	if failback || !changed {
		t.Fatalf("fallback decision failback=%v changed=%v", failback, changed)
	}
	if bytes.Contains(rewritten, []byte(`"type":"reasoning"`)) || bytes.Contains(rewritten, []byte(`"reasoning":`)) {
		t.Fatalf("websocket fallback retained reasoning that can synthesize a placeholder: %s", rewritten)
	}
	for _, want := range []string{`"model":"vendor/fallback"`, `"call_id":"call-1"`, "real summary"} {
		if !bytes.Contains(rewritten, []byte(want)) {
			t.Fatalf("websocket fallback lost %q: %s", want, rewritten)
		}
	}
}

func TestQuotaFailoverHTTPSanitizesReasoningAfterRoutingRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload("acct-reasoning-http", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	table := routing.NewTable(map[string]routing.Route{
		"vendor/fallback": {Kind: routing.External, ProviderID: "vendor"},
	}, []string{"gpt-6.1-sol"}, nil)
	roles := RuntimeRoles{
		OfficialFallback: RoleTarget{ProviderID: "vendor", ClientModel: "vendor/fallback", Ready: true},
		Compaction:       RoleTarget{ProviderID: "vendor", ClientModel: "vendor/compact", Ready: true},
	}
	seen := make(chan []byte, 1)
	engine := gin.New()
	engine.Use(routingMiddlewareWithRuntime(nil, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1"), guard, table.Resolve, roles))
	engine.POST("/v1/responses", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		seen <- raw
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	server := httptest.NewServer(engine)
	defer server.Close()

	message := quotaFallbackHistoryFixture()
	raw, _ := json.Marshal(message)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ChatGPT-Account-ID", "acct-reasoning-http")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback HTTP status = %d", resp.StatusCode)
	}

	select {
	case forwarded := <-seen:
		text := string(forwarded)
		if strings.Contains(text, `"type":"reasoning"`) || strings.Contains(text, `"reasoning":`) || strings.Contains(text, "rs-empty") {
			t.Fatalf("HTTP fallback retained summary-less reasoning: %s", forwarded)
		}
		for _, want := range []string{`"model":"vendor/fallback"`, `"call_id":"call-1"`, "real summary"} {
			if !strings.Contains(text, want) {
				t.Fatalf("HTTP fallback lost %q: %s", want, forwarded)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("fallback HTTP request was not forwarded")
	}
}
