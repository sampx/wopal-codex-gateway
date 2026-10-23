package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/launchd"
)

const testLaunchAgentTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{LABEL}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{PROGRAM_PATH}}</string>
    <string>serve</string>
    <string>--project-root</string>
    <string>{{PROJECT_ROOT}}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>{{LOG_PATH}}</string>
  <key>StandardErrorPath</key>
  <string>{{ERROR_LOG_PATH}}</string>
</dict>
</plist>`

// useTempGatewayRoot points every path helper at a throwaway project root and
// home directory, so tests can exercise install/uninstall without touching the
// user's LaunchAgents folder.
func useTempGatewayRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Cleanup(config.SetProjectRootForTest(root))
	if err := os.MkdirAll(filepath.Dir(config.LaunchAgentTemplatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// prepareServiceInstall writes the two preconditions `service install` now
// checks before it touches launchd: a valid versioned config and a runnable
// binary at the path the plist will reference.
func prepareServiceInstall(t *testing.T) {
	t.Helper()
	root := config.MustProjectRoot()
	if root == "" {
		t.Fatal("test did not set up a project root")
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime", "config.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.GatewayBinaryPath(), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeLaunchAgentTemplate(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(config.LaunchAgentTemplatePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubLaunchctl records every launchd call and lets each test decide the
// result. No test ever reaches the real launchctl binary.
func stubLaunchctl(t *testing.T, respond func(args []string) error) *[][]string {
	t.Helper()
	var calls [][]string
	original := launchd.RunLaunchctl
	launchd.RunLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if respond == nil {
			return nil
		}
		return respond(args)
	}
	t.Cleanup(func() { launchd.RunLaunchctl = original })
	return &calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func stubHealthClient(t *testing.T, status int) {
	t.Helper()
	original := launchd.HealthClient
	launchd.HealthClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { launchd.HealthClient = original })
}

func stubHealthError(t *testing.T, probeErr error) {
	t.Helper()
	original := launchd.HealthClient
	launchd.HealthClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, probeErr
	})}
	t.Cleanup(func() { launchd.HealthClient = original })
}

func stubPortProbeFree(t *testing.T) {
	t.Helper()
	original := launchd.PortProbe
	launchd.PortProbe = func(string, time.Duration) error { return errors.New("connection refused") }
	t.Cleanup(func() { launchd.PortProbe = original })
}

func withHealthTiming(t *testing.T, timeout, retry time.Duration) {
	t.Helper()
	previousTimeout, previousRetry := launchd.HealthWaitTimeout, launchd.HealthRetryDelay
	launchd.HealthWaitTimeout, launchd.HealthRetryDelay = timeout, retry
	t.Cleanup(func() {
		launchd.HealthWaitTimeout, launchd.HealthRetryDelay = previousTimeout, previousRetry
	})
}

func captureStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	runErr := run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestServiceCommandInstallWaitsForHealth(t *testing.T) {
	useTempGatewayRoot(t)
	prepareServiceInstall(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	stubLaunchctl(t, nil)
	stubHealthClient(t, http.StatusOK)
	stubPortProbeFree(t)

	stdout, err := captureStdout(t, func() error { return runServiceCommand("install", nil) })
	if err != nil {
		t.Fatalf("service install: %v", err)
	}
	for _, want := range []string{
		"LaunchAgent installed and proxy started.",
		"Proxy service is healthy: http://127.0.0.1:11435/_health",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("install output %q missing %q", stdout, want)
		}
	}
}

func TestServiceCommandInstallReportsHealthFailure(t *testing.T) {
	useTempGatewayRoot(t)
	prepareServiceInstall(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	stubLaunchctl(t, nil)
	stubHealthError(t, errors.New("connection refused"))
	stubPortProbeFree(t)
	withHealthTiming(t, 60*time.Millisecond, 5*time.Millisecond)

	stdout, err := captureStdout(t, func() error { return runServiceCommand("install", nil) })
	if err == nil {
		t.Fatal("service install succeeded although health checks failed")
	}
	for _, want := range []string{"logs/service.log", "logs/service-error.log", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(stdout, "healthy") {
		t.Fatalf("install claimed success after health failure: %q", stdout)
	}
}

func TestServiceCommandStatusReportsReady(t *testing.T) {
	useTempGatewayRoot(t)
	if err := os.MkdirAll(filepath.Dir(config.LaunchAgentPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.LaunchAgentPath(), []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubLaunchctl(t, nil)
	stubHealthClient(t, http.StatusOK)

	stdout, err := captureStdout(t, func() error { return runServiceCommand("status", nil) })
	if err != nil {
		t.Fatalf("service status: %v", err)
	}
	if stdout != "installed=yes loaded=yes healthy=yes\n" {
		t.Fatalf("status output = %q", stdout)
	}
}

func TestServiceCommandStatusFailsWhenUnloaded(t *testing.T) {
	useTempGatewayRoot(t)
	stubLaunchctl(t, func(args []string) error {
		if args[0] == "print" {
			return errors.New("not loaded")
		}
		return nil
	})
	stubHealthClient(t, http.StatusOK)

	stdout, err := captureStdout(t, func() error { return runServiceCommand("status", nil) })
	if err == nil {
		t.Fatal("service status succeeded while the job was unloaded")
	}
	if stdout != "installed=no loaded=no healthy=yes\n" {
		t.Fatalf("status output = %q", stdout)
	}
	if !strings.Contains(err.Error(), "installed=no") || !strings.Contains(err.Error(), "logs/service.log") {
		t.Fatalf("status error not descriptive: %v", err)
	}
}

func TestServiceCommandRejectsUnknownSubcommand(t *testing.T) {
	err := runServiceCommand("frobnicate", nil)
	if err == nil {
		t.Fatal("unknown service subcommand succeeded")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Fatalf("error %q should be a usage error", err)
	}
}
