package launchd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
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
    <string>--instance</string>
    <string>{{INSTANCE}}</string>
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
	original := RunLaunchctl
	RunLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if respond == nil {
			return nil
		}
		return respond(args)
	}
	t.Cleanup(func() { RunLaunchctl = original })
	return &calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func stubHealthClient(t *testing.T, status int, body string) {
	t.Helper()
	original := HealthClient
	HealthClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { HealthClient = original })
}

func stubHealthError(t *testing.T, probeErr error) {
	t.Helper()
	original := HealthClient
	HealthClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, probeErr
	})}
	t.Cleanup(func() { HealthClient = original })
}

func withHealthTiming(t *testing.T, timeout, retry time.Duration) {
	t.Helper()
	previousTimeout, previousRetry := HealthWaitTimeout, HealthRetryDelay
	HealthWaitTimeout, HealthRetryDelay = timeout, retry
	t.Cleanup(func() {
		HealthWaitTimeout, HealthRetryDelay = previousTimeout, previousRetry
	})
}

// stubInstanceGate replaces the launch-scoped health gate so orchestration
// tests can assert plist/bootstrap ordering without a real daemon.
func stubInstanceGate(t *testing.T, err error) {
	t.Helper()
	original := WaitForInstanceHook
	WaitForInstanceHook = func(string, int, string) error { return err }
	t.Cleanup(func() { WaitForInstanceHook = original })
}

func stubPortProbe(t *testing.T, probeErr error) {
	t.Helper()
	original := PortProbe
	PortProbe = func(string, time.Duration) error { return probeErr }
	t.Cleanup(func() { PortProbe = original })
}

func withPortTiming(t *testing.T, timeout, retry time.Duration) {
	t.Helper()
	previousTimeout, previousRetry := PortReleaseTimeout, PortReleaseRetryDelay
	PortReleaseTimeout, PortReleaseRetryDelay = timeout, retry
	t.Cleanup(func() {
		PortReleaseTimeout, PortReleaseRetryDelay = previousTimeout, previousRetry
	})
}

func hostPortFromURL(t *testing.T, raw string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Hostname(), port
}

func TestLaunchAgentRenderPlistReplacesAndEscapes(t *testing.T) {
	rendered := RenderLaunchAgentPlist(
		"label={{LABEL}} program={{PROGRAM_PATH}} root={{PROJECT_ROOT}} out={{LOG_PATH}} err={{ERROR_LOG_PATH}}",
		LaunchAgentTemplateValues{
			Label:        config.LaunchAgentLabel,
			ProgramPath:  "/tmp/gateway & <bin>",
			ProjectRoot:  `/tmp/root "quoted" 'single'`,
			LogPath:      "/tmp/out.log",
			ErrorLogPath: "/tmp/err.log",
		},
	)
	want := `label=com.wopal.codex-gateway program=/tmp/gateway &amp; &lt;bin&gt; root=/tmp/root &quot;quoted&quot; &apos;single&apos; out=/tmp/out.log err=/tmp/err.log`
	if rendered != want {
		t.Fatalf("rendered plist = %q, want %q", rendered, want)
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("rendered plist still contains placeholders: %q", rendered)
	}
}

func TestLaunchAgentTemplateProgramArgumentsUseServe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "launchd", config.LaunchAgentLabel+".plist.template"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	start := strings.Index(content, "<key>ProgramArguments</key>")
	if start < 0 {
		t.Fatal("template has no ProgramArguments")
	}
	end := strings.Index(content[start:], "</array>")
	if end < 0 {
		t.Fatal("template has no closing array for ProgramArguments")
	}
	block := content[start : start+end]
	if !strings.Contains(block, "<string>{{PROGRAM_PATH}}</string>\n    <string>serve</string>") {
		t.Fatalf("ProgramArguments must run the binary then `serve`, got:\n%s", block)
	}
	for _, want := range []string{"<string>--project-root</string>", "<string>{{PROJECT_ROOT}}</string>"} {
		if !strings.Contains(block, want) {
			t.Fatalf("ProgramArguments missing %q, got:\n%s", want, block)
		}
	}
}

func TestLaunchAgentInstallWritesPlistAndLoadsJob(t *testing.T) {
	root := useTempGatewayRoot(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	// bootout is expected to fail and must be ignored.
	calls := stubLaunchctl(t, func(args []string) error {
		if args[0] == "bootout" {
			return errors.New("not loaded")
		}
		return nil
	})
	stubPortProbe(t, errors.New("connection refused"))
	stubInstanceGate(t, nil)

	if err := InstallLaunchAgent("127.0.0.1", 11435); err != nil {
		t.Fatalf("installLaunchAgent: %v", err)
	}

	plist, err := os.ReadFile(config.LaunchAgentPath())
	if err != nil {
		t.Fatal(err)
	}
	content := string(plist)
	for _, want := range []string{
		"<string>" + filepath.Join(root, "runtime", "bin", config.BinaryName) + "</string>",
		"<string>serve</string>",
		"<string>" + root + "</string>",
		"<string>" + filepath.Join(root, "logs", "service.log") + "</string>",
		"<string>" + filepath.Join(root, "logs", "service-error.log") + "</string>",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("rendered plist missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "{{") {
		t.Fatalf("rendered plist still contains placeholders:\n%s", content)
	}

	info, err := os.Stat(config.LaunchAgentPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("plist mode = %o, want 644", got)
	}
	for _, dir := range []string{config.StateDir(), config.LogsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, got)
		}
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	wantCalls := [][]string{
		{"bootout", domain, config.LaunchAgentPath()},
		{"bootstrap", domain, config.LaunchAgentPath()},
		{"kickstart", "-k", domain + "/" + config.LaunchAgentLabel},
	}
	if len(*calls) != len(wantCalls) {
		t.Fatalf("launchctl calls = %v, want %v", *calls, wantCalls)
	}
	for index := range wantCalls {
		if strings.Join((*calls)[index], " ") != strings.Join(wantCalls[index], " ") {
			t.Fatalf("launchctl call %d = %v, want %v", index, (*calls)[index], wantCalls[index])
		}
	}
}

func TestLaunchAgentInstallReportsBootstrapFailure(t *testing.T) {
	useTempGatewayRoot(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	calls := stubLaunchctl(t, func(args []string) error {
		if args[0] == "bootstrap" {
			return &launchctlError{exitCode: 42, output: "bootstrap denied"}
		}
		return nil
	})
	stubPortProbe(t, errors.New("connection refused"))

	err := InstallLaunchAgent("127.0.0.1", 11435)
	if err == nil {
		t.Fatal("installLaunchAgent succeeded after bootstrap failed")
	}
	for _, want := range []string{"bootstrap", "exit code 42", "bootstrap denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	for _, call := range *calls {
		if call[0] == "kickstart" {
			t.Fatalf("kickstart ran after bootstrap failed: %v", *calls)
		}
	}
}

func TestLaunchAgentInstallFailsWhenPortStaysBusy(t *testing.T) {
	useTempGatewayRoot(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	calls := stubLaunchctl(t, nil)
	stubPortProbe(t, nil) // no error means a listener answered
	withPortTiming(t, 60*time.Millisecond, 5*time.Millisecond)

	err := InstallLaunchAgent("127.0.0.1", 11435)
	if err == nil {
		t.Fatal("installLaunchAgent succeeded while the port was still in use")
	}
	if !strings.Contains(err.Error(), "port 127.0.0.1:11435 is still in use after stopping the previous service; stop the process holding it and retry") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range *calls {
		if call[0] == "bootstrap" || call[0] == "kickstart" {
			t.Fatalf("install continued after the port stayed busy: %v", *calls)
		}
	}
}

func TestLaunchAgentInstallBootstrapsWhenPortIsFree(t *testing.T) {
	useTempGatewayRoot(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	calls := stubLaunchctl(t, nil)
	stubPortProbe(t, errors.New("connection refused"))
	stubInstanceGate(t, nil)

	if err := InstallLaunchAgent("127.0.0.1", 11435); err != nil {
		t.Fatalf("installLaunchAgent: %v", err)
	}
	var bootstrapped, kickstarted bool
	for _, call := range *calls {
		switch call[0] {
		case "bootstrap":
			bootstrapped = true
		case "kickstart":
			kickstarted = true
		}
	}
	if !bootstrapped || !kickstarted {
		t.Fatalf("install did not bootstrap/kickstart with a free port: %v", *calls)
	}
}

func TestLaunchAgentUninstallBootoutsAndRemovesPlist(t *testing.T) {
	useTempGatewayRoot(t)
	if err := os.MkdirAll(filepath.Dir(config.LaunchAgentPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.LaunchAgentPath(), []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubLaunchctl(t, nil)

	if err := UninstallLaunchAgent(); err != nil {
		t.Fatalf("uninstallLaunchAgent: %v", err)
	}
	if _, err := os.Stat(config.LaunchAgentPath()); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := strings.Join([]string{"bootout", domain, config.LaunchAgentPath()}, " ")
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != want {
		t.Fatalf("launchctl calls = %v, want one %q", *calls, want)
	}

	// Removing an already-absent plist is not an error.
	if err := UninstallLaunchAgent(); err != nil {
		t.Fatalf("second uninstallLaunchAgent: %v", err)
	}
}

func TestWaitForHealthRetriesUntilHealthy(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	withHealthTiming(t, 2*time.Second, 10*time.Millisecond)
	host, port := hostPortFromURL(t, server.URL)
	if err := WaitForHealth(host, port); err != nil {
		t.Fatalf("waitForHealth: %v", err)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("health hits = %d, want at least 2 (retry)", got)
	}
}

func TestWaitForHealthTimesOutWithLastError(t *testing.T) {
	stubHealthClient(t, http.StatusServiceUnavailable, `{"ok":false}`)

	withHealthTiming(t, 150*time.Millisecond, 10*time.Millisecond)
	host, port := "gateway.test", 11435
	started := time.Now()
	err := WaitForHealth(host, port)
	if err == nil {
		t.Fatal("waitForHealth succeeded against a failing server")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("waitForHealth took %s, want roughly 150ms", elapsed)
	}
	for _, want := range []string{HealthURL(host, port), "150ms", "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestWaitForInstanceRejectsHealthyForeignInstance(t *testing.T) {
	stubHealthClient(t, http.StatusOK, `{"ok":true,"instance":"someone-else"}`)

	withHealthTiming(t, 150*time.Millisecond, 10*time.Millisecond)
	host, port := "gateway.test", 11435
	err := WaitForInstance(host, port, "mine")
	if err == nil {
		t.Fatal("WaitForInstance accepted a healthy daemon from another instance")
	}
	for _, want := range []string{"someone-else", "mine"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestWaitForInstanceAcceptsMatchingInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"instance":"mine"}`))
	}))
	defer server.Close()

	withHealthTiming(t, 2*time.Second, 10*time.Millisecond)
	host, port := hostPortFromURL(t, server.URL)
	if err := WaitForInstance(host, port, "mine"); err != nil {
		t.Fatalf("WaitForInstance: %v", err)
	}
}

func TestWaitForInstanceRejectsLegacyHealthWithoutInstance(t *testing.T) {
	stubHealthClient(t, http.StatusOK, `{"ok":true,"mode":"single-process"}`)

	withHealthTiming(t, 150*time.Millisecond, 10*time.Millisecond)
	host, port := "gateway.test", 11435
	err := WaitForInstance(host, port, "mine")
	if err == nil {
		t.Fatal("WaitForInstance accepted a health response without an instance id")
	}
	if !strings.Contains(err.Error(), "no instance id") {
		t.Fatalf("unexpected error: %v", err)
	}
}
