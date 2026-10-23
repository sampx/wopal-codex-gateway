package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	quotaNotificationGroupID = "wopal-codex-gateway-quota"
	quotaNotificationTimeout = 5 * time.Second
)

type notificationsConfig struct {
	Enabled *bool  `json:"enabled"`
	Sound   string `json:"sound"`
	Tool    string `json:"tool"`
}

func (c notificationsConfig) notificationsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// normalizedSound maps the sentinel values onto "no sound flag"; anything else
// is passed to the notification tool as a macOS sound name.
func (c notificationsConfig) normalizedSound() string {
	sound := strings.TrimSpace(c.Sound)
	if sound == "" {
		return "default"
	}
	if strings.EqualFold(sound, "none") || strings.EqualFold(sound, "off") {
		return ""
	}
	return sound
}

// quotaNotifier surfaces automatic quota failover to the user. It is
// best-effort: a missing tool or a failed notification never affects routing.
type quotaNotifier struct {
	logger *activityLogger

	enabled bool
	tool    string
	sound   string

	run func(ctx context.Context, name string, args ...string) error

	mu       sync.Mutex
	notified map[string]time.Time
}

func newQuotaNotifier(logger *activityLogger, cfg notificationsConfig) *quotaNotifier {
	return &quotaNotifier{
		logger:  logger,
		enabled: cfg.notificationsEnabled(),
		tool:    resolveNotificationTool(cfg.Tool),
		sound:   cfg.normalizedSound(),
		run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		notified: make(map[string]time.Time),
	}
}

// resolveNotificationTool prefers the configured tool, then terminal-notifier on
// PATH. osascript is deliberately not used as a fallback: its notifications are
// attributed to Script Editor and clicking them opens it.
func resolveNotificationTool(configured string) string {
	if trimmed := strings.TrimSpace(configured); trimmed != "" {
		if resolved, err := exec.LookPath(trimmed); err == nil {
			return resolved
		}
		return ""
	}
	if resolved, err := exec.LookPath("terminal-notifier"); err == nil {
		return resolved
	}
	for _, candidate := range []string{"/opt/homebrew/bin/terminal-notifier", "/usr/local/bin/terminal-notifier"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func (n *quotaNotifier) status() map[string]any {
	if n == nil {
		return map[string]any{"enabled": false, "available": false, "tool": ""}
	}
	name := ""
	if n.tool != "" {
		name = filepath.Base(n.tool)
	}
	return map[string]any{"enabled": n.enabled, "available": n.tool != "", "tool": name}
}

// notifyQuotaFailover posts at most one notification per quota protection
// period, window and requested model. It reports whether a notification was
// dispatched; the dispatch itself runs in the background.
func (n *quotaNotifier) notifyQuotaFailover(requestedModel, effectiveModel, reason string, resetAt, now time.Time) bool {
	if n == nil || !n.enabled || classifyModel(requestedModel) != routeOfficial {
		return false
	}
	if n.tool == "" {
		n.logger.write(map[string]any{
			"event": "quota_failover_notify_unavailable", "requested_model": requestedModel, "reason": reason,
		})
		return false
	}
	expiry := resetAt
	if expiry.IsZero() {
		expiry = now.Add(24 * time.Hour)
	}
	if !n.reserve("failover|"+reason+"|"+resetAt.UTC().Format(time.RFC3339)+"|"+requestedModel, expiry, now) {
		return false
	}

	n.dispatch("quota_failover_notified", "quota_failover_notify_failed",
		n.notificationArgs(quotaNotificationTitle(reason), "已自动切换到 Wopal AI 模型继续",
			quotaNotificationMessage(requestedModel, effectiveModel, resetAt, now)),
		map[string]any{
			"requested_model": requestedModel, "effective_model": effectiveModel,
			"reason": reason, "reset_at": resetAt.UTC().Format(time.RFC3339),
		})
	return true
}

// notifyQuotaRecovered tells the user their protected window ended and native
// models are usable again. It fires once per protected window.
func (n *quotaNotifier) notifyQuotaRecovered(requestedModel, reason string, resetAt, now time.Time) bool {
	if n == nil || !n.enabled {
		return false
	}
	if n.tool == "" {
		n.logger.write(map[string]any{
			"event": "quota_recovery_notify_unavailable", "requested_model": requestedModel, "reason": reason,
		})
		return false
	}
	if !n.reserve("recovered|"+reason+"|"+resetAt.UTC().Format(time.RFC3339)+"|"+requestedModel, now.Add(24*time.Hour), now) {
		return false
	}

	n.dispatch("quota_recovery_notified", "quota_recovery_notify_failed",
		n.notificationArgs("官方额度已恢复", "已自动切回官方模型继续", quotaNotificationRecoveryMessage(requestedModel)),
		map[string]any{"requested_model": requestedModel, "reason": reason})
	return true
}

func quotaNotificationRecoveryMessage(requestedModel string) string {
	return requestedModel + " 已恢复可用；本次自动切换结束，下次请求会自动回到官方模型"
}

// dispatch posts one notification without ever blocking or failing the request
// path: the caller decides, this only reports the outcome to the log.
func (n *quotaNotifier) dispatch(successEvent, failureEvent string, args []string, fields map[string]any) {
	tool, run := n.tool, n.run
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaNotificationTimeout)
		defer cancel()
		event := successEvent
		entry := map[string]any{"event": event}
		for key, value := range fields {
			entry[key] = value
		}
		if err := run(ctx, tool, args...); err != nil {
			entry["event"] = failureEvent
			entry["error"] = err.Error()
		}
		n.logger.write(entry)
	}()
}

func (n *quotaNotifier) reserve(key string, expiry, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for existing, expiresAt := range n.notified {
		if !expiresAt.After(now) {
			delete(n.notified, existing)
		}
	}
	if _, seen := n.notified[key]; seen {
		return false
	}
	n.notified[key] = expiry
	return true
}

// notificationArgs builds the terminal-notifier call. No -execute/-open is
// ever passed, so clicking the banner does nothing.
func (n *quotaNotifier) notificationArgs(title, subtitle, message string) []string {
	args := []string{
		"-title", title,
		"-subtitle", subtitle,
		"-message", message,
		"-group", quotaNotificationGroupID,
	}
	if n.sound != "" {
		args = append(args, "-sound", n.sound)
	}
	return args
}

func quotaNotificationTitle(reason string) string {
	if reason == "weekly" {
		return "官方每周额度已进入保护区"
	}
	return "官方 5 小时额度已进入保护区"
}

func quotaNotificationMessage(requestedModel, effectiveModel string, resetAt, now time.Time) string {
	return requestedModel + " → " + effectiveModel + "；额度恢复后（" + formatQuotaResetTime(resetAt, now) + "）自动切回官方模型，任务不中断"
}

func formatQuotaResetTime(resetAt, now time.Time) string {
	if resetAt.IsZero() {
		return "额度重置后"
	}
	local, localNow := resetAt.Local(), now.Local()
	if local.Year() == localNow.Year() && local.YearDay() == localNow.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("01-02 15:04")
}
