package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	officialFiveHourWindowMinutes = 300
	officialWeeklyWindowMinutes   = 10080
	officialFiveHourStopPercent   = 97.0
	officialWeeklyStopPercent     = 99.0

	// The upstream only reports whole percentages, and one sampling step can
	// consume more than one point. The guard therefore stops earlier than the
	// hard limit by the largest step it has actually observed, never less than
	// these margins. The five-hour threshold of 97 with a two-point margin
	// means the worst case ends at 98%, keeping two points in reserve for the
	// app to stay usable. A larger observed step automatically tightens the
	// margin.
	officialFiveHourMinMargin = 2.0
	officialWeeklyMinMargin   = 1.0
)

type officialQuotaWindow struct {
	UsedPercent   float64
	WindowMinutes int
	ResetAt       time.Time
	ObservedAt    time.Time
}

type officialQuotaAccountState struct {
	FiveHour *officialQuotaWindow
	Weekly   *officialQuotaWindow
	// Largest single-step increase ever observed for each window type. These
	// live on the account, not the window, so a reset does not erase what the
	// guard has learned about how much one sampling step can cost.
	FiveHourMaxStep float64 `json:",omitempty"`
	WeeklyMaxStep   float64 `json:",omitempty"`
}

type officialQuotaGuard struct {
	mu              sync.RWMutex
	accounts        map[string]officialQuotaAccountState
	latestAccount   string
	persistencePath string
	logger          *activityLogger
	notifier        *quotaNotifier

	failoverMu     sync.Mutex
	failoverActive bool
	failoverSince  time.Time
	failoverModel  string
	recoveryKey    string

	// scheduleAfter is overridden in tests; production uses time.AfterFunc.
	scheduleAfter func(time.Duration, func())
}

type officialQuotaPersistedState struct {
	Version       int                                  `json:"version"`
	Accounts      map[string]officialQuotaAccountState `json:"accounts,omitempty"`
	LatestAccount string                               `json:"latest_account,omitempty"`
}

func newOfficialQuotaGuard(logger *activityLogger) *officialQuotaGuard {
	return &officialQuotaGuard{accounts: make(map[string]officialQuotaAccountState), logger: logger}
}

func newOfficialQuotaGuardWithState(logger *activityLogger, statePath string, now time.Time) *officialQuotaGuard {
	guard := newOfficialQuotaGuard(logger)
	guard.persistencePath = strings.TrimSpace(statePath)
	guard.loadPersisted(now)
	return guard
}

func quotaAccountKey(accountID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(accountID)))
	return hex.EncodeToString(sum[:])
}

// activeQuotaState drops expired windows but keeps the learned per-step cost.
// Losing that memory at a window boundary would reintroduce exactly the risk
// the margin exists to prevent.
func activeQuotaState(state officialQuotaAccountState, now time.Time) (officialQuotaAccountState, bool) {
	if state.FiveHour != nil && !now.Before(state.FiveHour.ResetAt) {
		state.FiveHour = nil
	}
	if state.Weekly != nil && !now.Before(state.Weekly.ResetAt) {
		state.Weekly = nil
	}
	active := state.FiveHour != nil || state.Weekly != nil
	if !active && state.FiveHourMaxStep == 0 && state.WeeklyMaxStep == 0 {
		return state, false
	}
	return state, true
}

func (g *officialQuotaGuard) loadPersisted(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	raw, err := os.ReadFile(g.persistencePath)
	if err != nil {
		if !os.IsNotExist(err) {
			g.logger.write(map[string]any{"event": "official_quota_state_load_failed", "error": err.Error()})
		}
		return
	}
	var persisted officialQuotaPersistedState
	if json.Unmarshal(raw, &persisted) != nil || persisted.Version != 1 {
		g.logger.write(map[string]any{"event": "official_quota_state_load_failed", "error": "invalid persisted quota state"})
		return
	}
	for key, state := range persisted.Accounts {
		if active, ok := activeQuotaState(state, now); ok {
			g.accounts[key] = active
		}
	}
	if _, ok := g.accounts[persisted.LatestAccount]; ok {
		g.latestAccount = persisted.LatestAccount
	}
	if g.latestAccount == "" {
		for key := range g.accounts {
			g.latestAccount = key
			break
		}
	}
	if len(g.accounts) > 0 {
		g.logger.write(map[string]any{"event": "official_quota_state_restored"})
	}
}

func (g *officialQuotaGuard) persist(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	g.mu.RLock()
	accounts := make(map[string]officialQuotaAccountState, len(g.accounts))
	for key, state := range g.accounts {
		if active, ok := activeQuotaState(state, now); ok {
			accounts[key] = active
		}
	}
	latest := g.latestAccount
	g.mu.RUnlock()
	payload, err := json.Marshal(officialQuotaPersistedState{Version: 1, Accounts: accounts, LatestAccount: latest})
	if err != nil {
		g.logger.write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
	}
	dir := filepath.Dir(g.persistencePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.logger.write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(dir, ".official-quota-state-*.tmp")
	if err != nil {
		g.logger.write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
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
		err = os.Rename(tmpPath, g.persistencePath)
		ok = err == nil
	}
	if !ok {
		g.logger.write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
	}
}

func chatGPTAccountID(req *http.Request) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(req.Header.Get("ChatGPT-Account-ID"))
}

// quotaExemptOfficialModel lists native models that may keep using the official
// provider while the reserve is protected. Only recovery models belong here:
// every other native model, including the internal approval reviewer, is
// metered and must be failovered once the reserve is active.
func quotaExemptOfficialModel(model string) bool {
	switch strings.TrimSpace(model) {
	case "gpt-5.6-luna", "gpt-6-luna":
		return true
	default:
		return false
	}
}

// internalMeteredOfficialModel reports native traffic the user did not request
// directly. It is still metered, so the reserve must fail it over, but it must
// not overwrite the user-visible failover state or fire a second notification.
func internalMeteredOfficialModel(model string) bool {
	return strings.TrimSpace(model) == "codex-auto-review"
}

func (g *officialQuotaGuard) blockedModel(accountID, model string, now time.Time) (bool, string) {
	if quotaExemptOfficialModel(model) {
		return false, ""
	}
	blocked, reason := g.blocked(accountID, now)
	if !blocked {
		return false, ""
	}
	if internalMeteredOfficialModel(model) {
		return true, reason
	}
	g.enterQuotaFailover(model, reason, accountID, now)
	return true, reason
}

// enterQuotaFailover records the automatic provider switch and arms the matching
// recovery notification for the moment the protected window resets. It is called
// only from the model-scoped guard used by the routing paths, so it sees exactly
// the requests that were replaced instead of failing.
func (g *officialQuotaGuard) enterQuotaFailover(requestedModel, reason, accountID string, now time.Time) {
	if g == nil {
		return
	}
	resetAt := g.windowResetAt(accountID, reason)
	key := quotaRecoveryKey(reason, resetAt, requestedModel)

	g.failoverMu.Lock()
	alreadyArmed := g.recoveryKey == key
	g.failoverActive = true
	g.failoverSince = now
	g.failoverModel = requestedModel
	if !alreadyArmed && !resetAt.IsZero() {
		g.recoveryKey = key
	}
	g.failoverMu.Unlock()

	g.notifier.notifyQuotaFailover(requestedModel, officialQuotaFailoverModel, reason, resetAt, now)
	if alreadyArmed || resetAt.IsZero() {
		return
	}

	delay := resetAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	schedule := g.scheduleAfter
	if schedule == nil {
		schedule = func(delay time.Duration, fn func()) { time.AfterFunc(delay, fn) }
	}
	schedule(delay, func() { g.notifyQuotaRecovered(requestedModel, reason, resetAt, key) })
}

func quotaRecoveryKey(reason string, resetAt time.Time, requestedModel string) string {
	return reason + "|" + resetAt.UTC().Format(time.RFC3339) + "|" + requestedModel
}

// notifyQuotaRecovered clears the failover state and tells the user native models
// are usable again. It runs from the timer armed at failover time.
func (g *officialQuotaGuard) notifyQuotaRecovered(requestedModel, reason string, resetAt time.Time, key string) {
	if g == nil {
		return
	}
	g.failoverMu.Lock()
	superseded := g.recoveryKey != key
	g.failoverActive = false
	g.failoverSince = time.Time{}
	g.failoverModel = ""
	g.failoverMu.Unlock()
	if superseded {
		return
	}
	g.notifier.notifyQuotaRecovered(requestedModel, reason, resetAt, time.Now())
}

// failoverStatus reports whether native requests are currently being replaced,
// so users can tell which model their conversation is actually running on.
func (g *officialQuotaGuard) failoverStatus() map[string]any {
	if g == nil {
		return map[string]any{"active": false}
	}
	g.failoverMu.Lock()
	defer g.failoverMu.Unlock()
	status := map[string]any{"active": g.failoverActive}
	if g.failoverActive {
		status["since"] = g.failoverSince.UTC().Format(time.RFC3339)
		status["requested_model"] = g.failoverModel
		status["effective_model"] = officialQuotaFailoverModel
	}
	return status
}

func (g *officialQuotaGuard) windowResetAt(accountID, reason string) time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	state, ok := g.accounts[quotaAccountKey(accountID)]
	if !ok {
		return time.Time{}
	}
	if reason == "weekly" {
		if state.Weekly != nil {
			return state.Weekly.ResetAt
		}
		return time.Time{}
	}
	if state.FiveHour != nil {
		return state.FiveHour.ResetAt
	}
	return time.Time{}
}

func (g *officialQuotaGuard) notificationStatus() map[string]any {
	if g == nil || g.notifier == nil {
		return map[string]any{"enabled": false, "available": false, "tool": ""}
	}
	return g.notifier.status()
}

func (g *officialQuotaGuard) blocked(accountID string, now time.Time) (bool, string) {
	if g == nil || strings.TrimSpace(accountID) == "" {
		return false, ""
	}
	key := quotaAccountKey(accountID)
	g.mu.RLock()
	state, ok := g.accounts[key]
	g.mu.RUnlock()
	if !ok {
		return false, ""
	}
	return quotaStateBlocked(state, now)
}

func quotaStateBlocked(state officialQuotaAccountState, now time.Time) (bool, string) {
	if quotaWindowBlocked(state.FiveHour, officialFiveHourStopPercent, officialFiveHourMinMargin, state.FiveHourMaxStep, now) {
		return true, "five_hour"
	}
	if quotaWindowBlocked(state.Weekly, officialWeeklyStopPercent, officialWeeklyMinMargin, state.WeeklyMaxStep, now) {
		return true, "weekly"
	}
	return false, ""
}

// quotaWindowThreshold lowers the stop point by however much more than the
// base margin a single step has been seen to cost. With whole-percent telemetry
// this is what turns "stop at 97" into a bound on the worst case instead of a
// bound on the observed value.
func quotaWindowThreshold(base, minMargin, maxStep float64) float64 {
	margin := math.Max(minMargin, maxStep)
	return base - (margin - minMargin)
}

func quotaWindowBlocked(window *officialQuotaWindow, base, minMargin, maxStep float64, now time.Time) bool {
	if window == nil || window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return false
	}
	return window.UsedPercent >= quotaWindowThreshold(base, minMargin, maxStep)
}

func (g *officialQuotaGuard) observeHeaders(accountID string, headers http.Header, now time.Time) {
	if g == nil || strings.TrimSpace(accountID) == "" || headers == nil {
		return
	}
	changed := false
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		used, errUsed := strconv.ParseFloat(strings.TrimSpace(headers.Get(prefix+"Used-Percent")), 64)
		minutes, errMinutes := strconv.Atoi(strings.TrimSpace(headers.Get(prefix + "Window-Minutes")))
		if errUsed != nil || errMinutes != nil {
			continue
		}
		resetAt, ok := quotaResetAtFromHeaders(headers, prefix, now)
		if !ok {
			continue
		}
		if g.observeWindow(accountID, officialQuotaWindow{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now}) {
			changed = true
		}
	}
	if changed {
		g.persist(now)
	}
}

func quotaResetAtFromHeaders(headers http.Header, prefix string, now time.Time) (time.Time, bool) {
	if raw := strings.TrimSpace(headers.Get(prefix + "Reset-At")); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && seconds > 0 {
			return time.Unix(seconds, 0), true
		}
	}
	if raw := strings.TrimSpace(headers.Get(prefix + "Reset-After-Seconds")); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && seconds >= 0 {
			return now.Add(time.Duration(seconds) * time.Second), true
		}
	}
	return time.Time{}, false
}

func (g *officialQuotaGuard) observeRateLimitPayload(accountID string, payload []byte, now time.Time) {
	if g == nil || strings.TrimSpace(accountID) == "" || len(payload) == 0 {
		return
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	typeName := strings.TrimSpace(stringOrEmpty(event["type"]))
	if typeName == "error" {
		if rawHeaders, ok := event["headers"].(map[string]any); ok {
			headers := make(http.Header)
			for key, value := range rawHeaders {
				headers.Set(key, fmt.Sprint(value))
			}
			g.observeHeaders(accountID, headers, now)
		}
		return
	}
	if typeName != "codex.rate_limits" {
		return
	}
	rateLimits := objectValue(event, "rate_limits", "rateLimit")
	if rateLimits == nil {
		return
	}
	changed := false
	for _, name := range []string{"primary", "secondary"} {
		window := objectValue(rateLimits, name)
		if window == nil {
			continue
		}
		used, okUsed := numberValue(window, "used_percent", "usedPercent")
		minutes, okMinutes := intValue(window, "window_minutes", "windowMinutes")
		if !okUsed || !okMinutes || used < 0 || used > 100 || minutes <= 0 {
			continue
		}
		resetAt, ok := quotaResetAtFromObject(window, now)
		if !ok {
			continue
		}
		if g.observeWindow(accountID, officialQuotaWindow{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now}) {
			changed = true
		}
	}
	if changed {
		g.persist(now)
	}
}

func quotaResetAtFromObject(window map[string]any, now time.Time) (time.Time, bool) {
	if seconds, ok := int64Value(window, "reset_at", "resetAt"); ok && seconds > 0 {
		return time.Unix(seconds, 0), true
	}
	if seconds, ok := int64Value(window, "reset_after_seconds", "resetAfterSeconds"); ok && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	return time.Time{}, false
}

func (g *officialQuotaGuard) observeWindow(accountID string, window officialQuotaWindow) bool {
	if window.WindowMinutes != officialFiveHourWindowMinutes && window.WindowMinutes != officialWeeklyWindowMinutes {
		return false
	}
	key := quotaAccountKey(accountID)
	g.mu.Lock()
	state := g.accounts[key]
	current := state.Weekly
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		current = state.FiveHour
	}
	if current != nil && current.UsedPercent == window.UsedPercent && current.WindowMinutes == window.WindowMinutes && current.ResetAt.Equal(window.ResetAt) {
		g.latestAccount = key
		g.mu.Unlock()
		return false
	}
	copy := window
	maxStep := 0.0
	step := 0.0
	if current != nil && window.ResetAt.Equal(current.ResetAt) {
		step = window.UsedPercent - current.UsedPercent
	}
	if step > 0 {
		step = math.Min(step, 100)
	}
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		state.FiveHour = &copy
		if step > state.FiveHourMaxStep {
			state.FiveHourMaxStep = step
		}
		maxStep = state.FiveHourMaxStep
	} else {
		state.Weekly = &copy
		if step > state.WeeklyMaxStep {
			state.WeeklyMaxStep = step
		}
		maxStep = state.WeeklyMaxStep
	}
	g.accounts[key] = state
	g.latestAccount = key
	g.mu.Unlock()

	threshold := quotaWindowThreshold(officialWeeklyStopPercent, officialWeeklyMinMargin, maxStep)
	windowName := "weekly"
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		threshold = quotaWindowThreshold(officialFiveHourStopPercent, officialFiveHourMinMargin, maxStep)
		windowName = "five_hour"
	}
	g.logger.write(map[string]any{
		"event":          "official_quota_observed",
		"window":         windowName,
		"used_percent":   window.UsedPercent,
		"stop_percent":   threshold,
		"reset_at":       window.ResetAt.UTC().Format(time.RFC3339),
		"reserve_active": window.UsedPercent >= threshold && window.ObservedAt.Before(window.ResetAt),
	})
	return true
}

func (g *officialQuotaGuard) status(now time.Time) map[string]any {
	result := map[string]any{
		"five_hour_stop_percent": officialFiveHourStopPercent,
		"weekly_stop_percent":    officialWeeklyStopPercent,
		"state":                  "unknown",
	}
	if g == nil {
		return result
	}
	result["failover"] = g.failoverStatus()
	g.mu.RLock()
	accountID := g.latestAccount
	state, ok := g.accounts[accountID]
	g.mu.RUnlock()
	if !ok || accountID == "" {
		return result
	}
	result["state"] = "open"
	if blocked, reason := quotaStateBlocked(state, now); blocked {
		result["state"] = "protected"
		result["reason"] = reason
	}
	if state.FiveHour != nil && now.Before(state.FiveHour.ResetAt) {
		result["five_hour"] = quotaWindowStatus(state.FiveHour)
	}
	if state.Weekly != nil && now.Before(state.Weekly.ResetAt) {
		result["weekly"] = quotaWindowStatus(state.Weekly)
	}
	return result
}

func quotaWindowStatus(window *officialQuotaWindow) map[string]any {
	return map[string]any{
		"used_percent": window.UsedPercent,
		"reset_at":     window.ResetAt.UTC().Format(time.RFC3339),
	}
}

func objectValue(source map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := source[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func numberValue(source map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch value := source[key].(type) {
		case float64:
			return value, true
		case json.Number:
			parsed, err := value.Float64()
			return parsed, err == nil
		}
	}
	return 0, false
}

func intValue(source map[string]any, keys ...string) (int, bool) {
	value, ok := int64Value(source, keys...)
	return int(value), ok
}

func int64Value(source map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch value := source[key].(type) {
		case float64:
			return int64(value), true
		case json.Number:
			parsed, err := value.Int64()
			return parsed, err == nil
		}
	}
	return 0, false
}
