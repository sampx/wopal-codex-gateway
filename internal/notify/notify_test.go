package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/routing"
)

type recordedNotification struct {
	name string
	args []string
}

func newRecordingNotifier(t *testing.T, enabled bool) (*Notifier, chan recordedNotification) {
	t.Helper()
	dispatched := make(chan recordedNotification, 8)
	notifier := NewQuotaNotifier(nil, config.NotificationsConfig{Enabled: boolPtr(enabled)})
	notifier.tool = "/usr/bin/true"
	notifier.run = func(_ context.Context, name string, args ...string) error {
		dispatched <- recordedNotification{name: name, args: append([]string(nil), args...)}
		return nil
	}
	return notifier, dispatched
}

func waitForNotification(t *testing.T, dispatched chan recordedNotification) recordedNotification {
	t.Helper()
	select {
	case record := <-dispatched:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("no notification dispatched")
		return recordedNotification{}
	}
}

// notifications run in the background, so tests collect them as a set instead
// of asserting on dispatch order.
func waitForNotifications(t *testing.T, dispatched chan recordedNotification, count int) []recordedNotification {
	t.Helper()
	collected := make([]recordedNotification, 0, count)
	for len(collected) < count {
		select {
		case record := <-dispatched:
			collected = append(collected, record)
		case <-time.After(2 * time.Second):
			t.Fatalf("collected %d of %d notifications", len(collected), count)
		}
	}
	return collected
}

func expectNoNotification(t *testing.T, dispatched chan recordedNotification) {
	t.Helper()
	select {
	case record := <-dispatched:
		t.Fatalf("unexpected notification: %v", record.args)
	case <-time.After(150 * time.Millisecond):
	}
}

func argValue(args []string, flag string) string {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func boolPtr(value bool) *bool { return &value }

func containsMessage(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func TestQuotaNotifierNotifiesOncePerProtectionPeriod(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * time.Hour)

	for attempt := 0; attempt < 4; attempt++ {
		if !notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
			if attempt == 0 {
				t.Fatal("first failover did not dispatch a notification")
			}
			continue
		}
		if attempt > 0 {
			t.Fatalf("repeated failover dispatched notification on attempt %d", attempt)
		}
	}

	waitForNotification(t, dispatched)
	expectNoNotification(t, dispatched)
}

func TestQuotaNotifierNotifiesAgainForDifferentModel(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * time.Hour)

	if !notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
		t.Fatal("first model did not dispatch a notification")
	}
	if !notifier.NotifyQuotaFailover("gpt-6-astra", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
		t.Fatal("second official model did not dispatch a notification")
	}
	if notifier.NotifyQuotaFailover("gpt-6-astra", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
		t.Fatal("duplicate of the second model dispatched again")
	}

	messages := []string{}
	for _, record := range waitForNotifications(t, dispatched, 2) {
		messages = append(messages, argValue(record.args, "-message"))
	}
	if !containsMessage(messages, "gpt-6.1-sol") || !containsMessage(messages, "gpt-6-astra") {
		t.Fatalf("messages = %v", messages)
	}
	expectNoNotification(t, dispatched)
}

func TestQuotaNotifierSeparatesQuotaWindows(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * time.Hour)

	if !notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
		t.Fatal("five-hour window did not dispatch a notification")
	}
	if !notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "weekly", resetAt, now) {
		t.Fatal("weekly window did not dispatch a notification")
	}

	titles := []string{}
	for _, record := range waitForNotifications(t, dispatched, 2) {
		titles = append(titles, argValue(record.args, "-title"))
	}
	if !containsMessage(titles, "5 小时") || !containsMessage(titles, "每周") {
		t.Fatalf("titles = %v", titles)
	}
}

func TestQuotaNotifierSkipsWhenDisabled(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, false)
	now := time.Unix(1_800_000_000, 0)

	if notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", now.Add(time.Hour), now) {
		t.Fatal("disabled notifier dispatched a notification")
	}
	expectNoNotification(t, dispatched)
}

func TestQuotaNotifierDescribesTheTransition(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * time.Hour)

	if !notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", resetAt, now) {
		t.Fatal("failover did not dispatch a notification")
	}
	record := waitForNotification(t, dispatched)

	if record.name != "/usr/bin/true" {
		t.Fatalf("tool = %q", record.name)
	}
	if got := argValue(record.args, "-subtitle"); !strings.Contains(got, "Wopal AI") {
		t.Fatalf("subtitle = %q", got)
	}
	message := argValue(record.args, "-message")
	if !strings.Contains(message, "gpt-6.1-sol") || !strings.Contains(message, routing.OfficialQuotaFailoverModel) {
		t.Fatalf("message = %q", message)
	}
	if !strings.Contains(message, formatQuotaResetTime(resetAt, now)) {
		t.Fatalf("message does not carry the reset time: %q", message)
	}
	if got := argValue(record.args, "-group"); got != quotaNotificationGroupID {
		t.Fatalf("group = %q", got)
	}
	if got := argValue(record.args, "-sound"); got == "" {
		t.Fatal("notification has no sound configured")
	}
}

func TestQuotaNotifierReportsMissingTool(t *testing.T) {
	root := t.TempDir()
	logger, err := activity.New(filepath.Join(root, "logs"), config.Logs{})
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	notifier := NewQuotaNotifier(logger, config.NotificationsConfig{Enabled: boolPtr(true), Tool: ""})
	notifier.tool = ""
	notifier.run = func(context.Context, string, ...string) error {
		t.Fatal("missing tool must not be executed")
		return nil
	}

	now := time.Unix(1_800_000_000, 0)
	if notifier.NotifyQuotaFailover("gpt-6.1-sol", routing.OfficialQuotaFailoverModel, "five_hour", now.Add(time.Hour), now) {
		t.Fatal("missing tool reported a dispatched notification")
	}
	contents, err := os.ReadFile(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(contents), "quota_failover_notify_unavailable") {
		t.Fatalf("log does not report the missing tool: %s", contents)
	}
}

func TestFormatQuotaResetTime(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 30, 0, 0, time.Local)

	sameDay := time.Date(2026, 10, 18, 17, 6, 0, 0, time.Local)
	if got := formatQuotaResetTime(sameDay, now); got != "17:06" {
		t.Fatalf("same-day reset = %q", got)
	}

	laterDay := time.Date(2026, 10, 25, 7, 5, 0, 0, time.Local)
	if got := formatQuotaResetTime(laterDay, now); got != "10-25 07:05" {
		t.Fatalf("later-day reset = %q", got)
	}

	if got := formatQuotaResetTime(time.Time{}, now); got != "额度重置后" {
		t.Fatalf("unknown reset = %q", got)
	}
}
func TestQuotaNotifierDescribesQuotaRecovery(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)
	resetAt := now.Add(3 * time.Hour)

	if !notifier.NotifyQuotaRecovered("gpt-6.1-sol", "five_hour", resetAt, now.Add(3*time.Hour)) {
		t.Fatal("quota recovery did not dispatch a notification")
	}
	if notifier.NotifyQuotaRecovered("gpt-6.1-sol", "five_hour", resetAt, now.Add(4*time.Hour)) {
		t.Fatal("quota recovery dispatched a second notification for the same window")
	}

	record := waitForNotification(t, dispatched)
	if got := argValue(record.args, "-title"); !strings.Contains(got, "已恢复") {
		t.Fatalf("title = %q", got)
	}
	message := argValue(record.args, "-message")
	if !strings.Contains(message, "gpt-6.1-sol") {
		t.Fatalf("message = %q", message)
	}
	if got := argValue(record.args, "-group"); got != quotaNotificationGroupID {
		t.Fatalf("group = %q", got)
	}
	expectNoNotification(t, dispatched)
}

func TestQuotaNotifierIgnoresNonOfficialModels(t *testing.T) {
	notifier, dispatched := newRecordingNotifier(t, true)
	now := time.Unix(1_800_000_000, 0)

	if notifier.NotifyQuotaFailover("wopal-ai/deepseek-v4.1-flash", routing.OfficialQuotaFailoverModel, "five_hour", now.Add(time.Hour), now) {
		t.Fatal("namespaced model dispatched a quota notification")
	}
	if notifier.NotifyQuotaFailover("other/model", routing.OfficialQuotaFailoverModel, "five_hour", now.Add(time.Hour), now) {
		t.Fatal("disabled namespace dispatched a quota notification")
	}
	expectNoNotification(t, dispatched)
}
