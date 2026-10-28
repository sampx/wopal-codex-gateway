package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/config"
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

// serve must read the config from the root the operator named, even when the
// process cwd is a different directory with its own config.json.
func TestServeReadsConfigFromProjectRoot(t *testing.T) {
	root := t.TempDir()
	configPath := config.ConfigPathFor(root)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":7,"wopalAi":{"baseUrl":"https://example.test/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	err := runServe([]string{"--project-root", root})
	if err == nil {
		t.Fatal("serve accepted the unsupported rooted config")
	}
	if !strings.Contains(err.Error(), "version 7") || !strings.Contains(err.Error(), configPath) {
		t.Fatalf("serve did not read %s: %v", configPath, err)
	}
}

func TestServeRejectsDuplicateClientVisibleNamesBeforeSideEffects(t *testing.T) {
	root := useTempGatewayRoot(t)
	path := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"one":{"baseUrl":"https://one.example","models":[{"name":"a","alias":"shared"}]},"two":{"baseUrl":"https://two.example","models":[{"name":"b","alias":"shared"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runServe([]string{"--project-root", root, "--config", path})
	if err == nil || !strings.Contains(err.Error(), "duplicate client-visible model") || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("serve error = %v, want duplicate client-visible model", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "logs")); statErr == nil {
		t.Fatal("serve created logs before rejecting duplicate model names")
	}
}
