package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/proxy"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

func TestBackendSwitcherDrainsOldSnapshotAfterInflightRequestCompletes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	oldStopped := make(chan struct{})
	old := newRuntimeBackend(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), func() { close(oldStopped) })
	next := newRuntimeBackend(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}), nil)
	switcher := newBackendSwitcher(old)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		recorder := httptest.NewRecorder()
		switcher.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if recorder.Code != http.StatusNoContent {
			t.Errorf("old request status = %d", recorder.Code)
		}
	}()
	<-entered

	switcher.Swap(next)
	select {
	case <-oldStopped:
		t.Fatal("old snapshot stopped while request was still in flight")
	default:
	}
	recorder := httptest.NewRecorder()
	switcher.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("new request status = %d, want %d", recorder.Code, http.StatusAccepted)
	}

	close(release)
	wg.Wait()
	select {
	case <-oldStopped:
	case <-time.After(time.Second):
		t.Fatal("old snapshot was not stopped after its final request completed")
	}
}

func TestBackendSwitcherResolveUsesCurrentSnapshot(t *testing.T) {
	oldRoutes := routing.NewTable(map[string]routing.Route{
		"old-alias": {Kind: routing.External, ProviderID: "vendor"},
	}, nil, nil)
	nextRoutes := routing.NewTable(nil, nil, nil)
	old := newRuntimeBackend(http.NotFoundHandler(), nil)
	old.routes = oldRoutes
	next := newRuntimeBackend(http.NotFoundHandler(), nil)
	next.routes = nextRoutes
	switcher := newBackendSwitcher(old)

	if route, err := switcher.Resolve("old-alias"); err != nil || route.ProviderID != "vendor" {
		t.Fatalf("old snapshot route = %#v, %v", route, err)
	}
	switcher.Swap(next)
	if _, err := switcher.Resolve("old-alias"); err == nil {
		t.Fatal("route resolver kept using the previous snapshot after swap")
	}
}

func TestEstablishedWebsocketUsesCurrentSnapshotAfterSwap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tableA := routing.NewTable(map[string]routing.Route{
		"old-alias": {Kind: routing.External, ProviderID: "vendor"},
	}, nil, nil)
	tableB := routing.NewTable(nil, nil, nil)
	switcher := newBackendSwitcher(nil)
	engine := gin.New()
	engine.Use(proxy.RoutingMiddlewareWithResolverAndRoles(nil, quota.NewGuard(nil), switcher.Resolve, proxy.RuntimeRoles{}))
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/event-stream", []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"one\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	})
	oldBackend := newRuntimeBackend(engine, nil)
	oldBackend.routes = tableA
	nextBackend := newRuntimeBackend(http.NotFoundHandler(), nil)
	nextBackend.routes = tableB
	switcher.Swap(oldBackend)

	server := httptest.NewServer(switcher)
	defer server.Close()
	header := http.Header{}
	header.Set("Authorization", "Bearer official-token")
	header.Set("ChatGPT-Account-ID", "acct-reload")
	header.Set("X-Codex-Routing-Hint", "model=old-alias")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "old-alias", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("initial websocket turn failed: %v", err)
		}
		if event["type"] == "response.completed" {
			break
		}
	}

	switcher.Swap(nextBackend)
	if err := conn.WriteJSON(map[string]any{"type": "response.append", "model": "old-alias", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("old socket kept the deleted route, payload=%s err=%v", payload, err)
	}
}

func TestLoadGatewaySnapshotUsesOfficialBaselineWithoutExecutingCodexDebug(t *testing.T) {
	root := t.TempDir()
	restoreRoot := config.SetProjectRootForTest(root)
	defer restoreRoot()
	if err := os.MkdirAll(config.StateDirFor(root), 0o700); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "executed")
	codex := filepath.Join(root, "codex")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 99\n"
	if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	identity, err := catalog.ComputeNativeArtifactIdentity(codex)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveOfficialModelsBaseline(&catalog.OfficialModelsBaseline{Artifact: identity, Slugs: []string{"gpt-official"}, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	cfg.WopalAI.Models = nil
	configPath := config.ConfigPathFor(root)
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	snapshot, err := loadGatewaySnapshot(root, configPath)
	if err != nil {
		t.Fatalf("loadGatewaySnapshot: %v", err)
	}
	if _, err := snapshot.routes.Resolve("gpt-official"); err != nil {
		t.Fatalf("official route: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("codex executable was run during serve snapshot load: stat=%v", err)
	}
}

func TestBackendSwitcherCloseStopsCurrentSnapshot(t *testing.T) {
	stopped := make(chan struct{})
	switcher := newBackendSwitcher(newRuntimeBackend(http.NotFoundHandler(), func() { close(stopped) }))
	switcher.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("current snapshot was not stopped")
	}
}

func TestRuntimeConfigCheckpointRestoresPreviousGeneratedConfig(t *testing.T) {
	root := t.TempDir()
	path := config.CLIProxyRuntimeConfigPathFor(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := captureRuntimeConfigCheckpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Restore(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old\n" {
		t.Fatalf("restored runtime config = %q", got)
	}
}

func TestReloadGatewayBackendFailureRestoresRuntimeConfigAndReturnsNoCandidate(t *testing.T) {
	root := t.TempDir()
	path := config.CLIProxyRuntimeConfigPathFor(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalStart := startGatewayBackendHook
	startGatewayBackendHook = func(string, string, string, *activity.Logger, *runtimeReloadStatus, proxy.RouteResolver) (*runtimeBackend, *gatewaySnapshot, error) {
		if err := os.WriteFile(path, []byte("failed-candidate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil, fmt.Errorf("candidate invalid")
	}
	t.Cleanup(func() { startGatewayBackendHook = originalStart })

	backend, err := reloadGatewayBackend(root, "unused", "instance", config.ListenConfig{Host: "127.0.0.1", Port: 11435}, nil, newRuntimeReloadStatus())
	if err == nil || backend != nil {
		t.Fatalf("reload = %#v, %v; want failed reload without candidate", backend, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "active\n" {
		t.Fatalf("runtime config after failed reload = %q", got)
	}
}
