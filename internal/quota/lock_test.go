package quota

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStateFileLockSerializesWriters is the direct guard for B-R9-01. The daemon
// and `quota reset-learning` are separate processes, so the only thing that can
// serialize them is the shared state-file lock. This proves the lock actually
// blocks a second writer instead of being a decorative no-op.
func TestStateFileLockSerializesWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "official-quota-state.json")

	first, err := lockStateFile(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		second, err := lockStateFile(path)
		if err != nil {
			close(acquired)
			return
		}
		close(acquired)
		second.Unlock()
	}()

	select {
	case <-acquired:
		first.Unlock()
		t.Fatal("second writer acquired the state file lock while the first held it")
	case <-time.After(150 * time.Millisecond):
		// Expected: the second writer is waiting.
	}

	first.Unlock()

	select {
	case <-acquired:
		// Expected: releasing the lock lets the second writer in.
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never acquired the lock after it was released")
	}
}

// TestResetLearningWaitsForTheStateFileLock proves the CLI reset honours the same
// lock as the daemon writer rather than bypassing it.
func TestResetLearningWaitsForTheStateFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "official-quota-state.json")
	if err := writeQuotaStateFile(path, PersistedState{
		Version:       1,
		Accounts:      map[string]AccountState{AccountKey("acct"): {FiveHourMaxStep: 3}},
		LatestAccount: AccountKey("acct"),
	}); err != nil {
		t.Fatal(err)
	}

	held, err := lockStateFile(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ResetLearning(path)
		done <- err
	}()

	select {
	case err := <-done:
		held.Unlock()
		t.Fatalf("ResetLearning completed while the state file lock was held (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: the reset is blocked behind the lock.
	}

	held.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ResetLearning after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResetLearning never completed after the lock was released")
	}
}
