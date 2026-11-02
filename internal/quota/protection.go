package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ProtectionStateVersion is the only switch-file version the daemon reads.
const ProtectionStateVersion = 1

// ProtectionState is the operator's manual override of official quota
// protection. A missing file means protection is on: the fixed stop points stay
// the default, and only an explicit "off" lets official traffic run to the
// upstream limit.
type ProtectionState struct {
	Version   int       `json:"version"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReadProtectionState reports the override and whether the file exists. A
// missing file is not an error; the caller falls back to protection on.
func ReadProtectionState(path string) (ProtectionState, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ProtectionState{}, false, nil
		}
		return ProtectionState{}, false, fmt.Errorf("read quota protection override: %w", err)
	}
	var stored struct {
		Version   int       `json:"version"`
		Enabled   *bool     `json:"enabled"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return ProtectionState{}, true, fmt.Errorf("parse quota protection override: %w", err)
	}
	if stored.Version != ProtectionStateVersion {
		return ProtectionState{}, true, fmt.Errorf(
			"quota protection override version %d is not supported", stored.Version)
	}
	if stored.Enabled == nil {
		return ProtectionState{}, true, fmt.Errorf("quota protection override is missing enabled")
	}
	return ProtectionState{Version: stored.Version, Enabled: *stored.Enabled, UpdatedAt: stored.UpdatedAt}, true, nil
}

// WriteProtectionState replaces the override atomically. The daemon reads the
// file on its next quota decision, so the switch needs no restart or reload.
func WriteProtectionState(path string, enabled bool, now time.Time) (ProtectionState, error) {
	state := ProtectionState{Version: ProtectionStateVersion, Enabled: enabled, UpdatedAt: now.UTC()}
	payload, err := json.Marshal(state)
	if err != nil {
		return ProtectionState{}, fmt.Errorf("encode quota protection override: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ProtectionState{}, fmt.Errorf("create quota protection directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".quota-protection-*.tmp")
	if err != nil {
		return ProtectionState{}, fmt.Errorf("create quota protection temp file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(payload)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
		ok = err == nil
	}
	if !ok {
		_ = os.Remove(tmpPath)
		return ProtectionState{}, fmt.Errorf("write quota protection override: %w", err)
	}
	return state, nil
}

// protectionCache keeps the override out of the request hot path: the file is
// only re-read when its metadata changes, so a toggle still applies on the next
// request without a stat per decision beyond the one stat that detects it.
type protectionCache struct {
	mu          sync.Mutex
	path        string
	loaded      bool
	modTime     time.Time
	size        int64
	enabled     bool
	hasOverride bool
	updatedAt   time.Time
}

// SetProtectionPath wires the override file into the guard.
func (g *Guard) SetProtectionPath(path string) {
	if g == nil {
		return
	}
	cache := &g.protection
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Set the fields in place: the mutex belongs to the cache and must not be
	// replaced while the call is about to unlock it.
	cache.path = path
	cache.loaded = false
	cache.modTime = time.Time{}
	cache.size = 0
	cache.enabled = true
	cache.hasOverride = false
	cache.updatedAt = time.Time{}
}

// ProtectionEnabled reports the effective switch. Protection is on unless the
// operator file says otherwise, and an unreadable or malformed file keeps the
// protective default.
func (g *Guard) ProtectionEnabled(now time.Time) bool {
	if g == nil {
		return false
	}
	cache := &g.protection
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.path == "" {
		return true
	}
	info, err := os.Stat(cache.path)
	if err != nil {
		if os.IsNotExist(err) {
			if cache.loaded && !cache.enabled {
				g.logProtectionChange(true, time.Time{})
			}
			cache.loaded = true
			cache.enabled = true
			cache.hasOverride = false
			cache.updatedAt = time.Time{}
			return true
		}
		cache.enabled = true
		cache.hasOverride = false
		return true
	}
	if cache.loaded && cache.modTime.Equal(info.ModTime()) && cache.size == info.Size() {
		return cache.enabled
	}
	state, _, readErr := ReadProtectionState(cache.path)
	cache.modTime = info.ModTime()
	cache.size = info.Size()
	cache.loaded = true
	if readErr != nil {
		if g.logger != nil {
			g.logger.Write(map[string]any{"event": "quota_protection_override_unreadable", "error": readErr.Error()})
		}
		cache.enabled = true
		cache.hasOverride = false
		return true
	}
	if cache.enabled != state.Enabled {
		cache.enabled = state.Enabled
		g.logProtectionChange(state.Enabled, state.UpdatedAt)
		if !state.Enabled {
			g.clearFailover()
		}
	}
	cache.updatedAt = state.UpdatedAt
	cache.hasOverride = true
	return cache.enabled
}

// ProtectionStatus reports the switch for /_status without changing it.
func (g *Guard) ProtectionStatus(now time.Time) map[string]any {
	status := map[string]any{"enabled": true, "state": "enabled"}
	if g == nil {
		return status
	}
	enabled := g.ProtectionEnabled(now)
	status["enabled"] = enabled
	if enabled {
		status["state"] = "enabled"
	} else {
		status["state"] = "disabled"
	}
	g.protection.mu.Lock()
	updatedAt := g.protection.updatedAt
	hasOverride := g.protection.hasOverride
	g.protection.mu.Unlock()
	if !updatedAt.IsZero() {
		status["updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	if hasOverride {
		status["source"] = "override"
	} else {
		status["source"] = "default"
	}
	return status
}

// logProtectionChange records each transition once so the log explains why
// official traffic stopped being replaced.
func (g *Guard) logProtectionChange(enabled bool, updatedAt time.Time) {
	if g == nil || g.logger == nil {
		return
	}
	fields := map[string]any{"event": "official_quota_protection_changed", "enabled": enabled}
	if !updatedAt.IsZero() {
		fields["updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	g.logger.Write(fields)
}
