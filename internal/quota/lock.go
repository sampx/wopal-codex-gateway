package quota

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// stateFileLock is the cross-process writer lock for the quota state file.
//
// The daemon persists from many goroutines and `quota reset-learning` writes the
// same file from a separate short-lived process. The in-process persistenceMu
// cannot serialize those two writers, so both take this file lock: whoever holds
// it may read, adopt, snapshot and rename, and the other waits. Without it a
// daemon snapshot taken before a reset could be renamed over the reset result and
// resurrect a cleared value.
type stateFileLock struct {
	file *os.File
}

// lockStateFile blocks until it owns the state-file lock for path.
func lockStateFile(path string) (*stateFileLock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create quota state directory %s for %s: %w", dir, path, err)
	}
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open quota state lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock quota state %s: %w", lockPath, err)
	}
	return &stateFileLock{file: file}, nil
}

// Unlock releases the lock. It is safe to call on a nil receiver so callers can
// defer it unconditionally after a successful lock.
func (l *stateFileLock) Unlock() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
