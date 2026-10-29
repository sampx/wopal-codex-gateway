package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/proxy"
)

func TestRuntimeStatusPayloadReportsProvidersRolesCredentialsAndReloadWithoutSecrets(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Headers: map[string]string{"x-safe": "DO-NOT-LEAK"},
		Models:  []config.ExternalModelConfig{{Name: "upstream", Alias: "friendly", Protocol: config.ProtocolOpenAIResponses}},
	}
	cfg.Providers["idle"] = config.ProviderConfig{BaseURL: "https://idle.test"}

	reload := newRuntimeReloadStatus()
	reload.MarkFailure(errors.New("candidate config invalid"))
	baselineTime := time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)
	catalogStatus := runtimeCatalogSnapshot{
		Status:        "current",
		Baseline:      &catalog.OfficialModelsBaseline{Artifact: catalog.NativeArtifactIdentity{SHA256: "baseline-hash"}, UpdatedAt: baselineTime},
		CurrentSHA256: "current-hash",
		CheckedAt:     baselineTime.Add(time.Minute),
	}
	payload := buildRuntimeStatusPayload(
		cfg,
		proxy.RuntimeRoles{
			OfficialFallback: proxy.RoleTarget{ProviderID: "vendor", ClientModel: "friendly", Ready: true},
			Compaction:       proxy.RoleTarget{ProviderID: "wopal-ai", ClientModel: "wopal-ai/deepseek-v4.1-flash", Ready: true},
		},
		map[string]bool{"wopal-ai": true, "vendor": true, "idle": false},
		reload.Snapshot(),
		catalogStatus,
		map[string]any{"state": "protected"},
		map[string]any{"active": true, "effective_model": "friendly"},
		map[string]any{"enabled": true},
		"instance-1",
		"disabled",
	)

	providers := payload["providers"].(map[string]any)
	vendor := providers["vendor"].(map[string]any)
	if vendor["credential_configured"] != true {
		t.Fatalf("vendor credential status = %#v", vendor)
	}
	idle := providers["idle"].(map[string]any)
	if idle["credential_configured"] != false {
		t.Fatalf("idle credential status = %#v", idle)
	}
	roles := payload["roles"].(map[string]any)
	fallback := roles["officialFallback"].(map[string]any)
	if fallback["provider"] != "vendor" || fallback["client_model"] != "friendly" || fallback["status"] != "ready" {
		t.Fatalf("fallback role = %#v", fallback)
	}
	reloadPayload := payload["reload"].(map[string]any)
	if reloadPayload["status"] != "failed" || reloadPayload["using_previous_snapshot"] != true {
		t.Fatalf("reload status = %#v", reloadPayload)
	}
	if payload["request_body_logging"] != "disabled" || payload["service"].(map[string]any)["status"] != "running" {
		t.Fatalf("service payload = %#v", payload)
	}
	catalogPayload := payload["catalog"].(map[string]any)
	if catalogPayload["status"] != "current" || catalogPayload["baseline_sha256"] != "baseline-hash" || catalogPayload["current_sha256"] != "current-hash" {
		t.Fatalf("catalog status = %#v", catalogPayload)
	}
	if _, ok := catalogPayload["last_auto_refresh"]; ok {
		t.Fatalf("last_auto_refresh should be omitted before auto recovery: %#v", catalogPayload)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"DO-NOT-LEAK", "Authorization", "x-safe"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("status leaked %q: %s", forbidden, text)
		}
	}
}

func TestRuntimeCatalogStatusReportsFailedAutoRecoverySeparatelyFromReload(t *testing.T) {
	baseline := &catalog.OfficialModelsBaseline{
		Artifact:  catalog.NativeArtifactIdentity{SHA256: "old-hash"},
		UpdatedAt: time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC),
	}
	status := newRuntimeCatalogStatus(baseline)
	status.MarkAutoFailure(errors.New(" refresh failed\nwith details "))
	got := catalogStatusPayload(status.Snapshot())
	if got["status"] != "refresh_failed" || got["baseline_sha256"] != "old-hash" {
		t.Fatalf("catalog failure payload = %#v", got)
	}
	last := got["last_auto_refresh"].(map[string]any)
	if last["status"] != "failed" || last["error"] != "refresh failed with details" {
		t.Fatalf("last auto refresh = %#v", last)
	}
}

func TestRuntimeReloadStatusClearsFailureAfterSuccessfulSwap(t *testing.T) {
	status := newRuntimeReloadStatus()
	status.MarkFailure(errors.New("bad candidate"))
	if got := status.Snapshot(); got.Status != "failed" || !got.UsingPreviousSnapshot {
		t.Fatalf("failure snapshot = %+v", got)
	}
	status.MarkSuccess()
	if got := status.Snapshot(); got.Status != "ok" || got.UsingPreviousSnapshot || got.Error != "" {
		t.Fatalf("success snapshot = %+v", got)
	}
}
