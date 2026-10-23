package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForRotatedLogs(t *testing.T, dir string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		backups, err := filepath.Glob(filepath.Join(dir, "gateway-*.log*"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := 0
		for _, backup := range backups {
			if strings.HasSuffix(backup, ".gz") {
				compressed++
			}
		}
		if len(backups) == want && compressed == want {
			return backups
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated logs = %v, want %d compressed backups", backups, want)
		}
		time.Sleep(20 * time.Millisecond)
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
