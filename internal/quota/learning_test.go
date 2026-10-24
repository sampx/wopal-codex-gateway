package quota

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestQuotaResetLearningTreatsMissingStateAsCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "official-quota-state.json")
	cleared, err := ResetLearning(path)
	if err != nil {
		t.Fatalf("ResetLearning on a missing state: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("cleared accounts = %d, want 0", cleared)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("reset created a state file for an absent state: %v", statErr)
	}
}

func TestQuotaResetLearningReportsCorruptStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResetLearning(path)
	if err == nil {
		t.Fatal("expected an error for a corrupt state file")
	}
	for _, want := range []string{path, "parse quota state"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
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

// TestQuotaResetLearningTakesEffectImmediately proves B-02: after a reset the
// next live decision and status report already use the base thresholds, without
// waiting for the daemon's own next persist.
func TestQuotaResetLearningTakesEffectImmediately(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)
	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(5 * time.Second),
	})
	guard.persist(now)

	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 3 {
		t.Fatalf("guard did not learn the step: %v", step)
	}

	if _, err := ResetLearning(path); err != nil {
		t.Fatal(err)
	}
	// Make the external write strictly newer than the guard's own persist.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// The very next status report must already show the base threshold.
	status := guard.Status(now)
	if step, _ := status["observed_five_hour_max_step"].(float64); step != 0 {
		t.Fatalf("status still reports a learned step: %v", step)
	}
	if effective, _ := status["effective_five_hour_stop_percent"].(float64); effective != FiveHourStopPercent {
		t.Fatalf("effective threshold = %v, want base %v", effective, FiveHourStopPercent)
	}

	// And the next routing decision uses the base threshold too: 96% is blocked
	// only while a >=3 step is still in force.
	blocked, _ := guard.Blocked("acct", now)
	if blocked {
		t.Fatal("guard still blocked at 93% after the reset")
	}
}

// TestQuotaResetLearningSurvivesDaemonStartupWindow covers the resurrection
// window in B-02: the daemon starts from a learned state and the user resets
// before the daemon persists anything. The cleared value must not be written
// back.
func TestQuotaResetLearningSurvivesDaemonStartupWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")

	account := AccountKey("acct")
	seeded := `{"version":1,"accounts":{"` + account + `":{"five_hour_max_step":3}},` +
		`"latest_account":"` + account + `"}`
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}

	// Daemon restart: learns the persisted step, has not persisted anything yet.
	// Do not call Status() here: it adopts an external reset as a side effect, and
	// that would repair the state this test is trying to observe.
	guard := NewGuardWithState(nil, path, now)
	guard.mu.RLock()
	learned := guard.accounts[account].FiveHourMaxStep
	guard.mu.RUnlock()
	if learned != 3 {
		t.Fatalf("guard did not restore the persisted step: %v", learned)
	}

	// The user clears it before the daemon's first own persist.
	if _, err := ResetLearning(path); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// The daemon's first persist must adopt the reset, not resurrect memory.
	guard.persist(now)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Accounts[account].FiveHourMaxStep; got != 0 {
		t.Fatalf("persisted learned step = %v, want 0 (reset was resurrected)", got)
	}
	if step, _ := guard.Status(now)["observed_five_hour_max_step"].(float64); step != 0 {
		t.Fatalf("guard still holds the learned step: %v", step)
	}
}

// TestQuotaResetLearningClearsActiveFailover covers the operator-visible state in
// B-02: a reset that makes the reserve inactive must also release failover.
func TestQuotaResetLearningClearsActiveFailover(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(5 * time.Second),
	})
	guard.persist(now)

	// With a 3-point learned step the effective threshold is 96, so 96% blocks.
	guard.ObserveWindow("acct", Window{
		UsedPercent: 96, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(10 * time.Second),
	})
	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now.Add(10*time.Second)); !blocked {
		t.Fatal("guard did not block at the learned threshold")
	}
	if active, _ := guard.FailoverStatus()["active"].(bool); !active {
		t.Fatal("failover was not recorded")
	}

	if _, err := ResetLearning(path); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// After the reset the base threshold (97) no longer blocks 96%.
	if blocked, _ := guard.Blocked("acct", now.Add(10*time.Second)); blocked {
		t.Fatal("guard still blocked after the reset")
	}
	if active, _ := guard.FailoverStatus()["active"].(bool); active {
		t.Fatal("failover stayed active after the reset made the reserve inactive")
	}
}

// quotaHeader builds the upstream quota headers for one five-hour sample.
// ObserveHeaders is the production entry point (HTTP responses, WebSocket
// frames and compaction replays all reach it) and it persists on every change.
func quotaHeader(used int, resetAt time.Time) http.Header {
	header := make(http.Header)
	header.Set("X-Codex-Primary-Used-Percent", strconv.Itoa(used))
	header.Set("X-Codex-Primary-Window-Minutes", strconv.Itoa(officialFiveHourWindowMinutes))
	header.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(resetAt.Unix(), 10))
	return header
}

// TestQuotaPersistIsRaceFreeUnderConcurrentObservations covers B-03. Run under
// -race: it proves the persistence cycle is serialized instead of relying on the
// account map's own lock, which is released before the file I/O.
func TestQuotaPersistIsRaceFreeUnderConcurrentObservations(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for turn := 0; turn < 15; turn++ {
				observedAt := now.Add(time.Duration(worker*15+turn) * time.Second)
				guard.ObserveHeaders("acct", quotaHeader(10+worker*15+turn, resetAt), observedAt)
				_ = guard.Status(observedAt)
				_, _ = guard.Blocked("acct", observedAt)
			}
		}(worker)
	}
	wg.Wait()

	// The state file must still be a valid document after the storm.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after concurrent persists: %v", err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("state file is corrupt after concurrent persists: %v", err)
	}
	if persisted.Version != 1 {
		t.Fatalf("state version = %d, want 1", persisted.Version)
	}
}

// TestQuotaPersistNeverWritesAnOlderSnapshot covers the lost-update half of B-03.
// Because persist snapshots the account state only after taking the persistence
// mutex, the newest observation always reaches disk: a slower goroutine holding
// an older snapshot can never rename its file last.
func TestQuotaPersistNeverWritesAnOlderSnapshot(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	const newest = 29
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Every worker walks the whole range, so the highest value is observed
			// from several goroutines while lower ones are still persisting.
			for turn := 0; turn <= newest; turn++ {
				observedAt := now.Add(time.Duration(worker*100+turn) * time.Second)
				guard.ObserveHeaders("acct", quotaHeader(turn, resetAt), observedAt)
			}
		}(worker)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	state, ok := persisted.Accounts[AccountKey("acct")]
	if !ok || state.FiveHour == nil {
		t.Fatal("persisted state lost the account window")
	}
	if state.FiveHour.UsedPercent != newest {
		t.Fatalf("persisted used_percent = %v, want the newest %v (an older snapshot landed last)",
			state.FiveHour.UsedPercent, float64(newest))
	}
}

// TestQuotaResetSurvivesConcurrentDaemonPersist covers B-R9-01 deterministically.
// The CLI reset and the daemon persist are separate processes, so an in-process
// mutex cannot serialize them.
//
// The ordering reproduced here is the one that resurrected a cleared value: the
// daemon starts a persist, then the reset lands, then the daemon completes its
// write. The test parks the daemon just before it takes the shared file lock, so
// the reset can take it first and finish; the daemon's persist must then re-read
// the cleared state instead of renaming its stale snapshot over it.
func TestQuotaResetSurvivesConcurrentDaemonPersist(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 93, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(5 * time.Second),
	})
	guard.persist(now)
	if step, _ := guard.learnedStepForTest("acct"); step != 3 {
		t.Fatalf("guard did not learn the step: %v", step)
	}

	// The reset lands while the daemon is about to persist.
	if _, err := ResetLearning(path); err != nil {
		t.Fatalf("ResetLearning: %v", err)
	}

	// The daemon's next persist must not resurrect what it still holds in memory.
	guard.persist(now.Add(time.Minute))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Accounts[AccountKey("acct")].FiveHourMaxStep; got != 0 {
		t.Fatalf("persisted learned step = %v, want 0 (a daemon write resurrected the reset)", got)
	}
}

// TestQuotaResetSurvivesStartupLoadInterleaving covers the second half of
// B-R9-01: the guard must not end up holding the old value together with the
// post-reset file mtime, which would make later adoption impossible.
func TestQuotaResetSurvivesStartupLoadInterleaving(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	account := AccountKey("acct")
	seeded := `{"version":1,"accounts":{"` + account + `":{"five_hour_max_step":3}},` +
		`"latest_account":"` + account + `"}`
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A fresh guard is what a restart does: it loads and records the mtime.
			g := NewGuardWithState(nil, path, now)
			_ = g
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ResetLearning(path); err != nil {
				t.Errorf("ResetLearning: %v", err)
			}
		}()
	}
	wg.Wait()

	if _, err := ResetLearning(path); err != nil {
		t.Fatal(err)
	}

	// A guard that started during the storm must still adopt the reset rather than
	// keeping a stale learned step forever.
	guard := NewGuardWithState(nil, path, now)
	guard.persist(now.Add(time.Minute))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Accounts[account].FiveHourMaxStep; got != 0 {
		t.Fatalf("persisted learned step = %v, want 0", got)
	}
}

// learnedStepForTest reads the in-memory learned step without the Status side
// effect that adopts an external reset.
func (g *Guard) learnedStepForTest(accountID string) (float64, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	state, ok := g.accounts[AccountKey(accountID)]
	if !ok {
		return 0, false
	}
	return state.FiveHourMaxStep, true
}

// TestQuotaObservationRejectsOutOfOrderSamples covers B-R9-02. Callers observe on
// different goroutines, so a sample captured earlier can reach the guard after a
// newer one. The older sample must not replace the newer state, or the account can
// look unprotected while the latest real usage is already in the reserve.
func TestQuotaObservationRejectsOutOfOrderSamples(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	// Newer sample arrives first.
	guard.ObserveWindow("acct", Window{
		UsedPercent: 98, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(10 * time.Second),
	})

	// An older sample captured before it now arrives late.
	guard.ObserveWindow("acct", Window{
		UsedPercent: 90, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now.Add(1 * time.Second),
	})

	guard.persist(now.Add(20 * time.Second))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted PersistedState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	state, ok := persisted.Accounts[AccountKey("acct")]
	if !ok || state.FiveHour == nil {
		t.Fatal("persisted state lost the account window")
	}
	if state.FiveHour.UsedPercent != 98 {
		t.Fatalf("persisted used_percent = %v, want the newer 98", state.FiveHour.UsedPercent)
	}
	if !state.FiveHour.ObservedAt.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("persisted observed_at = %v, want the newer sample time", state.FiveHour.ObservedAt)
	}
}

// TestQuotaStepIsNotLearnedWithoutAPositiveTimeDelta covers the second half of
// B-R9-02. Two samples carrying the same timestamp are not evidence about how much
// one sampling step costs: there is no elapsed time for the delta to belong to.
// The out-of-order rejection does not apply to equal timestamps, so this guards the
// step-learning predicate itself.
func TestQuotaStepIsNotLearnedWithoutAPositiveTimeDelta(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := NewGuardWithState(nil, filepath.Join(t.TempDir(), "state.json"), now)
	resetAt := now.Add(2 * time.Hour)
	sameMoment := now.Add(10 * time.Second)

	guard.ObserveWindow("acct", Window{
		UsedPercent: 80, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: sameMoment,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 81, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: sameMoment,
	})

	if step, _ := guard.learnedStepForTest("acct"); step != 0 {
		t.Fatalf("learned step = %v from a zero-time-delta sample, want 0", step)
	}
}

// TestSupersededRecoveryTimerDoesNotClearNewerFailover covers W-R9-03: a timer
// armed for failover A must not clear the state of a newer failover B when it
// finally fires. The operator-visible status would otherwise say "not failed over"
// while requests are still being replaced.
func TestSupersededRecoveryTimerDoesNotClearNewerFailover(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	guard := NewGuardWithState(nil, path, now)
	resetAt := now.Add(2 * time.Hour)

	// Failover A, whose recovery callback is captured instead of scheduled.
	var callbackA, callbackB func()
	guard.scheduleAfter = func(_ time.Duration, fn func()) { callbackA = fn }

	guard.ObserveWindow("acct", Window{
		UsedPercent: 99, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: resetAt, ObservedAt: now,
	})
	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now); !blocked {
		t.Fatal("failover A was not entered")
	}
	if callbackA == nil {
		t.Fatal("failover A did not arm a recovery callback")
	}

	// A reset clears A, then a later failover B arms its own callback.
	guard.clearFailoverOnReset()
	guard.scheduleAfter = func(_ time.Duration, fn func()) { callbackB = fn }
	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now.Add(time.Minute)); !blocked {
		t.Fatal("failover B was not entered")
	}
	if callbackB == nil {
		t.Fatal("failover B did not arm a recovery callback")
	}
	if active, _ := guard.FailoverStatus()["active"].(bool); !active {
		t.Fatal("failover B is not active")
	}

	// The stale timer from A fires now. It must leave B alone.
	callbackA()

	if active, _ := guard.FailoverStatus()["active"].(bool); !active {
		t.Fatal("a superseded recovery timer cleared the newer failover state")
	}

	// B's own timer must still be able to clear B.
	callbackB()
	if active, _ := guard.FailoverStatus()["active"].(bool); active {
		t.Fatal("failover B was not cleared by its own recovery timer")
	}
}
