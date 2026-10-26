package main

import (
	"reflect"
	"strings"
	"testing"

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
	t.Cleanup(func() {
		refreshCatalogCommand = previousRefresh
		recordCatalogExternalModels = previousRecord
		signalGatewayReload = previousSignal
	})
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
