package main

import (
	"os"
	"path/filepath"
	"testing"

	sdklogging "github.com/router-for-me/CLIProxyAPI/v8/sdk/logging"
)

func TestBlockSDKRequestLogDirCreatesRegularFile(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "cliproxy-auth")
	if err := blockSDKRequestLogDir(authDir); err != nil {
		t.Fatal(err)
	}

	sink := filepath.Join(authDir, "logs")
	info, err := os.Lstat(sink)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("log sink mode = %v, want a regular file", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log sink permissions = %o, want 600", perm)
	}
}

func TestBlockSDKRequestLogDirIsIdempotentAndReplacesStaleDirectory(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "cliproxy-auth")
	stale := filepath.Join(authDir, "logs", "request-log-parts-request-body-1234")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "body"), []byte("old prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := blockSDKRequestLogDir(authDir); err != nil {
		t.Fatal(err)
	}
	// A second call must not fail on the sink it just created.
	if err := blockSDKRequestLogDir(authDir); err != nil {
		t.Fatalf("second call: %v", err)
	}

	// The sink is now a regular file, so any lookup below it fails with ENOTDIR.
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale request-log directory survived")
	}
	if info, err := os.Lstat(filepath.Join(authDir, "logs")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("log sink is not a regular file: %v %v", info, err)
	}
}

// TestSDKCannotCreateRequestBodySource is the regression that matters: with the
// sink in place the embedded SDK cannot open a body source at all, so no
// request or response body can reach disk.
func TestSDKCannotCreateRequestBodySource(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "cliproxy-auth")
	if err := blockSDKRequestLogDir(authDir); err != nil {
		t.Fatal(err)
	}

	logger := sdklogging.NewFileRequestLogger(false, filepath.Join(authDir, "logs"), "")
	source, err := logger.NewFileBodySource("request-body")
	if err == nil {
		if source != nil {
			_ = source.Cleanup()
		}
		t.Fatal("SDK created a request-body source although the log sink is blocked")
	}

	// Nothing may have been created under the sink path.
	if info, statErr := os.Lstat(filepath.Join(authDir, "logs")); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("sink was replaced after the failed attempt: %v %v", info, statErr)
	}
}

func TestBlockSDKRequestLogDirRejectsSymlinkedSink(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "cliproxy-auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(authDir, "logs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := blockSDKRequestLogDir(authDir); err == nil {
		t.Fatal("a symlinked log sink was accepted")
	}
}
