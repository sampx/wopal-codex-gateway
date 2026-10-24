package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// learnResetMarker keeps the on-disk contract explicit: only the fields below
// are meaningful for learning, everything else in the state file is owned by the
// daemon.
type quotaLearningView struct {
	FiveHourMaxStep float64 `json:"five_hour_max_step"`
	WeeklyMaxStep   float64 `json:"weekly_max_step"`
}

// resetLearningFromDiskIfNewer re-reads the persisted state when another process
// wrote it more recently than this guard, and applies any cleared learned step to
// the live account state. `quota reset-learning` writes that file from a separate
// short-lived process, so adoption has to happen before live decisions and status
// reports, not only before the next persist.
//
// It is called from every path that reads the learned step (Blocked, Status and
// persist), so a reset becomes effective immediately and cannot be resurrected by
// a later persist. The persistence mutex serializes it against persist.
func (g *Guard) resetLearningFromDiskIfNewer(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	g.persistenceMu.Lock()
	defer g.persistenceMu.Unlock()
	// Hold the cross-process lock while comparing and recording the file mtime, so
	// a concurrent reset cannot land between the stat, the read and the record.
	fileLock, lockErr := lockStateFile(g.persistencePath)
	if lockErr != nil {
		return
	}
	defer fileLock.Unlock()
	g.adoptExternalLearningResetLocked(now)
}

// adoptExternalLearningResetLocked requires persistenceMu to be held.
func (g *Guard) adoptExternalLearningResetLocked(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	info, err := os.Stat(g.persistencePath)
	if err != nil {
		return
	}
	if !info.ModTime().After(g.lastPersistModTime) {
		return
	}

	raw, err := os.ReadFile(g.persistencePath)
	if err != nil {
		return
	}
	var persisted PersistedState
	if json.Unmarshal(raw, &persisted) != nil || persisted.Version != 1 {
		return
	}

	cleared := false
	g.mu.Lock()
	for key, state := range g.accounts {
		saved, ok := persisted.Accounts[key]
		if !ok {
			continue
		}
		// Keep the smaller value: the daemon only adopts a reset, never growth
		// written by somebody else.
		nextFiveHour := minFloat(state.FiveHourMaxStep, g.clampMaxStep(saved.FiveHourMaxStep))
		nextWeekly := minFloat(state.WeeklyMaxStep, g.clampMaxStep(saved.WeeklyMaxStep))
		if nextFiveHour != state.FiveHourMaxStep || nextWeekly != state.WeeklyMaxStep {
			cleared = true
		}
		state.FiveHourMaxStep = nextFiveHour
		state.WeeklyMaxStep = nextWeekly
		g.accounts[key] = state
	}
	accountID := g.latestAccount
	state, hasState := g.accounts[accountID]
	g.mu.Unlock()
	g.lastPersistModTime = info.ModTime()

	if !cleared {
		return
	}

	// The learned step feeds the effective stop point, which decides whether the
	// reserve is active. A reset can therefore end an active failover, so the
	// operator-visible failover state has to follow instead of waiting for the
	// quota window to roll over.
	if hasState {
		if blocked, _ := g.quotaStateBlocked(state, now); !blocked {
			g.clearFailoverOnReset()
		}
	}
	g.logger.Write(map[string]any{"event": "official_quota_learning_adopted"})
}

// clearFailoverOnReset releases the automatic provider switch after a reset makes
// the reserve inactive again.
func (g *Guard) clearFailoverOnReset() {
	g.failoverMu.Lock()
	wasActive := g.failoverActive
	g.failoverActive = false
	g.failoverSince = time.Time{}
	g.failoverModel = ""
	g.recoveryKey = ""
	g.recoveryReason = ""
	g.recoveryModel = ""
	g.recoveryResetAt = time.Time{}
	// Advance the generation so a timer armed for the cleared failover can never be
	// mistaken for the owner of the next one.
	g.failoverSeq++
	g.failoverMu.Unlock()
	if wasActive {
		g.logger.Write(map[string]any{"event": "official_quota_failover_cleared_by_reset"})
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// readQuotaStateFile loads the persisted quota state, returning a typed error
// when the file is missing or unreadable so the CLI can name the path.
func readQuotaStateFile(path string) (PersistedState, error) {
	var state PersistedState
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, fmt.Errorf("read quota state %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("parse quota state %s: %w", path, err)
	}
	if state.Version != 1 {
		return state, fmt.Errorf("unsupported quota state version %d in %s", state.Version, path)
	}
	return state, nil
}

// writeQuotaStateFile replaces the state file atomically with 0600 permissions.
func writeQuotaStateFile(path string, state PersistedState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create quota state directory %s for %s: %w", dir, path, err)
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode quota state: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".official-quota-state-*.tmp")
	if err != nil {
		return fmt.Errorf("write quota state %s: %w", path, err)
	}
	tempPath := temp.Name()
	ok := false
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(payload)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tempPath, path)
		ok = err == nil
	}
	if !ok {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace quota state %s: %w", path, err)
	}
	return nil
}

// ResetLearning clears every learned step for every account and reports how many
// accounts changed. It takes the cross-process state file lock, so a daemon
// snapshot prepared before the reset cannot be renamed over the result afterwards.
// The running daemon adopts the cleared value on its next Blocked, Status or
// persist call, so a successful reset is visible immediately.
func ResetLearning(path string) (int, error) {
	// Serialize with the daemon writer before reading, so the read-modify-write is
	// atomic with respect to a concurrent persist.
	fileLock, lockErr := lockStateFile(path)
	if lockErr != nil {
		return 0, lockErr
	}
	defer fileLock.Unlock()

	state, err := readQuotaStateFile(path)
	if err != nil {
		// No state file means the guard never learned a step, which is already
		// the requested outcome. Only a real read/write failure is an error.
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	changed := 0
	for key, account := range state.Accounts {
		if account.FiveHourMaxStep == 0 && account.WeeklyMaxStep == 0 {
			continue
		}
		account.FiveHourMaxStep = 0
		account.WeeklyMaxStep = 0
		state.Accounts[key] = account
		changed++
	}

	if changed == 0 {
		return 0, nil
	}
	if err := writeQuotaStateFile(path, state); err != nil {
		return 0, fmt.Errorf("clear learned quota steps in %s: %w", path, err)
	}
	return changed, nil
}
