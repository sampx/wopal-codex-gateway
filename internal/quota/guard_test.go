package quota

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOfficialQuotaGuardThresholdsAndReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := NewGuard(nil)

	guard.ObserveRateLimitPayload("acct-five", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_at":1800000300},"secondary":{"used_percent":98,"window_minutes":10080,"reset_at":1800100000}}}`), now)
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
		t.Fatal("guard blocked below both thresholds")
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
	if blocked, reason := reloaded.BlockedModel("another-account-after-restart", "gpt-6.1-sol", now.Add(time.Second)); blocked {
		t.Fatalf("persisted quota state leaked across accounts: reason=%q", reason)
	}
	if blocked, _ := reloaded.BlockedModel("private-account-id", "gpt-6.1-sol", now.Add(3601*time.Second)); blocked {
		t.Fatal("persisted five-hour protection survived its reset time")
	}
}

func TestOfficialQuotaGuardStatusUsesHashedAccountState(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	guard := NewGuard(nil)
	guard.ObserveRateLimitPayload("private-account-id", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), now)
	status := guard.Status(now)
	if status["state"] != "protected" || status["reason"] != "five_hour" {
		t.Fatalf("quota status = %#v", status)
	}
}

func TestQuotaExemptOfficialModelsIncludeCurrentLuna(t *testing.T) {
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna"} {
		if !quotaExemptOfficialModel(model) {
			t.Fatalf("model %q should bypass quota reserve", model)
		}
	}
	for _, model := range []string{"gpt-6.1-sol", "codex-auto-review"} {
		if quotaExemptOfficialModel(model) {
			t.Fatalf("metered model %q must not bypass quota reserve", model)
		}
	}
}

func TestOfficialQuotaGuardDoesNotLowerThresholdForLongObservationGaps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := NewGuard(nil)
	observe := func(percent float64, offset time.Duration) {
		guard.ObserveWindow("acct", Window{
			UsedPercent: percent, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: resetAt, ObservedAt: now.Add(offset),
		})
	}

	// A gap wider than the sampling window can span many turns or an idle
	// period. Its delta is not evidence about a single step.
	observe(39, 0)
	observe(65, 5*time.Minute)
	if blocked, _ := guard.Blocked("acct", now); blocked {
		t.Fatal("guard lowered the threshold after an accumulated observation gap")
	}
	observe(96, 6*time.Minute)
	if blocked, _ := guard.Blocked("acct", now); blocked {
		t.Fatal("guard stopped below the base 97 percent threshold")
	}
	observe(97, 7*time.Minute)
	if blocked, _ := guard.Blocked("acct", now); !blocked {
		t.Fatal("guard did not stop at the base 97 percent threshold")
	}
}

func TestOfficialQuotaGuardLowersThresholdForNearbyStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := NewGuard(nil)
	observe := func(percent float64, offset time.Duration) {
		guard.ObserveWindow("acct", Window{
			UsedPercent: percent, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: resetAt, ObservedAt: now.Add(offset),
		})
	}

	observe(90, 0)
	observe(93, 10*time.Second)
	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("observed max step = %v, want 3", step)
	}
	if effective, _ := guard.Status(now)["effective_five_hour_stop_percent"].(float64); effective != 96 {
		t.Fatalf("effective stop = %v, want 96", effective)
	}
	if blocked, _ := guard.Blocked("acct", now); blocked {
		t.Fatal("guard stopped below the learned 96 percent threshold")
	}
	observe(96, 20*time.Second)
	if blocked, _ := guard.Blocked("acct", now); !blocked {
		t.Fatal("guard did not stop at the learned 96 percent threshold")
	}
}

func TestOfficialQuotaGuardDiscardsOverCapLearnedStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	path := filepath.Join(t.TempDir(), "quota.json")
	legacy := map[string]any{
		"version": 1,
		"accounts": map[string]any{
			AccountKey("acct"): map[string]any{
				"FiveHour": map[string]any{
					"UsedPercent": 96, "WindowMinutes": 300, "ResetAt": resetAt, "ObservedAt": now,
				},
				"five_hour_max_step": 26,
				"weekly_max_step":    4,
			},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	guard := NewGuardWithState(nil, path, now)
	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 0 {
		t.Fatalf("over-cap learned step survived load: %v", step)
	}
	if blocked, _ := guard.Blocked("acct", now); blocked {
		t.Fatal("guard stopped below the base threshold after loading an over-cap step")
	}
}

func TestOfficialQuotaGuardKeepsLearnedStepAcrossWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := NewGuard(nil)
	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now.Add(10 * time.Second),
	})

	// The next window starts from scratch, but what a step costs is still known.
	nextReset := resetAt.Add(5 * time.Hour)
	guard.ObserveWindow("acct", Window{
		UsedPercent: 0, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: nextReset, ObservedAt: now.Add(3 * time.Hour),
	})
	if step, _ := guard.Status(now.Add(3 * time.Hour))["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("learned step lost at window boundary: %v", step)
	}
}
