package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeRequestBodyLogsRemovesOnlyRequestArtifacts(t *testing.T) {
	authDir := t.TempDir()
	logsDir := filepath.Join(authDir, "logs")
	partsDir := filepath.Join(logsDir, "request-log-parts-request-body-123")
	if err := os.MkdirAll(partsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partsDir, "body.part"), []byte("prompt-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	errorLog := filepath.Join(logsDir, "error-v1-responses-2026-10-23T120555-abc.log")
	if err := os.WriteFile(errorLog, []byte("request body"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(logsDir, "keep-me.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := purgeRequestBodyLogs(authDir)
	if result.Files != 1 || result.Dirs != 1 {
		t.Fatalf("files=%d dirs=%d, want 1/1", result.Files, result.Dirs)
	}
	if result.Bytes == 0 {
		t.Fatal("expected non-zero purged bytes")
	}
	if _, err := os.Stat(errorLog); !os.IsNotExist(err) {
		t.Fatalf("error log still present: %v", err)
	}
	if _, err := os.Stat(partsDir); !os.IsNotExist(err) {
		t.Fatalf("request-log-parts dir still present: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}

	again := purgeRequestBodyLogs(authDir)
	if again.Files != 0 || again.Dirs != 0 {
		t.Fatalf("second purge files=%d dirs=%d, want 0/0", again.Files, again.Dirs)
	}
}

func TestPurgeRequestBodyLogsMissingDirectoryIsHarmless(t *testing.T) {
	result := purgeRequestBodyLogs(filepath.Join(t.TempDir(), "absent"))
	if result.Files != 0 || result.Dirs != 0 {
		t.Fatalf("unexpected purge result: %+v", result)
	}
}
