package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestQuotaResetLearningMissingStateExitsSuccessfully(t *testing.T) {
	root := useTempGatewayRoot(t)

	stdout, err := captureStdout(t, func() error { return runQuotaCommand("reset-learning", nil) })
	if err != nil {
		t.Fatalf("quota reset-learning: %v", err)
	}
	want := "Quota learning reset: 0 account(s) cleared; five_hour=97, weekly=99.\n"
	if stdout != want {
		t.Fatalf("output = %q, want %q", stdout, want)
	}

	logPayload, err := os.ReadFile(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatalf("read reset metadata log: %v", err)
	}
	if !strings.Contains(string(logPayload), `"event":"official_quota_learning_reset"`) {
		t.Fatalf("reset metadata event missing from gateway.log: %s", logPayload)
	}
}

func TestQuotaResetLearningUsesConfiguredLogRotation(t *testing.T) {
	useTempGatewayRoot(t)
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	cfg.Logs = config.Logs{MaxSizeMB: 1, MaxBackups: 1}
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := quotaLogsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxSizeMB != 1 || got.MaxBackups != 1 {
		t.Fatalf("quota log config = %+v, want 1MB/1 backup", got)
	}
}
