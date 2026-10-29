package quota

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOfficialQuotaGuardThresholdsAndReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := NewGuard(nil)

	guard.ObserveRateLimitPayload("acct-five", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":97,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	blocked, reason := guard.Blocked("acct-five", now)
	if !blocked || reason != "five_hour" {
		t.Fatalf("five-hour guard = blocked:%v reason:%q", blocked, reason)
	}

	guard.ObserveRateLimitPayload("acct-week", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":99,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	blocked, reason = guard.Blocked("acct-week", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("weekly guard = blocked:%v reason:%q", blocked, reason)
	}

	guard.ObserveRateLimitPayload("acct-open", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
	if blocked, _ := guard.Blocked("acct-open", now); blocked {
		t.Fatal("guard blocked below fixed thresholds")
	}

	if blocked, _ := guard.Blocked("acct-five", now.Add(301*time.Second)); blocked {
		t.Fatal("five-hour guard did not clear after reset")
	}
}

func TestOfficialQuotaGuardReadsHeadersByWindowLength(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := NewGuard(nil)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "99")
	headers.Set("X-Codex-Primary-Window-Minutes", "10080")
	headers.Set("X-Codex-Primary-Reset-At", "1800100000")
	headers.Set("X-Codex-Secondary-Used-Percent", "96")
	headers.Set("X-Codex-Secondary-Window-Minutes", "300")
	headers.Set("X-Codex-Secondary-Reset-At", "1800000300")

	guard.ObserveHeaders("acct", headers, now)
	blocked, reason := guard.Blocked("acct", now)
	if !blocked || reason != "weekly" {
		t.Fatalf("header guard = blocked:%v reason:%q", blocked, reason)
	}
}

func TestOfficialQuotaGuardModelExemptions(t *testing.T) {
	now := time.Now()
	guard := NewGuard(nil)
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)

	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		if blocked, reason := guard.BlockedModel("acct", model, now); blocked {
			t.Fatalf("quota-exempt model %s blocked with reason %q", model, reason)
		}
	}
	if blocked, reason := guard.BlockedModel("acct", "codex-auto-review", now); !blocked || reason != "five_hour" {
		t.Fatalf("auto-review guard = blocked:%v reason:%q", blocked, reason)
	}
	if blocked, reason := guard.BlockedModel("acct", "gpt-6.1-sol", now); !blocked || reason != "five_hour" {
		t.Fatalf("metered model guard = blocked:%v reason:%q", blocked, reason)
	}
}

func TestOfficialQuotaGuardPersistsProtectedStateAcrossRestart(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	statePath := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, statePath, now)
	guard.ObserveRateLimitPayload("private-account-id", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600},"secondary":{"used_percent":53,"window_minutes":10080,"reset_after_seconds":7200}}}`), now)

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-account-id")) {
		t.Fatal("quota persistence leaked the raw ChatGPT account id")
	}

	reloaded := NewGuardWithState(nil, statePath, now.Add(time.Second))
	if blocked, reason := reloaded.BlockedModel("private-account-id", "gpt-6.1-sol", now.Add(time.Second)); !blocked || reason != "five_hour" {
		t.Fatalf("reloaded guard = blocked:%v reason:%q", blocked, reason)
	}
	if blocked, _ := reloaded.BlockedModel("private-account-id", "gpt-6.1-sol", now.Add(3601*time.Second)); blocked {
		t.Fatal("persisted five-hour protection survived its reset time")
	}
}

func TestQuotaObservationRejectsOutOfOrderSamples(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)
	guard.ObserveWindow("acct", Window{UsedPercent: 98, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now.Add(10 * time.Second)})
	guard.ObserveWindow("acct", Window{UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now.Add(time.Second)})
	guard.persist(now.Add(20 * time.Second))

	reloaded := NewGuardWithState(nil, path, now.Add(20*time.Second))
	state := reloaded.accounts[AccountKey("acct")]
	if state.FiveHour == nil || state.FiveHour.UsedPercent != 98 || !state.FiveHour.ObservedAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("out-of-order sample replaced newer state: %+v", state.FiveHour)
	}
}
