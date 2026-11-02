package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func weeklyOverLimitPayload() []byte {
	return []byte(`{"type":"codex.rate_limits","rate_limits":{"secondary":{"used_percent":99.5,"window_minutes":10080,"reset_after_seconds":3600}}}`)
}

func newGuardWithProtectionFile(t *testing.T) (*Guard, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quota-protection.json")
	guard := NewGuard(nil)
	guard.SetProtectionPath(path)
	return guard, path
}

func TestQuotaProtectionDefaultsToEnabled(t *testing.T) {
	now := time.Now()
	guard, _ := newGuardWithProtectionFile(t)
	guard.ObserveRateLimitPayload("acct-protect-default", weeklyOverLimitPayload(), now)

	if !guard.ProtectionEnabled(now) {
		t.Fatal("protection should default to enabled without an override file")
	}
	if blocked, reason := guard.Blocked("acct-protect-default", now); !blocked || reason != "weekly" {
		t.Fatalf("Blocked() = %v %q, want blocked weekly", blocked, reason)
	}
}

func TestQuotaProtectionOffStopsBlockingAndFailover(t *testing.T) {
	now := time.Now()
	guard, path := newGuardWithProtectionFile(t)
	guard.ObserveRateLimitPayload("acct-protect-off", weeklyOverLimitPayload(), now)
	if blocked, _ := guard.Blocked("acct-protect-off", now); !blocked {
		t.Fatal("weekly usage over the stop point should block while protection is enabled")
	}

	if _, err := WriteProtectionState(path, false, now); err != nil {
		t.Fatalf("WriteProtectionState() error = %v", err)
	}
	if guard.ProtectionEnabled(now) {
		t.Fatal("protection switch did not pick up the disabled override")
	}
	if blocked, reason := guard.Blocked("acct-protect-off", now); blocked {
		t.Fatalf("blocked while protection is disabled: reason=%q", reason)
	}
	blocked, _ := guard.BlockedModelWithTarget("acct-protect-off", "gpt-6.1-sol", "wopal-ai/deepseek-v4.1-flash", now)
	if blocked {
		t.Fatal("official request was still failed over while protection is disabled")
	}
	if active, _ := guard.FailoverStatus()["active"].(bool); active {
		t.Fatalf("failover stayed active after protection was disabled: %v", guard.FailoverStatus())
	}

	if _, err := WriteProtectionState(path, true, now.Add(time.Second)); err != nil {
		t.Fatalf("WriteProtectionState() error = %v", err)
	}
	later := now.Add(2 * time.Second)
	if blocked, reason := guard.Blocked("acct-protect-off", later); !blocked || reason != "weekly" {
		t.Fatalf("protection was not restored: blocked=%v reason=%q", blocked, reason)
	}
}

func TestQuotaProtectionReportsSwitchState(t *testing.T) {
	now := time.Now()
	guard, path := newGuardWithProtectionFile(t)
	if _, err := WriteProtectionState(path, false, now); err != nil {
		t.Fatalf("WriteProtectionState() error = %v", err)
	}

	status := guard.ProtectionStatus(now)
	if status["state"] != "disabled" {
		t.Fatalf("ProtectionStatus() state = %v, want disabled", status["state"])
	}
	if status["source"] != "override" {
		t.Fatalf("ProtectionStatus() source = %v, want override", status["source"])
	}

	guard.ObserveRateLimitPayload("acct-protect-status", weeklyOverLimitPayload(), now)
	quotaStatus := guard.Status(now)
	if quotaStatus["state"] == "protected" {
		t.Fatalf("Status() reported protected while protection is disabled: %v", quotaStatus)
	}
	if protection, ok := quotaStatus["protection"].(map[string]any); !ok || protection["state"] != "disabled" {
		t.Fatalf("Status() protection = %v, want disabled switch", quotaStatus["protection"])
	}
}

func TestQuotaProtectionWithoutFileStillReportsEnabled(t *testing.T) {
	guard, _ := newGuardWithProtectionFile(t)
	status := guard.ProtectionStatus(time.Now())
	if status["state"] != "enabled" || status["source"] != "default" {
		t.Fatalf("ProtectionStatus() = %v, want enabled from default", status)
	}
}

func TestMalformedProtectionOverrideKeepsProtectionOn(t *testing.T) {
	now := time.Now()
	guard, path := newGuardWithProtectionFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create protection dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed override: %v", err)
	}
	guard.ObserveRateLimitPayload("acct-protect-bad", weeklyOverLimitPayload(), now)

	if !guard.ProtectionEnabled(now) {
		t.Fatal("malformed override should leave protection enabled")
	}
	if blocked, _ := guard.Blocked("acct-protect-bad", now); !blocked {
		t.Fatal("malformed override let a protected request through")
	}
}

func TestMissingEnabledCannotDisableProtection(t *testing.T) {
	g, path := newGuardWithProtectionFile(t)
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !g.ProtectionEnabled(time.Now()) {
		t.Fatal("incomplete override disabled protection")
	}
}
