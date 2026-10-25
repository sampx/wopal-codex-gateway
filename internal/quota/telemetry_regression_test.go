package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
)

// quotaWindowSample builds a five-hour sample.
func quotaWindowSample(observedAt time.Time, usedPercent float64, resetAt time.Time) Window {
	return Window{UsedPercent: usedPercent, WindowMinutes: 300, ResetAt: resetAt, ObservedAt: observedAt}
}

func newQuotaTestLogger(t *testing.T, dir string) *activity.Logger {
	t.Helper()
	logger, err := activity.New(dir, config.Logs{})
	if err != nil {
		t.Fatalf("activity.New: %v", err)
	}
	t.Cleanup(logger.Close)
	return logger
}

func quotaLogContains(t *testing.T, dir, event string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatalf("read activity log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var decoded map[string]any
		if json.Unmarshal([]byte(line), &decoded) != nil {
			continue
		}
		if decoded["event"] == event {
			return true
		}
	}
	return false
}

// TestFixtureResetAtJitterNeverBlindsGuard reproduces the 2026-10-25 incident.
// Upstream reported the single five-hour window ending 11:15Z as 11:14:59Z from
// one transport and 11:15:01Z from another. Treating ResetAt as an exact window
// identity made the guard reject every later sample, so usage climbed past the
// 97% stop point while /_status still reported 19% and blocked nothing.
func TestFixtureResetAtJitterNeverBlindsGuard(t *testing.T) {
	base := time.Date(2026, 10, 25, 19, 14, 59, 0, time.Local)
	jittered := base.Add(2 * time.Second)
	start := time.Date(2026, 10, 25, 15, 47, 53, 0, time.Local)
	guard := NewGuard(nil)

	// The window the guard actually stored in production.
	if !guard.ObserveWindow("acct", quotaWindowSample(start, 19, base)) {
		t.Fatal("baseline sample was not accepted")
	}
	// The sample that froze production: same window, reported 2s later.
	if !guard.ObserveWindow("acct", quotaWindowSample(start.Add(time.Minute), 19, jittered)) {
		t.Fatal("a 2-second reset_at drift was rejected as an older window")
	}
	// Usage then climbs past every stop point on the original reading.
	for offset, used := range []float64{40, 60, 80, 98, 100} {
		observed := start.Add(time.Duration(offset+2) * time.Minute)
		if !guard.ObserveWindow("acct", quotaWindowSample(observed, used, base)) {
			t.Fatalf("sample with used_percent=%.0f was rejected; the guard is blind", used)
		}
	}

	now := start.Add(10 * time.Minute)
	blocked, reason := guard.Blocked("acct", now)
	if !blocked || reason != "five_hour" {
		t.Fatalf("guard did not protect at 100%% usage: blocked=%v reason=%q", blocked, reason)
	}
	if telemetry, ok := guard.Status(now)["quota_telemetry"].(map[string]any); !ok || telemetry["stale"] != false {
		t.Fatalf("telemetry reported stale while samples were arriving: %+v", guard.Status(now)["quota_telemetry"])
	}
}

// A sample from a real older generation must still be dropped, or a delayed
// response would regress the guard onto a window that already reset.
func TestFixtureOlderQuotaWindowStillRejected(t *testing.T) {
	base := time.Date(2026, 10, 25, 19, 14, 59, 0, time.Local)
	now := time.Date(2026, 10, 25, 15, 47, 53, 0, time.Local)
	guard := NewGuard(nil)

	if !guard.ObserveWindow("acct", quotaWindowSample(now, 90, base)) {
		t.Fatal("current-window sample was not accepted")
	}
	older := base.Add(-officialFiveHourWindowMinutes * time.Minute)
	if guard.ObserveWindow("acct", quotaWindowSample(now.Add(time.Second), 5, older)) {
		t.Fatal("a sample from the previous window replaced the current one")
	}
	if state, ok := guard.Status(now)["five_hour"].(map[string]any); !ok || state["used_percent"] != 90.0 {
		t.Fatalf("stored usage = %v, want 90", guard.Status(now)["five_hour"])
	}
}

// A telemetry break has to be visible. An open reserve that stopped updating is
// not the same reading as an open reserve that is still being sampled.
func TestFixtureTelemetryBreakIsReportedStale(t *testing.T) {
	now := time.Date(2026, 10, 25, 15, 47, 53, 0, time.Local)
	resetAt := now.Add(4 * time.Hour)
	guard := NewGuard(nil)

	if !guard.ObserveWindow("acct", quotaWindowSample(now, 19, resetAt)) {
		t.Fatal("sample was not accepted")
	}
	fresh := guard.Status(now.Add(time.Minute))["quota_telemetry"].(map[string]any)
	if fresh["stale"] != false {
		t.Fatalf("fresh telemetry reported stale: %+v", fresh)
	}

	// Three hours with no accepted sample, exactly as production behaved.
	stale := guard.Status(now.Add(3 * time.Hour))["quota_telemetry"].(map[string]any)
	if stale["stale"] != true {
		t.Fatalf("telemetry that stopped accepting samples is not reported stale: %+v", stale)
	}
	if stale["last_accepted_seconds_ago"].(int64) != int64((3*time.Hour)/time.Second) {
		t.Fatalf("last_accepted_seconds_ago = %v", stale["last_accepted_seconds_ago"])
	}
}

// The first accepted sample has to reach the log. Before this, a guard that only
// ever saw one sample wrote nothing at all.
func TestFixtureFirstAcceptedSampleIsLogged(t *testing.T) {
	dir := t.TempDir()
	guard := NewGuard(newQuotaTestLogger(t, dir))
	now := time.Date(2026, 10, 25, 15, 47, 53, 0, time.Local)

	if !guard.ObserveWindow("acct", quotaWindowSample(now, 19, now.Add(4*time.Hour))) {
		t.Fatal("sample was not accepted")
	}
	if !quotaLogContains(t, dir, "official_quota_observed") {
		t.Fatal("the first accepted sample produced no official_quota_observed entry")
	}
}

// A rejected sample must announce itself, so a silent guard can be diagnosed
// from the log alone.
func TestFixtureRejectedSampleIsLogged(t *testing.T) {
	dir := t.TempDir()
	guard := NewGuard(newQuotaTestLogger(t, dir))
	now := time.Date(2026, 10, 25, 15, 47, 53, 0, time.Local)
	resetAt := now.Add(4 * time.Hour)

	if !guard.ObserveWindow("acct", quotaWindowSample(now, 90, resetAt)) {
		t.Fatal("sample was not accepted")
	}
	if guard.ObserveWindow("acct", quotaWindowSample(now.Add(-time.Minute), 10, resetAt)) {
		t.Fatal("an older sample was accepted")
	}
	if !quotaLogContains(t, dir, "official_quota_sample_rejected") {
		t.Fatal("a rejected sample produced no official_quota_sample_rejected entry")
	}
	if rejected := guard.Status(now)["quota_telemetry"].(map[string]any)["rejected_in_a_row"]; rejected != 1 {
		t.Fatalf("rejected_in_a_row = %v, want 1", rejected)
	}
}
