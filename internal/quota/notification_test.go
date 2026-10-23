package quota

import (
	"sync"
	"testing"
	"time"
)

type recordedQuotaNotification struct {
	requestedModel string
	effectiveModel string
	reason         string
	resetAt        time.Time
}

type fakeNotifier struct {
	mu         sync.Mutex
	failovers  []recordedQuotaNotification
	recoveries []recordedQuotaNotification
}

func (f *fakeNotifier) NotifyQuotaFailover(requestedModel, effectiveModel, reason string, resetAt, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failovers = append(f.failovers, recordedQuotaNotification{requestedModel, effectiveModel, reason, resetAt})
	return true
}

func (f *fakeNotifier) NotifyQuotaRecovered(requestedModel, reason string, resetAt, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoveries = append(f.recoveries, recordedQuotaNotification{requestedModel, "", reason, resetAt})
	return true
}

func (f *fakeNotifier) Status() map[string]any {
	return map[string]any{"enabled": true, "available": true, "tool": "fake"}
}

func (f *fakeNotifier) snapshot() ([]recordedQuotaNotification, []recordedQuotaNotification) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedQuotaNotification(nil), f.failovers...), append([]recordedQuotaNotification(nil), f.recoveries...)
}

func TestGuardNotifiesOnlyForBlockedOfficialModels(t *testing.T) {
	notifier := &fakeNotifier{}
	guard := NewGuard(nil)
	guard.SetNotifier(notifier)

	now := time.Unix(1_800_000_000, 0)
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_at":1800003600}}}`), now)

	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now); !blocked {
		t.Fatal("official model was not blocked")
	}
	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now); !blocked {
		t.Fatal("official model was not blocked on the second sampling step")
	}
	if blocked, _ := guard.BlockedModel("acct", "gpt-5.6-luna", now); blocked {
		t.Fatal("exempt model was blocked")
	}
	if blocked, _ := guard.BlockedModel("acct", "codex-auto-review", now); !blocked {
		t.Fatal("auto-review was not blocked")
	}

	failovers, recoveries := notifier.snapshot()
	if len(failovers) == 0 {
		t.Fatal("blocked official model produced no failover notification")
	}
	for _, notification := range failovers {
		if notification.requestedModel != "gpt-6.1-sol" || notification.effectiveModel != "wopal-ai/deepseek-v4.1-flash" {
			t.Fatalf("unexpected failover notification = %#v", notification)
		}
	}
	if len(recoveries) != 0 {
		t.Fatalf("unexpected recovery notifications = %#v", recoveries)
	}
}

func TestGuardNotifiesRecoveryWhenTheWindowResets(t *testing.T) {
	notifier := &fakeNotifier{}
	guard := NewGuard(nil)
	guard.SetNotifier(notifier)

	var scheduled []func()
	var delays []time.Duration
	guard.scheduleAfter = func(delay time.Duration, fn func()) {
		delays = append(delays, delay)
		scheduled = append(scheduled, fn)
	}

	now := time.Unix(1_800_000_000, 0)
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_at":1800010800}}}`), now)

	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now); !blocked {
		t.Fatal("official model was not blocked")
	}
	if len(scheduled) != 1 || delays[0] != 3*time.Hour {
		t.Fatalf("scheduled=%d delays=%v", len(scheduled), delays)
	}
	status := guard.FailoverStatus()
	if status["active"] != true || status["requested_model"] != "gpt-6.1-sol" {
		t.Fatalf("failover status = %v", status)
	}

	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now.Add(time.Minute)); !blocked {
		t.Fatal("official model was not blocked on the second sampling step")
	}
	if len(scheduled) != 1 {
		t.Fatalf("second sampling step scheduled another recovery callback: %d", len(scheduled))
	}

	scheduled[0]()
	failovers, recoveries := notifier.snapshot()
	if len(failovers) != 2 || len(recoveries) != 1 || recoveries[0].requestedModel != "gpt-6.1-sol" {
		t.Fatalf("failovers=%#v recoveries=%#v", failovers, recoveries)
	}
	if status := guard.FailoverStatus(); status["active"] != false {
		t.Fatalf("failover still active after recovery: %v", status)
	}
}

func TestGuardSkipsRecoveryWithoutFailover(t *testing.T) {
	notifier := &fakeNotifier{}
	guard := NewGuard(nil)
	guard.SetNotifier(notifier)

	scheduledCount := 0
	guard.scheduleAfter = func(time.Duration, func()) { scheduledCount++ }

	now := time.Unix(1_800_000_000, 0)
	guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":96,"window_minutes":300,"reset_at":1800010800}}}`), now)

	if blocked, _ := guard.BlockedModel("acct", "gpt-6.1-sol", now); blocked {
		t.Fatal("official model was blocked below the threshold")
	}
	if scheduledCount != 0 {
		t.Fatalf("scheduled %d recovery callbacks without a failover", scheduledCount)
	}
	if status := guard.FailoverStatus(); status["active"] != false {
		t.Fatalf("failover status = %v", status)
	}
	failovers, recoveries := notifier.snapshot()
	if len(failovers) != 0 || len(recoveries) != 0 {
		t.Fatalf("unexpected notifications: %#v %#v", failovers, recoveries)
	}
}
