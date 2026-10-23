package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRejectsUnsupportedConfigBeforeSideEffects(t *testing.T) {
	root := useTempGatewayRoot(t)
	path := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":9,"wopalAi":{"baseUrl":"https://example.test/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runServe([]string{"--project-root", root, "--config", path})
	if err == nil {
		t.Fatal("serve started with an unsupported config version")
	}
	if !strings.Contains(err.Error(), "version 9") {
		t.Fatalf("unexpected error: %v", err)
	}
	// No logs directory means nothing was started before the rejection.
	if _, statErr := os.Stat(filepath.Join(root, "logs")); statErr == nil {
		t.Fatal("serve created logs before rejecting the config")
	}
}

func TestServeFailsWhenSDKLogSinkCannotBeBlocked(t *testing.T) {
	root := useTempGatewayRoot(t)
	path := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(root, "runtime", "cliproxy")
	if err := os.MkdirAll(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(writable, "logs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := runServe([]string{"--project-root", root, "--config", path})
	if err == nil {
		t.Fatal("serve started although the SDK log sink could not be blocked")
	}
	if !strings.Contains(err.Error(), "block CLIProxyAPI writable log sink") {
		t.Fatalf("unexpected error: %v", err)
	}
}
