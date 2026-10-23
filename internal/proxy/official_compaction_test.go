package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"wopal-codex-gateway/internal/compaction"
)

func TestOfficialProxyExpandsWopalCompactionCapsuleEndToEnd(t *testing.T) {
	compaction.ConfigureKey("unit-test-secret")
	seenBody := ""
	seenEncoding := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		seenBody = string(body)
		seenEncoding = r.Header.Get("Content-Encoding")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(nil, target, target))
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	item := testWopalCompactionItem(t, "cmp_wopal_e2e-1", "remember E2E-42")
	requestBody, err := json.Marshal(map[string]any{
		"model": "codex-auto-review",
		"input": []any{item, map[string]any{"type": "message", "role": "user", "content": "review this"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Codex-Routing-Hint", "model=codex-auto-review")
	request.Header.Set("ChatGPT-Account-ID", "acct-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if seenEncoding != "" {
		t.Fatalf("upstream content-encoding = %q, want empty", seenEncoding)
	}
	if strings.Contains(seenBody, "wgcp1.") {
		t.Fatalf("upstream received Wopal capsule: %s", seenBody)
	}
	if !strings.Contains(seenBody, "remember E2E-42") {
		t.Fatalf("upstream body missing expanded summary: %s", seenBody)
	}

	plainBody := `{"model":"codex-auto-review","input":[{"type":"message","role":"user","content":"no capsules"}]}`
	request2, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/responses", strings.NewReader(plainBody))
	if err != nil {
		t.Fatal(err)
	}
	request2.Header.Set("X-Codex-Routing-Hint", "model=codex-auto-review")
	response2, err := http.DefaultClient.Do(request2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response2.Body)
	response2.Body.Close()
	if seenBody != plainBody {
		t.Fatalf("plain official body was modified: %q", seenBody)
	}
}
