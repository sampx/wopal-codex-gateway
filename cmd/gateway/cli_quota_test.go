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

// TestApplyProjectRootFlagPinsControlPlaneCommands guards the regression found
// during AC verification: a CLI command that ignored --project-root acted on
// an unrelated runtime directory.
func TestApplyProjectRootFlagPinsControlPlaneCommands(t *testing.T) {
	root := t.TempDir()
	restore := config.SetProjectRootForTest("")
	t.Cleanup(restore)

	remaining, err := applyProjectRootFlag([]string{"reset-learning", "--project-root", root})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != "reset-learning" {
		t.Fatalf("remaining args = %v, want [reset-learning]", remaining)
	}
	if got := config.StateDir(); got != filepath.Join(root, "runtime") {
		t.Fatalf("state dir = %q, want %q", got, filepath.Join(root, "runtime"))
	}
}

func TestApplyProjectRootFlagRejectsMissingValue(t *testing.T) {
	if _, err := applyProjectRootFlag([]string{"reset-learning", "--project-root"}); err == nil {
		t.Fatal("--project-root without a value was accepted")
	}
}

// TestQuotaResetLearningHonoursProjectRoot proves the fix end to end: the file
// under the named root is cleared and an unrelated runtime is untouched.
func TestQuotaResetLearningHonoursProjectRoot(t *testing.T) {
	named := t.TempDir()
	other := t.TempDir()
	restore := config.SetProjectRootForTest("")
	t.Cleanup(restore)

	account := "def0b17f603285ef4336f6e3e3dcd43cd18fd789277ab9d6e086b7e567539e25"
	seed := func(root string) string {
		path := filepath.Join(root, "runtime", "official-quota-state.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		payload := `{"version":1,"accounts":{"` + account + `":{"five_hour_max_step":3,"weekly_max_step":2}},"latest_account":"` + account + `"}`
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	namedPath := seed(named)
	otherPath := seed(other)

	if _, err := applyProjectRootFlag([]string{"--project-root", named}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return runQuotaCommand("reset-learning", nil) }); err != nil {
		t.Fatal(err)
	}

	cleared, err := os.ReadFile(namedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cleared), "five_hour_max_step") {
		t.Fatalf("named root still holds a learned step: %s", cleared)
	}
	untouched, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(untouched), `"five_hour_max_step":3`) {
		t.Fatalf("unrelated runtime was modified: %s", untouched)
	}
}
