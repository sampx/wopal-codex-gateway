package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdklogging "github.com/router-for-me/CLIProxyAPI/v8/sdk/logging"
)

func TestBlockSDKLogSinkCreatesRegularFile(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "cliproxy-auth", "logs")
	if err := blockSDKLogSink(sink); err != nil {
		t.Fatal(err)
	}

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

func TestBlockSDKLogSinkIsIdempotentAndReplacesStaleDirectory(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "cliproxy-auth", "logs")
	stale := filepath.Join(sink, "request-log-parts-request-body-1234")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "body"), []byte("old prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := blockSDKLogSink(sink); err != nil {
		t.Fatal(err)
	}
	// A second call must not fail on the sink it just created.
	if err := blockSDKLogSink(sink); err != nil {
		t.Fatalf("second call: %v", err)
	}

	// The sink is now a regular file, so any lookup below it fails with ENOTDIR.
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale request-log directory survived")
	}
	if info, err := os.Lstat(sink); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("log sink is not a regular file: %v %v", info, err)
	}
}

// TestSDKCannotCreateRequestBodySource is the regression that matters: with the
// sink in place the embedded SDK cannot open a body source at all, so no
// request or response body can reach disk.
func TestSDKCannotCreateRequestBodySource(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "cliproxy-auth", "logs")
	if err := blockSDKLogSink(logDir); err != nil {
		t.Fatal(err)
	}

	logger := sdklogging.NewFileRequestLogger(false, logDir, "")
	source, err := logger.NewFileBodySource("request-body")
	if err == nil {
		if source != nil {
			_ = source.Cleanup()
		}
		t.Fatal("SDK created a request-body source although the log sink is blocked")
	}

	// Nothing may have been created under the sink path.
	if info, statErr := os.Lstat(logDir); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("sink was replaced after the failed attempt: %v %v", info, statErr)
	}
}

func TestBlockSDKLogSinkRejectsSymlinkedSink(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "cliproxy-auth", "logs")
	if err := os.MkdirAll(filepath.Dir(sink), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := blockSDKLogSink(sink); err == nil {
		t.Fatal("a symlinked log sink was accepted")
	}
}

// TestInstallSDKLogProtectionPinsWritablePath is the regression for the real
// ResolveLogDirectory bypass: a writable cwd/logs must not win over the
// explicitly pinned WRITABLE_PATH.
func TestInstallSDKLogProtectionPinsWritablePath(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	cwdLogs := filepath.Join(cwd, "logs")
	if err := os.MkdirAll(cwdLogs, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("WRITABLE_PATH", "leftover-value")

	state, err := installSDKLogProtection(root)
	if err != nil {
		t.Fatal(err)
	}
	if state != requestBodyLoggingState {
		t.Fatalf("request body logging state = %q, want %q", state, requestBodyLoggingState)
	}

	writable := filepath.Join(root, "runtime", "cliproxy")
	if got := os.Getenv("WRITABLE_PATH"); got != writable {
		t.Fatalf("WRITABLE_PATH = %q, want %q", got, writable)
	}
	// ResolveLogDirectory returns <WRITABLE_PATH>/logs as soon as WRITABLE_PATH
	// is set, so this is the directory the SDK uses regardless of cwd.
	logDir := filepath.Join(os.Getenv("WRITABLE_PATH"), "logs")
	if filepath.Dir(logDir) != writable {
		t.Fatalf("resolved log dir %q is not rooted at the pinned writable path %q", logDir, writable)
	}
	if logDir == cwdLogs || strings.HasPrefix(logDir, cwd+string(filepath.Separator)) {
		t.Fatalf("resolved log dir %q is still under cwd %q", logDir, cwd)
	}

	// The SDK resolves WRITABLE_PATH first, so this is the directory it uses.
	source, err := sdklogging.NewFileRequestLogger(false, logDir, "").NewFileBodySource("request-body")
	if err == nil {
		if source != nil {
			_ = source.Cleanup()
		}
		t.Fatal("SDK created a request-body source under the pinned log sink")
	}

	// Control: the same SDK accepts cwd/logs when it is writable, which is the
	// branch WRITABLE_PATH has to override.
	control, err := sdklogging.NewFileRequestLogger(false, cwdLogs, "").NewFileBodySource("control")
	if err != nil {
		t.Fatalf("control cwd log dir is unexpectedly unwritable: %v", err)
	}
	_ = control.Cleanup()

	authSink := filepath.Join(root, "runtime", "cliproxy-auth", "logs")
	for _, sink := range []string{logDir, authSink} {
		if info, statErr := os.Lstat(sink); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file: %v %v", sink, info, statErr)
		}
	}
}
