package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestClassifyModel(t *testing.T) {
	cases := map[string]routeKind{
		"gpt-6.1-sol":                  routeOfficial,
		"wopal-ai/deepseek-v4.1-flash": routeWopal,
		"wopal-ai/x":                   routeWopal,
		"ollama/llama3":                routeDisabled,
		"ollama/qwen3":                 routeDisabled,
		"other/model":                  routeDisabled,
		"":                             routeUnknown,
	}
	for model, want := range cases {
		if got := classifyModel(model); got != want {
			t.Fatalf("classifyModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestRouteModelNamespaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var officialHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		officialHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	logger, err := newActivityLogger(t.TempDir(), logsConfig{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.close()

	engine := gin.New()
	target := mustURL(upstream.URL)
	engine.Use(routingMiddlewareWithTargets(logger, target, target))
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	cases := []struct {
		name       string
		model      string
		wantStatus int
		wantHits   int32
	}{
		{name: "ollama namespace", model: "ollama/llama3", wantStatus: http.StatusNotFound, wantHits: 0},
		{name: "wopal namespace", model: "wopal-ai/x", wantStatus: http.StatusNoContent, wantHits: 0},
		{name: "official model", model: "gpt-6.1-sol", wantStatus: http.StatusOK, wantHits: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := officialHits.Load()
			resp, err := http.Post(
				downstream.URL+"/v1/responses",
				"application/json",
				strings.NewReader(`{"model":"`+tc.model+`","input":"hello"}`),
			)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, body)
			}
			if hits := officialHits.Load() - before; hits != tc.wantHits {
				t.Fatalf("official upstream hits = %d, want %d", hits, tc.wantHits)
			}
			if tc.wantStatus == http.StatusNotFound && !strings.Contains(string(body), "model namespace is disabled") {
				t.Fatalf("rejection body = %s", body)
			}
		})
	}
}

func TestLogRotationKeepsHealthAndStatusRoutes(t *testing.T) {
	root := t.TempDir()
	logger, err := newActivityLogger(root, logsConfig{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.close()

	blob := strings.Repeat("x", 600*1024)
	logger.write(map[string]any{"event": "probe", "payload": blob})
	logger.write(map[string]any{"event": "probe", "payload": blob})
	waitForRotatedLogs(t, filepath.Join(root, "logs"), 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(routingMiddlewareWithTargets(logger, mustURL("http://127.0.0.1:1"), mustURL("http://127.0.0.1:1")))
	engine.GET("/_health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	engine.GET("/_status", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	for _, path := range []string{"/_health", "/_status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d; body = %s", path, recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}
}

func TestDetectRequestModelPrefersRoutingHint(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Codex-Routing-Hint", "model=wopal-ai/glm-5.3-flash;tier=auto")
	got, err := detectRequestModel(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wopal-ai/glm-5.3-flash" {
		t.Fatalf("model = %q", got)
	}
}

func TestDetectRequestModelPreservesCompressedBody(t *testing.T) {
	payload := []byte(`{"model":"wopal-ai/deepseek-v4.1-flash","input":"hi"}`)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll(payload, nil)
	enc.Close()

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "zstd")
	got, err := detectRequestModel(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wopal-ai/deepseek-v4.1-flash" {
		t.Fatalf("model = %q", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, compressed) {
		t.Fatal("routing inspection modified compressed request body")
	}
}

func TestDecodeRoutingBodyGzip(t *testing.T) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(`{"model":"gpt-6.1-sol"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRoutingBody(buffer.Bytes(), "gzip")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "gpt-6.1-sol") {
		t.Fatalf("decoded = %s", decoded)
	}
}
