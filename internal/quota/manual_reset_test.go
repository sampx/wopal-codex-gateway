package quota

import (
	"testing"
	"time"
)

func TestOfficialReadingClearsFailoverOnlyWhenBothWindowsRecover(t *testing.T) {
	now := time.Now()
	g := NewGuard(nil)
	var timers []func()
	g.scheduleAfter = func(_ time.Duration, f func()) { timers = append(timers, f) }
	update := func(account string, minutes int, used float64, at time.Time) {
		t.Helper()
		if !g.ObserveWindow(account, Window{UsedPercent: used, WindowMinutes: minutes, ResetAt: now.Add(time.Hour), ObservedAt: at}) {
			t.Fatal("sample rejected")
		}
	}
	update("a", 300, 98, now)
	update("a", 10080, 99, now)
	if blocked, _ := g.BlockedModel("a", "gpt-6.1-terra", now); !blocked {
		t.Fatal("expected protection")
	}
	if blocked, _ := g.BlockedModel("a", "gpt-6-luna", now); blocked {
		t.Fatal("Luna must bypass protection")
	}
	update("other", 300, 0, now.Add(time.Second))
	if g.FailoverStatus()["active"] != true {
		t.Fatal("other account cleared failover")
	}
	update("a", 300, 0, now.Add(2*time.Second))
	if g.FailoverStatus()["active"] != true {
		t.Fatal("weekly window still protected")
	}
	update("a", 10080, 0, now.Add(3*time.Second))
	if g.FailoverStatus()["active"] != false {
		t.Fatal("recovered sample did not clear failover")
	}
	if blocked, _ := g.BlockedModel("a", "gpt-6.1-terra", now.Add(3*time.Second)); blocked {
		t.Fatal("official route still blocked")
	}
	if len(timers) > 0 {
		timers[0]()
		if g.FailoverStatus()["active"] != false {
			t.Fatal("old timer changed recovered state")
		}
	}
}
