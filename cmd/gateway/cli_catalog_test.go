package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

func TestCatalogCommandRejectsUnknownSubcommand(t *testing.T) {
	err := runCatalogCommand("status", nil)
	if err == nil {
		t.Fatal("runCatalogCommand() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "catalog refresh") {
		t.Fatalf("error = %q, want catalog refresh usage", err.Error())
	}
}

func TestCatalogRefreshRecordsExternalNamesBeforeReload(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	previousRefresh := refreshCatalogCommand
	previousRecord := recordCatalogExternalModels
	previousSignal := signalGatewayReload
	previousRunning := catalogGatewayRunning
	t.Cleanup(func() {
		refreshCatalogCommand = previousRefresh
		recordCatalogExternalModels = previousRecord
		signalGatewayReload = previousSignal
		catalogGatewayRunning = previousRunning
	})
	catalogGatewayRunning = func() bool { return false }
	refreshCatalogCommand = func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
		return &catalog.CatalogRefreshResult{ExternalNames: []string{"friendly", "vendor/model"}}, nil
	}
	var recorded []string
	recordCatalogExternalModels = func(names []string) error {
		recorded = append(recorded, names...)
		return nil
	}
	reloaded := false
	signalGatewayReload = func() error {
		if !reflect.DeepEqual(recorded, []string{"friendly", "vendor/model"}) {
			t.Fatalf("reload happened before exposed names were recorded: %#v", recorded)
		}
		reloaded = true
		return nil
	}

	if err := runCatalogCommand("refresh", nil); err != nil {
		t.Fatal(err)
	}
	if !reloaded {
		t.Fatal("catalog refresh did not signal reload")
	}
}

func TestCatalogRefreshWaitsForRunningDaemonToActivateCommittedBaseline(t *testing.T) {
	setupConfigAuthTestRoot(t)
	var active atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_status" {
			http.NotFound(w, r)
			return
		}
		sha := "old-hash"
		if active.Load() {
			sha = "new-hash"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"catalog": map[string]any{"status": "current", "baseline_sha256": sha}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := strings.Cut(parsed.Host, ":")
	port, _ := strconv.Atoi(portText)
	cfg := config.DefaultGatewayConfig()
	cfg.Listen.Host = host
	cfg.Listen.Port = port
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	previousRefresh := refreshCatalogCommand
	previousRecord := recordCatalogExternalModels
	previousSignal := signalGatewayReload
	previousRunning := catalogGatewayRunning
	previousWait := waitCatalogBaseline
	t.Cleanup(func() {
		refreshCatalogCommand = previousRefresh
		recordCatalogExternalModels = previousRecord
		signalGatewayReload = previousSignal
		catalogGatewayRunning = previousRunning
		waitCatalogBaseline = previousWait
	})
	refreshCatalogCommand = func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
		return &catalog.CatalogRefreshResult{Baseline: &catalog.OfficialModelsBaseline{Artifact: catalog.NativeArtifactIdentity{SHA256: "new-hash"}, UpdatedAt: time.Now()}}, nil
	}
	recordCatalogExternalModels = func([]string) error { return nil }
	catalogGatewayRunning = func() bool { return true }
	signalGatewayReload = func() error {
		active.Store(true)
		return nil
	}
	waitCatalogBaseline = waitForCatalogBaseline

	if err := runCatalogCommand("refresh", nil); err != nil {
		t.Fatal(err)
	}
}
