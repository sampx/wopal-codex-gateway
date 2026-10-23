package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// learnResetMarker keeps the on-disk contract explicit: only the fields below
// are meaningful for learning, everything else in the state file is owned by the
// daemon.
type quotaLearningView struct {
	FiveHourMaxStep float64 `json:"five_hour_max_step"`
	WeeklyMaxStep   float64 `json:"weekly_max_step"`
}

// adoptExternalLearningReset re-reads the persisted state when another process
// wrote it more recently than this guard. `quota reset-learning` uses that path
// to clear a learned step while the daemon is running; without this check the
// next persist would write the in-memory value straight back.
func (g *officialQuotaGuard) adoptExternalLearningReset() {
	if g == nil || g.persistencePath == "" {
		return
	}
	info, err := os.Stat(g.persistencePath)
	if err != nil {
		return
	}
	if g.lastPersistModTime.IsZero() {
		g.lastPersistModTime = info.ModTime()
		return
	}
	if !info.ModTime().After(g.lastPersistModTime) {
		return
	}

	raw, err := os.ReadFile(g.persistencePath)
	if err != nil {
		return
	}
	var persisted officialQuotaPersistedState
	if json.Unmarshal(raw, &persisted) != nil || persisted.Version != 1 {
		return
	}

	g.mu.Lock()
	for key, state := range g.accounts {
		saved, ok := persisted.Accounts[key]
		if !ok {
			continue
		}
		// Keep the smaller value: the daemon only adopts a reset, never growth
		// written by somebody else.
		state.FiveHourMaxStep = minFloat(state.FiveHourMaxStep, clampMaxStep(saved.FiveHourMaxStep))
		state.WeeklyMaxStep = minFloat(state.WeeklyMaxStep, clampMaxStep(saved.WeeklyMaxStep))
		g.accounts[key] = state
	}
	g.mu.Unlock()
	g.lastPersistModTime = info.ModTime()
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// readQuotaStateFile loads the persisted quota state, returning a typed error
// when the file is missing or unreadable so the CLI can name the path.
func readQuotaStateFile(path string) (officialQuotaPersistedState, error) {
	var state officialQuotaPersistedState
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
func writeQuotaStateFile(path string, state officialQuotaPersistedState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create quota state directory %s: %w", dir, err)
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

// resetQuotaLearning clears every learned step for every account and reports how
// many accounts changed. The daemon picks the change up on its next persist.
func resetQuotaLearning(path string) (int, error) {
	state, err := readQuotaStateFile(path)
	if err != nil {
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

	if err := writeQuotaStateFile(path, state); err != nil {
		return 0, err
	}
	return changed, nil
}
