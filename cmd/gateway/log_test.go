package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivityLoggerRotatesInsteadOfGrowing(t *testing.T) {
	root := t.TempDir()
	logger, err := newActivityLogger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.close()

	// Write well past the rotation threshold in the same session; the live file
	// must stay bounded and older data must be parked in backups.
	blob := strings.Repeat("x", 1024)
	for i := 0; i < 12*1024; i++ { // ~12 MiB
		logger.write(map[string]any{"event": "probe", "payload": blob})
	}

	live, err := os.Stat(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	if live.Size() > int64(maxLogSizeMB)<<20 {
		t.Fatalf("live log grew to %d bytes, want <= %d MiB", live.Size(), maxLogSizeMB)
	}
	backups, _ := filepath.Glob(filepath.Join(root, "logs", "gateway-*.log*"))
	if len(backups) == 0 {
		t.Fatal("expected rotated backups")
	}
	if len(backups) > maxLogBackups {
		t.Fatalf("kept %d backups, want <= %d", len(backups), maxLogBackups)
	}
}

func TestAbortHandlerFilterDropsRecoveryNoise(t *testing.T) {
	var out bytes.Buffer
	w := newAbortHandlerFilterWriter(&out)

	noise := "\n\n\x1b[31m[Recovery] panic recovered:\nnet/http: abort Handler\nstack...\x1b[0m"
	if _, err := w.Write([]byte(noise)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "abort Handler") {
		t.Fatalf("abort-handler noise was forwarded: %q", out.String())
	}

	real := "\n\n\x1b[31m[Recovery] panic recovered:\nboom\nstack...\x1b[0m"
	if _, err := w.Write([]byte(real)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Fatalf("genuine panic was dropped: %q", out.String())
	}
}

func TestAbortHandlerFilterForwardsPlainOutput(t *testing.T) {
	var out bytes.Buffer
	w := newAbortHandlerFilterWriter(&out)
	if _, err := w.Write([]byte("plain line\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "plain line\n" {
		t.Fatalf("plain output not forwarded: %q", out.String())
	}
}
