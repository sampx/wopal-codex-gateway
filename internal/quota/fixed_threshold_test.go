package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaThresholdsStayFixedAcrossNearbySamples(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(2 * time.Hour)
	guard := NewGuard(nil)
	for i, used := range []float64{90, 93, 96} {
		guard.ObserveWindow("acct", Window{
			UsedPercent: used, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: resetAt, ObservedAt: now.Add(time.Duration(i) * 10 * time.Second),
		})
	}
	if blocked, _ := guard.Blocked("acct", now.Add(30*time.Second)); blocked {
		t.Fatal("nearby quota deltas lowered the fixed five-hour 97% threshold")
	}
	guard.ObserveWindow("acct", Window{UsedPercent: 97, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: resetAt, ObservedAt: now.Add(40 * time.Second)})
	if blocked, reason := guard.Blocked("acct", now.Add(40*time.Second)); !blocked || reason != "five_hour" {
		t.Fatalf("fixed threshold did not activate at 97%%: blocked=%v reason=%q", blocked, reason)
	}
	status := guard.Status(now.Add(40 * time.Second))
	if status["effective_five_hour_stop_percent"] != FiveHourStopPercent || status["effective_weekly_stop_percent"] != WeeklyStopPercent {
		t.Fatalf("effective thresholds are not fixed: %#v", status)
	}
	if _, ok := status["observed_five_hour_max_step"]; ok {
		t.Fatalf("status still exposes learned quota step: %#v", status)
	}
}

func TestLegacyQuotaLearningFieldsAreIgnored(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	legacy := map[string]any{
		"version": 1,
		"accounts": map[string]any{
			AccountKey("acct"): map[string]any{
				"FiveHour": map[string]any{
					"UsedPercent": 96, "WindowMinutes": 300,
					"ResetAt": now.Add(time.Hour), "ObservedAt": now,
				},
				"five_hour_max_step": 5,
				"weekly_max_step":    5,
			},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	guard := NewGuardWithState(nil, path, now)
	if blocked, _ := guard.Blocked("acct", now); blocked {
		t.Fatal("legacy learned step changed the fixed threshold")
	}
	guard.ObserveWindow("acct", Window{UsedPercent: 96, WindowMinutes: officialFiveHourWindowMinutes, ResetAt: now.Add(time.Hour), ObservedAt: now.Add(time.Second)})
	guard.persist(now.Add(time.Second))
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) == "" || containsJSONKey(rewritten, "five_hour_max_step") || containsJSONKey(rewritten, "weekly_max_step") {
		t.Fatalf("legacy learning fields were persisted again: %s", rewritten)
	}
}

func containsJSONKey(raw []byte, key string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return findJSONKey(value, key)
}

func findJSONKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if name == key || findJSONKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if findJSONKey(child, key) {
				return true
			}
		}
	}
	return false
}
