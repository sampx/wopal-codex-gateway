package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
)

func TestQuotaProtectionCommandSwitchesOverride(t *testing.T) {
	setupConfigAuthTestRoot(t)

	output, err := captureConfigAuthTestStdout(t, func() error {
		return run([]string{"quota", "protection", "off"})
	})
	if err != nil {
		t.Fatalf("quota protection off error = %v", err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode output %q: %v", output, err)
	}
	if payload["protection"] != "disabled" {
		t.Fatalf("off output protection = %v, want disabled", payload["protection"])
	}
	state, exists, err := quota.ReadProtectionState(config.QuotaProtectionPath())
	if err != nil {
		t.Fatalf("ReadProtectionState() error = %v", err)
	}
	if !exists || state.Enabled {
		t.Fatalf("override file = %+v exists=%v, want disabled", state, exists)
	}

	output, err = captureConfigAuthTestStdout(t, func() error {
		return run([]string{"quota", "protection"})
	})
	if err != nil {
		t.Fatalf("quota protection status error = %v", err)
	}
	if !strings.Contains(output, `"protection": "disabled"`) {
		t.Fatalf("status output = %q, want disabled", output)
	}

	output, err = captureConfigAuthTestStdout(t, func() error {
		return run([]string{"quota", "protection", "on"})
	})
	if err != nil {
		t.Fatalf("quota protection on error = %v", err)
	}
	if !strings.Contains(output, `"protection": "enabled"`) {
		t.Fatalf("on output = %q, want enabled", output)
	}
	state, exists, err = quota.ReadProtectionState(config.QuotaProtectionPath())
	if err != nil {
		t.Fatalf("ReadProtectionState() error = %v", err)
	}
	if !exists || !state.Enabled {
		t.Fatalf("override file = %+v exists=%v, want enabled", state, exists)
	}
}

func TestQuotaProtectionCommandRejectsUnknownAction(t *testing.T) {
	setupConfigAuthTestRoot(t)
	if err := run([]string{"quota", "protection", "sometimes"}); err == nil {
		t.Fatal("unknown protection action was accepted")
	}
}

func TestQuotaProtectionAppearsInHelp(t *testing.T) {
	if !strings.Contains(helpText, "quota protection") {
		t.Fatalf("help does not mention the quota protection command")
	}
}

func TestStartupReenablesTemporarilyDisabledQuotaProtection(t *testing.T) {
	root := setupConfigAuthTestRoot(t)
	if _, err := quota.WriteProtectionState(config.QuotaProtectionPathFor(root), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := resetQuotaProtectionAtStartup(root); err != nil {
		t.Fatal(err)
	}
	state, exists, err := quota.ReadProtectionState(config.QuotaProtectionPathFor(root))
	if err != nil || !exists || !state.Enabled {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
}
