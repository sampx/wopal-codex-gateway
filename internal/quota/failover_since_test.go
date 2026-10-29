package quota

import (
	"testing"
	"time"
)

func TestOfficialFailoverSinceIsStableWithinOneProtectionCycle(t *testing.T) {
	now := time.Unix(1_900_000_000, 0).UTC()
	guard := NewGuard(nil)
	var callbacks []func()
	guard.scheduleAfter = func(_ time.Duration, fn func()) { callbacks = append(callbacks, fn) }
	guard.ObserveWindow("acct", Window{
		UsedPercent: 98, WindowMinutes: officialFiveHourWindowMinutes,
		ResetAt: now.Add(5 * time.Minute), ObservedAt: now,
	})
	guard.ObserveWindow("acct", Window{
		UsedPercent: 99, WindowMinutes: officialWeeklyWindowMinutes,
		ResetAt: now.Add(time.Hour), ObservedAt: now,
	})

	if blocked, reason := guard.BlockedModelWithTarget("acct", "gpt-first", "vendor/first", now); !blocked || reason != "five_hour" {
		t.Fatalf("first failover = blocked:%v reason:%q", blocked, reason)
	}
	firstSince := guard.FailoverStatus()["since"]
	if firstSince != now.Format(time.RFC3339) {
		t.Fatalf("first since = %v, want %s", firstSince, now.Format(time.RFC3339))
	}

	if blocked, _ := guard.BlockedModelWithTarget("acct", "gpt-second", "vendor/second", now.Add(time.Minute)); !blocked {
		t.Fatal("second request was not protected")
	}
	if got := guard.FailoverStatus()["since"]; got != firstSince {
		t.Fatalf("same-cycle model change refreshed since: first=%v got=%v", firstSince, got)
	}

	later := now.Add(6 * time.Minute)
	if blocked, reason := guard.BlockedModelWithTarget("acct", "gpt-third", "vendor/third", later); !blocked || reason != "weekly" {
		t.Fatalf("weekly failover = blocked:%v reason:%q", blocked, reason)
	}
	if got := guard.FailoverStatus()["since"]; got != firstSince {
		t.Fatalf("same-cycle reason change refreshed since: first=%v got=%v", firstSince, got)
	}

	if len(callbacks) == 0 {
		t.Fatal("failover did not arm a recovery callback")
	}
	callbacks[len(callbacks)-1]()
	if active, _ := guard.FailoverStatus()["active"].(bool); active {
		t.Fatal("recovery did not clear failover state")
	}

	newCycle := later.Add(time.Minute)
	if blocked, _ := guard.BlockedModelWithTarget("acct", "gpt-new-cycle", "vendor/new-cycle", newCycle); !blocked {
		t.Fatal("new protection cycle did not enter failover")
	}
	if got := guard.FailoverStatus()["since"]; got != newCycle.Format(time.RFC3339) {
		t.Fatalf("new cycle since = %v, want %s", got, newCycle.Format(time.RFC3339))
	}
}
