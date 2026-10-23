package main

import (
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
	previousRoot := resolvedRoot
	resolvedRoot = root
	t.Cleanup(func() { resolvedRoot = previousRoot })
	if err := os.MkdirAll(filepath.Dir(launchAgentTemplatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeLaunchAgentTemplate(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(launchAgentTemplatePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubLaunchctl records every launchd call and lets each test decide the
// result. No test ever reaches the real launchctl binary.
func stubLaunchctl(t *testing.T, respond func(args []string) error) *[][]string {
	t.Helper()
	var calls [][]string
	original := runLaunchctl
	runLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if respond == nil {
			return nil
		}
		return respond(args)
	}
	t.Cleanup(func() { runLaunchctl = original })
	return &calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func stubHealthClient(t *testing.T, status int) {
	t.Helper()
	original := healthClient
	healthClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    request,
		}, nil
	})}
	t.Cleanup(func() { healthClient = original })
}

func stubHealthError(t *testing.T, probeErr error) {
	t.Helper()
	original := healthClient
	healthClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, probeErr
	})}
	t.Cleanup(func() { healthClient = original })
}

func withHealthTiming(t *testing.T, timeout, retry time.Duration) {
	t.Helper()
	previousTimeout, previousRetry := healthWaitTimeout, healthRetryDelay
	healthWaitTimeout, healthRetryDelay = timeout, retry
	t.Cleanup(func() {
		healthWaitTimeout, healthRetryDelay = previousTimeout, previousRetry
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
	rendered := renderLaunchAgentPlist(
		"label={{LABEL}} program={{PROGRAM_PATH}} root={{PROJECT_ROOT}} out={{LOG_PATH}} err={{ERROR_LOG_PATH}}",
		launchAgentTemplateValues{
			label:        launchAgentLabel,
			programPath:  "/tmp/gateway & <bin>",
			projectRoot:  `/tmp/root "quoted" 'single'`,
			logPath:      "/tmp/out.log",
			errorLogPath: "/tmp/err.log",
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
	body, err := os.ReadFile(filepath.Join("..", "..", "launchd", launchAgentLabel+".plist.template"))
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

	if err := installLaunchAgent(); err != nil {
		t.Fatalf("installLaunchAgent: %v", err)
	}

	plist, err := os.ReadFile(launchAgentPath())
	if err != nil {
		t.Fatal(err)
	}
	content := string(plist)
	for _, want := range []string{
		"<string>" + filepath.Join(root, "runtime", "bin", binaryName) + "</string>",
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

	info, err := os.Stat(launchAgentPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("plist mode = %o, want 644", got)
	}
	for _, dir := range []string{stateDir(), logsDir()} {
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
		{"bootout", domain, launchAgentPath()},
		{"bootstrap", domain, launchAgentPath()},
		{"kickstart", "-k", domain + "/" + launchAgentLabel},
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

	err := installLaunchAgent()
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

func TestLaunchAgentUninstallBootoutsAndRemovesPlist(t *testing.T) {
	useTempGatewayRoot(t)
	if err := os.MkdirAll(filepath.Dir(launchAgentPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchAgentPath(), []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubLaunchctl(t, nil)

	if err := uninstallLaunchAgent(); err != nil {
		t.Fatalf("uninstallLaunchAgent: %v", err)
	}
	if _, err := os.Stat(launchAgentPath()); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := strings.Join([]string{"bootout", domain, launchAgentPath()}, " ")
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != want {
		t.Fatalf("launchctl calls = %v, want one %q", *calls, want)
	}

	// Removing an already-absent plist is not an error.
	if err := uninstallLaunchAgent(); err != nil {
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
	if err := waitForHealth(host, port); err != nil {
		t.Fatalf("waitForHealth: %v", err)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("health hits = %d, want at least 2 (retry)", got)
	}
}

func TestWaitForHealthTimesOutWithLastError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	withHealthTiming(t, 150*time.Millisecond, 10*time.Millisecond)
	host, port := hostPortFromURL(t, server.URL)
	started := time.Now()
	err := waitForHealth(host, port)
	if err == nil {
		t.Fatal("waitForHealth succeeded against a failing server")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("waitForHealth took %s, want roughly 150ms", elapsed)
	}
	for _, want := range []string{server.URL + "/_health", "150ms", "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestServiceCommandInstallWaitsForHealth(t *testing.T) {
	useTempGatewayRoot(t)
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	stubLaunchctl(t, nil)
	stubHealthClient(t, http.StatusOK)

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
	writeLaunchAgentTemplate(t, testLaunchAgentTemplate)
	stubLaunchctl(t, nil)
	stubHealthError(t, errors.New("connection refused"))
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
	if err := os.MkdirAll(filepath.Dir(launchAgentPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchAgentPath(), []byte("plist"), 0o644); err != nil {
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
