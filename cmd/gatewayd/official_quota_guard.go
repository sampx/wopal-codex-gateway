package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	officialFiveHourWindowMinutes = 300
	officialWeeklyWindowMinutes   = 10080
	officialFiveHourStopPercent   = 98.0
	officialWeeklyStopPercent     = 99.0
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
}

type officialQuotaGuard struct {
	mu            sync.RWMutex
	accounts      map[string]officialQuotaAccountState
	latestAccount string
	logger        *activityLogger
}

func newOfficialQuotaGuard(logger *activityLogger) *officialQuotaGuard {
	return &officialQuotaGuard{accounts: make(map[string]officialQuotaAccountState), logger: logger}
}

func chatGPTAccountID(req *http.Request) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(req.Header.Get("ChatGPT-Account-ID"))
}

func quotaExemptOfficialModel(model string) bool {
	switch strings.TrimSpace(model) {
	case "gpt-6-luna", "codex-auto-review":
		return true
	default:
		return false
	}
}

func (g *officialQuotaGuard) blockedModel(accountID, model string, now time.Time) (bool, string) {
	if quotaExemptOfficialModel(model) {
		return false, ""
	}
	return g.blocked(accountID, now)
}

func (g *officialQuotaGuard) blocked(accountID string, now time.Time) (bool, string) {
	if g == nil || strings.TrimSpace(accountID) == "" {
		return false, ""
	}
	g.mu.RLock()
	state, ok := g.accounts[accountID]
	g.mu.RUnlock()
	if !ok {
		return false, ""
	}
	if quotaWindowBlocked(state.FiveHour, officialFiveHourStopPercent, now) {
		return true, "five_hour"
	}
	if quotaWindowBlocked(state.Weekly, officialWeeklyStopPercent, now) {
		return true, "weekly"
	}
	return false, ""
}

func quotaWindowBlocked(window *officialQuotaWindow, threshold float64, now time.Time) bool {
	if window == nil || window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return false
	}
	return window.UsedPercent >= threshold
}

func (g *officialQuotaGuard) observeHeaders(accountID string, headers http.Header, now time.Time) {
	if g == nil || strings.TrimSpace(accountID) == "" || headers == nil {
		return
	}
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
		g.observeWindow(accountID, officialQuotaWindow{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now})
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
		g.observeWindow(accountID, officialQuotaWindow{UsedPercent: used, WindowMinutes: minutes, ResetAt: resetAt, ObservedAt: now})
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

func (g *officialQuotaGuard) observeWindow(accountID string, window officialQuotaWindow) {
	if window.WindowMinutes != officialFiveHourWindowMinutes && window.WindowMinutes != officialWeeklyWindowMinutes {
		return
	}
	g.mu.Lock()
	state := g.accounts[accountID]
	copy := window
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		state.FiveHour = &copy
	} else {
		state.Weekly = &copy
	}
	g.accounts[accountID] = state
	g.latestAccount = accountID
	g.mu.Unlock()

	threshold := officialWeeklyStopPercent
	windowName := "weekly"
	if window.WindowMinutes == officialFiveHourWindowMinutes {
		threshold = officialFiveHourStopPercent
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
	g.mu.RLock()
	accountID := g.latestAccount
	state, ok := g.accounts[accountID]
	g.mu.RUnlock()
	if !ok || accountID == "" {
		return result
	}
	result["state"] = "open"
	if blocked, reason := g.blocked(accountID, now); blocked {
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
