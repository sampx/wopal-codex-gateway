package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quotaTestGuardState(now time.Time, fiveHourStep, weeklyStep float64) PersistedState {
	account := AccountState{
		FiveHour: &Window{
			UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
			ResetAt: now.Add(2 * time.Hour), ObservedAt: now,
		},
		FiveHourMaxStep: fiveHourStep,
		WeeklyMaxStep:   weeklyStep,
	}
	return PersistedState{
		Version:       1,
		Accounts:      map[string]AccountState{AccountKey("acct"): account},
		LatestAccount: AccountKey("acct"),
	}
}

func TestQuotaResetLearningClearsObservedSteps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	if err := writeQuotaStateFile(path, quotaTestGuardState(now, 3, 2)); err != nil {
		t.Fatal(err)
	}

	cleared, err := ResetLearning(path)
	if err != nil {
		t.Fatalf("ResetLearning: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("cleared accounts = %d, want 1", cleared)
	}

	state, err := readQuotaStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Accounts[AccountKey("acct")]
	if account.FiveHourMaxStep != 0 || account.WeeklyMaxStep != 0 {
		t.Fatalf("learned steps survived reset: %+v", account)
	}
	if account.FiveHour == nil || account.FiveHour.UsedPercent != 93 {
		t.Fatalf("reset modified the quota observation: %+v", account.FiveHour)
	}
}

func TestQuotaResetLearningReportsMissingStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "official-quota-state.json")
	_, err := ResetLearning(path)
	if err == nil {
		t.Fatal("expected an error for a missing state file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the state path: %v", err)
	}
}

func TestQuotaStatusExposesBaseAndEffectiveThresholds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := NewGuard(nil)
	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: now.Add(2 * time.Hour), ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: now.Add(2 * time.Hour), ObservedAt: now.Add(5 * time.Second),
	})

	status := guard.Status(now.Add(10 * time.Second))
	checks := map[string]float64{
		"five_hour_stop_percent":           97,
		"weekly_stop_percent":              99,
		"effective_five_hour_stop_percent": 96,
		"effective_weekly_stop_percent":    99,
		"observed_five_hour_max_step":      3,
		"observed_weekly_max_step":         0,
	}
	for field, want := range checks {
		got, ok := status[field].(float64)
		if !ok || got != want {
			t.Fatalf("%s = %v, want %v", field, status[field], want)
		}
	}
	for _, forbidden := range []string{"account", "account_id", "api_key"} {
		if _, present := status[forbidden]; present {
			t.Fatalf("status leaks %q", forbidden)
		}
	}
}

func TestQuotaPersistAdoptsExternalLearningReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: now.Add(2 * time.Hour), ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: now.Add(2 * time.Hour), ObservedAt: now.Add(5 * time.Second),
	})
	guard.persist(now)

	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("guard did not learn the step: %v", step)
	}

	// `quota reset-learning` runs in another process and rewrites the file.
	if _, err := ResetLearning(path); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	guard.persist(now)
	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 0 {
		t.Fatalf("daemon did not adopt the external reset: %v", step)
	}

	// The adopted value must survive the next round trip instead of being
	// resurrected from memory.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Accounts[AccountKey("acct")].FiveHourMaxStep; got != 0 {
		t.Fatalf("persisted learned step = %v, want 0", got)
	}
}
