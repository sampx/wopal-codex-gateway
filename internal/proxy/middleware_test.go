package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

func TestRouteModelNamespaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var officialHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		officialHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	logger, err := activity.New(t.TempDir(), config.Logs{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

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
			if tc.wantStatus == http.StatusNotFound && !strings.Contains(string(body), "is not registered") {
				t.Fatalf("rejection body = %s", body)
			}
		})
	}
}

func TestRoutingMiddlewareUsesExactRouteTableAndFailsBareOnVerifierDrift(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var officialHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		officialHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	logger, err := activity.New(t.TempDir(), config.Logs{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	drifted := false
	table := routing.NewTable(map[string]routing.Route{
		"vendor/model": {Kind: routing.External, ProviderID: "vendor"},
		"short":        {Kind: routing.External, ProviderID: "vendor"},
	}, []string{"gpt-official"}, func() error {
		if drifted {
			return fmt.Errorf("native artifact changed")
		}
		return nil
	})
	target := mustURL(upstream.URL)
	engine := gin.New()
	engine.Use(routingMiddlewareWithResolverAndTargetsAndQuotaGuard(logger, target, target, quota.NewGuard(logger), table.Resolve))
	engine.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	downstream := httptest.NewServer(engine)
	defer downstream.Close()

	post := func(model string) (int, string) {
		t.Helper()
		resp, err := http.Post(downstream.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, _ := post("vendor/model"); status != http.StatusNoContent {
		t.Fatalf("namespaced external status = %d", status)
	}
	if status, _ := post("gpt-official"); status != http.StatusOK || officialHits.Load() != 1 {
		t.Fatalf("official status=%d hits=%d", status, officialHits.Load())
	}
	drifted = true
	for _, model := range []string{"short", "gpt-official"} {
		status, body := post(model)
		if status != http.StatusNotFound || !strings.Contains(body, "native artifact changed") {
			t.Fatalf("drifted bare %q status=%d body=%s", model, status, body)
		}
	}
	if status, _ := post("vendor/model"); status != http.StatusNoContent {
		t.Fatalf("namespaced external after drift status = %d", status)
	}
}

func TestLogRotationKeepsHealthAndStatusRoutes(t *testing.T) {
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	blob := strings.Repeat("x", 600*1024)
	logger.Write(map[string]any{"event": "probe", "payload": blob})
	logger.Write(map[string]any{"event": "probe", "payload": blob})
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
	got, _, err := detectRequestShape(req)
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
	got, _, err := detectRequestShape(req)
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

func waitForRotatedLogs(t *testing.T, dir string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		backups, err := filepath.Glob(filepath.Join(dir, "gateway-*.log*"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := 0
		for _, backup := range backups {
			if strings.HasSuffix(backup, ".gz") {
				compressed++
			}
		}
		if len(backups) == want && compressed == want {
			return backups
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated logs = %v, want %d compressed backups", backups, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
