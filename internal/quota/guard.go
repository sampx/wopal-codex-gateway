package quota

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

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/routing"
)

const (
	officialFiveHourWindowMinutes = 300
	officialWeeklyWindowMinutes   = 10080
	FiveHourStopPercent           = 97.0
	WeeklyStopPercent             = 99.0

	// The upstream reports whole percentages, so one sampling step can consume
	// more than one point. The guard stops earlier by the largest step it has
	// actually observed, never less than these margins: with a 2-point five-hour
	// margin the base 97 still leaves the app usable after a 2-point step.
	officialFiveHourMinMargin = 2.0
	officialWeeklyMinMargin   = 1.0

	// officialMaxStepCap bounds how far a single observed step may lower a stop
	// point, which is also the hard floor for the effective thresholds:
	// five-hour 97-(5-2)=94, weekly 99-(5-1)=95. A larger delta is treated as an
	// accumulated gap rather than one step, so the quota can never be pulled
	// arbitrarily low by one noisy sample.
	officialMaxStepCap = 5.0

	// officialMaxStepSampleGap is how close two samples must be to count their
	// delta as one sampling step. A wider gap can span many turns or an idle
	// period, and its delta is not evidence about the next single step.
	officialMaxStepSampleGap = 60 * time.Second
)

type Window struct {
	UsedPercent   float64
	WindowMinutes int
	ResetAt       time.Time
	ObservedAt    time.Time
}

type AccountState struct {
	FiveHour *Window
	Weekly   *Window
	// Largest single-step increase ever observed for each window type. These
	// live on the account, not the window, so a reset does not erase what the
	// guard has learned about how much one sampling step can cost.
	FiveHourMaxStep float64 `json:"five_hour_max_step,omitempty"`
	WeeklyMaxStep   float64 `json:"weekly_max_step,omitempty"`
}

// Notifier surfaces automatic quota transitions to the user. Implementations
// live outside this package so quota never imports a notification transport.
type Notifier interface {
	NotifyQuotaFailover(requestedModel, effectiveModel, reason string, resetAt, now time.Time) bool
	NotifyQuotaRecovered(requestedModel, reason string, resetAt, now time.Time) bool
	Status() map[string]any
}

type Guard struct {
	mu              sync.RWMutex
	accounts        map[string]AccountState
	latestAccount   string
	persistencePath string
	logger          *activity.Logger
	notifier        Notifier

	// lastObserved deduplicates high-frequency quota telemetry by signature.
	lastObservedMu sync.Mutex
	lastObserved   string

	failoverMu     sync.Mutex
	failoverActive bool
	failoverSince  time.Time
	failoverModel  string
	recoveryKey    string

	// scheduleAfter is overridden in tests; production uses time.AfterFunc.
	scheduleAfter func(time.Duration, func())

	// lastPersistModTime is the state file's modification time after the guard's
	// own last write. A newer file means `quota reset-learning` changed it from
	// another process, so the next persist adopts the cleared values instead of
	// writing the in-memory ones back.
	lastPersistModTime time.Time
}

type PersistedState struct {
	Version       int                     `json:"version"`
	Accounts      map[string]AccountState `json:"accounts,omitempty"`
	LatestAccount string                  `json:"latest_account,omitempty"`
}

func NewGuard(logger *activity.Logger) *Guard {
	return &Guard{accounts: make(map[string]AccountState), logger: logger}
}

func NewGuardWithState(logger *activity.Logger, statePath string, now time.Time) *Guard {
	guard := NewGuard(logger)
	guard.persistencePath = strings.TrimSpace(statePath)
	guard.loadPersisted(now)
	return guard
}

// SetNotifier attaches the notification implementation after construction.
func (g *Guard) SetNotifier(n Notifier) {
	if g == nil {
		return
	}
	g.notifier = n
}

func (g *Guard) notifyFailover(requestedModel, effectiveModel, reason string, resetAt, now time.Time) bool {
	if g == nil || g.notifier == nil {
		return false
	}
	return g.notifier.NotifyQuotaFailover(requestedModel, effectiveModel, reason, resetAt, now)
}

func AccountKey(accountID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(accountID)))
	return hex.EncodeToString(sum[:])
}

// activeQuotaState drops expired windows but keeps the learned per-step cost.
// Losing that memory at a window boundary would reintroduce exactly the risk the
// margin exists to prevent.
func activeQuotaState(state AccountState, now time.Time) (AccountState, bool) {
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

// clampMaxStep bounds a learned step to the configured cap. Anything above the
// cap is not trustworthy evidence about a single sampling step.
func clampMaxStep(step float64) float64 {
	if step <= 0 || math.IsNaN(step) {
		return 0
	}
	if step > officialMaxStepCap {
		return officialMaxStepCap
	}
	return step
}

// quotaWindowThreshold lowers a base stop point by however much more than the
// minimum margin a single step has been seen to cost.
func quotaWindowThreshold(base, minMargin, maxStep float64) float64 {
	margin := math.Max(minMargin, clampMaxStep(maxStep))
	return base - (margin - minMargin)
}

func effectiveFiveHourStopPercent(maxStep float64) float64 {
	return quotaWindowThreshold(FiveHourStopPercent, officialFiveHourMinMargin, maxStep)
}

func effectiveWeeklyStopPercent(maxStep float64) float64 {
	return quotaWindowThreshold(WeeklyStopPercent, officialWeeklyMinMargin, maxStep)
}

func (g *Guard) loadPersisted(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	raw, err := os.ReadFile(g.persistencePath)
	if err != nil {
		if !os.IsNotExist(err) {
			g.logger.Write(map[string]any{"event": "official_quota_state_load_failed", "error": err.Error()})
		}
		return
	}
	var persisted PersistedState
	if json.Unmarshal(raw, &persisted) != nil || persisted.Version != 1 {
		g.logger.Write(map[string]any{"event": "official_quota_state_load_failed", "error": "invalid persisted quota state"})
		return
	}
	for key, state := range persisted.Accounts {
		// A value above the cap can only come from the older accumulator logic
		// that mistook an accumulated gap for one step; it is not evidence and
		// must not permanently lower the stop point.
		if state.FiveHourMaxStep > officialMaxStepCap {
			state.FiveHourMaxStep = 0
		}
		if state.WeeklyMaxStep > officialMaxStepCap {
			state.WeeklyMaxStep = 0
		}
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
		g.logger.Write(map[string]any{"event": "official_quota_state_restored"})
	}
}

func (g *Guard) persist(now time.Time) {
	if g == nil || g.persistencePath == "" {
		return
	}
	g.adoptExternalLearningReset()

	g.mu.RLock()
	accounts := make(map[string]AccountState, len(g.accounts))
	for key, state := range g.accounts {
		if active, ok := activeQuotaState(state, now); ok {
			accounts[key] = active
		}
	}
	latest := g.latestAccount
	g.mu.RUnlock()
	payload, err := json.Marshal(PersistedState{Version: 1, Accounts: accounts, LatestAccount: latest})
	if err != nil {
		g.logger.Write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
	}
	dir := filepath.Dir(g.persistencePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.logger.Write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(dir, ".official-quota-state-*.tmp")
	if err != nil {
		g.logger.Write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
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
		g.logger.Write(map[string]any{"event": "official_quota_state_persist_failed", "error": err.Error()})
		return
	}
	if info, statErr := os.Stat(g.persistencePath); statErr == nil {
		g.lastPersistModTime = info.ModTime()
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

func (g *Guard) BlockedModel(accountID, model string, now time.Time) (bool, string) {
	if quotaExemptOfficialModel(model) {
		return false, ""
	}
	blocked, reason := g.Blocked(accountID, now)
	if !blocked {
		return false, ""
	}
	if internalMeteredOfficialModel(model) {
		return true, reason
	}
	g.enterFailover(model, reason, accountID, now)
	return true, reason
}

// enterFailover records the automatic provider switch and arms the matching
// recovery notification for the moment the protected window resets. It is called
// only from the model-scoped guard used by the routing paths, so it sees exactly
// the requests that were replaced instead of failing.
func (g *Guard) enterFailover(requestedModel, reason, accountID string, now time.Time) {
	if g == nil {
		return
	}
	resetAt := g.windowResetAt(accountID, reason)
	key := quotaRecoveryKey(reason, resetAt, requestedModel)

	g.failoverMu.Lock()
	alreadyArmed := g.recoveryKey == key
	firstEntry := !g.failoverActive
	g.failoverActive = true
	g.failoverSince = now
	g.failoverModel = requestedModel
	if !alreadyArmed && !resetAt.IsZero() {
		g.recoveryKey = key
	}
	g.failoverMu.Unlock()

	// The routing path logs every replaced request; record the switch itself once
	// so the log shows one official -> Wopal transition per protection period.
	if firstEntry {
		g.logger.Write(map[string]any{
			"event": "official_quota_failover_entered", "requested_model": requestedModel,
			"effective_model": routing.OfficialQuotaFailoverModel, "reason": reason,
			"reset_at": resetAt.UTC().Format(time.RFC3339),
		})
	}

	notified := g.notifyFailover(requestedModel, routing.OfficialQuotaFailoverModel, reason, resetAt, now)
	if firstEntry {
		g.logger.Write(map[string]any{
			"event": "official_quota_failover_notified", "requested_model": requestedModel,
			"reason": reason, "notified": notified,
		})
	}
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
	schedule(delay, func() { g.notifyRecovered(requestedModel, reason, resetAt, key) })
}

// quotaObservationSignature captures everything an official_quota_observed entry
// reports, so identical samples can be dropped.
func quotaObservationSignature(window string, usedPercent float64, resetAt time.Time, reserveActive bool) string {
	return fmt.Sprintf("%s|%.0f|%s|%t", window, usedPercent, resetAt.UTC().Format(time.RFC3339), reserveActive)
}

func quotaRecoveryKey(reason string, resetAt time.Time, requestedModel string) string {
	return reason + "|" + resetAt.UTC().Format(time.RFC3339) + "|" + requestedModel
}

// notifyRecovered clears the failover state and tells the user native models
// are usable again. It runs from the timer armed at failover time.
func (g *Guard) notifyRecovered(requestedModel, reason string, resetAt time.Time, key string) {
	if g == nil {
		return
	}
	g.failoverMu.Lock()
	superseded := g.recoveryKey != key
	g.failoverActive = false
	g.failoverSince = time.Time{}
	g.failoverModel = ""
	g.failoverMu.Unlock()
	if superseded || g.notifier == nil {
		return
	}
	g.notifier.NotifyQuotaRecovered(requestedModel, reason, resetAt, time.Now())
}

// FailoverStatus reports whether native requests are currently being replaced,
// so users can tell which model their conversation is actually running on.
func (g *Guard) FailoverStatus() map[string]any {
	if g == nil {
		return map[string]any{"active": false}
	}
	g.failoverMu.Lock()
	defer g.failoverMu.Unlock()
	status := map[string]any{"active": g.failoverActive}
	if g.failoverActive {
		status["since"] = g.failoverSince.UTC().Format(time.RFC3339)
		status["requested_model"] = g.failoverModel
		status["effective_model"] = routing.OfficialQuotaFailoverModel
	}
	return status
}

func (g *Guard) windowResetAt(accountID, reason string) time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	state, ok := g.accounts[AccountKey(accountID)]
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

func (g *Guard) NotificationStatus() map[string]any {
	if g == nil || g.notifier == nil {
		return map[string]any{"enabled": false, "available": false, "tool": ""}
	}
	return g.notifier.Status()
}

func (g *Guard) Blocked(accountID string, now time.Time) (bool, string) {
	if g == nil || strings.TrimSpace(accountID) == "" {
		return false, ""
	}
	key := AccountKey(accountID)
	g.mu.RLock()
	state, ok := g.accounts[key]
	g.mu.RUnlock()
	if !ok {
		return false, ""
	}
	return quotaStateBlocked(state, now)
}

func quotaStateBlocked(state AccountState, now time.Time) (bool, string) {
	if quotaWindowBlocked(state.FiveHour, effectiveFiveHourStopPercent(state.FiveHourMaxStep), now) {
		return true, "five_hour"
	}
	if quotaWindowBlocked(state.Weekly, effectiveWeeklyStopPercent(state.WeeklyMaxStep), now) {
		return true, "weekly"
	}
	return false, ""
}

func quotaWindowBlocked(window *Window, threshold float64, now time.Time) bool {
	if window == nil || window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return false
	}
	return window.UsedPercent >= threshold
}

func (g *Guard) ObserveHeaders(accountID string, headers http.Header, now time.Time) {
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
		if g.ObserveWindow(accountID, Window{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now}) {
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

func (g *Guard) ObserveRateLimitPayload(accountID string, payload []byte, now time.Time) {
	if g == nil || strings.TrimSpace(accountID) == "" || len(payload) == 0 {
		return
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	typeName := strings.TrimSpace(routing.StringOrEmpty(event["type"]))
	if typeName == "error" {
		if rawHeaders, ok := event["headers"].(map[string]any); ok {
			headers := make(http.Header)
			for key, value := range rawHeaders {
				headers.Set(key, fmt.Sprint(value))
			}
			g.ObserveHeaders(accountID, headers, now)
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
		if g.ObserveWindow(accountID, Window{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now}) {
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

func (g *Guard) ObserveWindow(accountID string, window Window) bool {
	if window.WindowMinutes != officialFiveHourWindowMinutes && window.WindowMinutes != officialWeeklyWindowMinutes {
		return false
	}
	key := AccountKey(accountID)
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
	// A positive delta between two nearby samples is the only evidence about how
	// much one sampling step costs. A wider gap can span many turns or an idle
	// period, and a delta above the cap cannot be one step at all; both are
	// discarded rather than clamped, so a noisy sample never permanently lowers
	// the stop point.
	step := 0.0
	if current != nil && window.ResetAt.Equal(current.ResetAt) &&
		!window.ObservedAt.IsZero() && !current.ObservedAt.IsZero() &&
		window.ObservedAt.Sub(current.ObservedAt) <= officialMaxStepSampleGap {
		delta := window.UsedPercent - current.UsedPercent
		if delta > 0 && delta <= officialMaxStepCap {
			step = delta
		}
	}
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		state.FiveHour = &copy
		if step > state.FiveHourMaxStep {
			state.FiveHourMaxStep = step
		}
	} else {
		state.Weekly = &copy
		if step > state.WeeklyMaxStep {
			state.WeeklyMaxStep = step
		}
	}
	g.accounts[key] = state
	g.latestAccount = key
	threshold := WeeklyStopPercent
	windowName := "weekly"
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		threshold = effectiveFiveHourStopPercent(state.FiveHourMaxStep)
		windowName = "five_hour"
	} else {
		threshold = effectiveWeeklyStopPercent(state.WeeklyMaxStep)
	}
	g.mu.Unlock()
	// Observe events fire on every upstream handshake, so only log a change in
	// the rounded percentage or the protection state. Identical samples carry no
	// information and used to dominate the activity log.
	reserveActive := window.UsedPercent >= threshold && window.ObservedAt.Before(window.ResetAt)
	signature := quotaObservationSignature(windowName, window.UsedPercent, window.ResetAt, reserveActive)
	g.lastObservedMu.Lock()
	changed := g.lastObserved != signature
	if changed {
		g.lastObserved = signature
	}
	g.lastObservedMu.Unlock()
	if changed {
		g.logger.Write(map[string]any{
			"event":          "official_quota_observed",
			"window":         windowName,
			"used_percent":   window.UsedPercent,
			"stop_percent":   threshold,
			"reset_at":       window.ResetAt.UTC().Format(time.RFC3339),
			"reserve_active": reserveActive,
		})
	}
	return true
}

func (g *Guard) Status(now time.Time) map[string]any {
	result := map[string]any{
		"five_hour_stop_percent":           FiveHourStopPercent,
		"weekly_stop_percent":              WeeklyStopPercent,
		"effective_five_hour_stop_percent": FiveHourStopPercent,
		"effective_weekly_stop_percent":    WeeklyStopPercent,
		"observed_five_hour_max_step":      0.0,
		"observed_weekly_max_step":         0.0,
		"state":                            "unknown",
	}
	if g == nil {
		return result
	}
	result["failover"] = g.FailoverStatus()
	g.mu.RLock()
	accountID := g.latestAccount
	state, ok := g.accounts[accountID]
	g.mu.RUnlock()
	if !ok || accountID == "" {
		return result
	}
	result["observed_five_hour_max_step"] = state.FiveHourMaxStep
	result["observed_weekly_max_step"] = state.WeeklyMaxStep
	result["effective_five_hour_stop_percent"] = effectiveFiveHourStopPercent(state.FiveHourMaxStep)
	result["effective_weekly_stop_percent"] = effectiveWeeklyStopPercent(state.WeeklyMaxStep)
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

func quotaWindowStatus(window *Window) map[string]any {
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
