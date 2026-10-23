package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// healthRequestTimeout bounds every single probe while waiting for launchd
	// to start the daemon.
	healthRequestTimeout = 500 * time.Millisecond
	// statusHealthTimeout bounds the single probe used by `service status`.
	statusHealthTimeout = time.Second
)

// healthWaitTimeout and healthRetryDelay are variables so tests can exercise
// the 90s production schedule without actually waiting 90 seconds.
var (
	healthWaitTimeout = 90 * time.Second
	healthRetryDelay  = 250 * time.Millisecond
)

// healthClient performs every health probe. Tests replace it so no test needs a
// live gateway.
var healthClient = &http.Client{Timeout: statusHealthTimeout}

// runLaunchctl is the only place this binary talks to launchd. Tests replace it
// so no test can bootstrap, kickstart or bootout a real user job.
var runLaunchctl = func(args ...string) error {
	output, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return &launchctlError{exitCode: exitCode, output: strings.TrimSpace(string(output)), err: err}
}

// launchctlError keeps the exit code and combined output so callers can report
// exactly why launchd refused a job.
type launchctlError struct {
	exitCode int
	output   string
	err      error
}

func (e *launchctlError) Error() string {
	if e.output != "" {
		return fmt.Sprintf("launchctl failed with exit code %d: %s", e.exitCode, e.output)
	}
	if e.err != nil {
		return fmt.Sprintf("launchctl failed with exit code %d: %v", e.exitCode, e.err)
	}
	return fmt.Sprintf("launchctl failed with exit code %d", e.exitCode)
}

// launchdDomain is the per-user launchd domain, matching the TypeScript CLI.
func launchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// launchAgentTarget is the service target used by print/kickstart calls.
func launchAgentTarget() string {
	return launchdDomain() + "/" + launchAgentLabel
}

// requireGatewayBinary makes an install refuse to register a job that launchd
// could never start. Without it `service install` would report a launchd
// success while the daemon is missing.
func requireGatewayBinary() error {
	path := gatewayBinaryPath()
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("gateway binary is missing at %s; build it with: go build -o %s ./cmd/gateway", path, path)
	}
	if info.IsDir() {
		return fmt.Errorf("gateway binary path %s is a directory", path)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("gateway binary at %s is not executable", path)
	}
	return nil
}

// installLaunchAgent renders the plist, bootstraps it and kickstarts the job.
func installLaunchAgent() error {
	root, err := projectRootDir()
	if err != nil {
		return err
	}

	agentsDir := filepath.Dir(launchAgentPath())
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	for _, dir := range []string{stateDir(), logsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	template, err := os.ReadFile(launchAgentTemplatePath())
	if err != nil {
		return fmt.Errorf("read LaunchAgent template: %w", err)
	}
	plist := renderLaunchAgentPlist(string(template), launchAgentTemplateValues{
		label:        launchAgentLabel,
		programPath:  gatewayBinaryPath(),
		projectRoot:  root,
		logPath:      serviceLogPath(),
		errorLogPath: serviceErrorLogPath(),
	})

	path := launchAgentPath()
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent plist: %w", err)
	}
	// WriteFile keeps the mode of an existing file, so make 0644 explicit.
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("set LaunchAgent plist permissions: %w", err)
	}

	domain := launchdDomain()
	// A previous job may still be registered; that is expected before install.
	_ = runLaunchctl("bootout", domain, path)
	if err := runLaunchctl("bootstrap", domain, path); err != nil {
		return fmt.Errorf("launchctl bootstrap %s failed: %w", path, err)
	}
	if err := runLaunchctl("kickstart", "-k", launchAgentTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart %s failed: %w", launchAgentTarget(), err)
	}
	return nil
}

// uninstallLaunchAgent boots the job out and removes the plist.
func uninstallLaunchAgent() error {
	path := launchAgentPath()
	// Not being loaded is fine; the plist still has to be removed.
	_ = runLaunchctl("bootout", launchdDomain(), path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	return nil
}

// launchAgentTemplateValues are the placeholder values substituted into the
// plist template.
type launchAgentTemplateValues struct {
	label        string
	programPath  string
	projectRoot  string
	logPath      string
	errorLogPath string
}

// renderLaunchAgentPlist substitutes every placeholder exactly once and XML
// escapes each value so a path containing & or < cannot break the plist.
func renderLaunchAgentPlist(template string, values launchAgentTemplateValues) string {
	replacer := strings.NewReplacer(
		"{{LABEL}}", xmlEscape(values.label),
		"{{PROGRAM_PATH}}", xmlEscape(values.programPath),
		"{{PROJECT_ROOT}}", xmlEscape(values.projectRoot),
		"{{LOG_PATH}}", xmlEscape(values.logPath),
		"{{ERROR_LOG_PATH}}", xmlEscape(values.errorLogPath),
	)
	return replacer.Replace(template)
}

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlEscape(value string) string {
	return xmlEscaper.Replace(value)
}

func healthURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d/_health", host, port)
}

// waitForHealth blocks until /_health reports a 2xx status or 90s pass.
func waitForHealth(host string, port int) error {
	url := healthURL(host, port)
	deadline := time.Now().Add(healthWaitTimeout)
	var lastErr error

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		requestTimeout := healthRequestTimeout
		if remaining < requestTimeout {
			requestTimeout = remaining
		}
		if err := probeHealth(url, requestTimeout); err == nil {
			return nil
		} else {
			lastErr = err
		}

		pause := healthRetryDelay
		if left := time.Until(deadline); left < pause {
			pause = left
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no health request completed")
	}
	return fmt.Errorf("gateway service did not become healthy at %s within %dms: %v", url, healthWaitTimeout.Milliseconds(), lastErr)
}

// probeHealth performs one GET and treats only a 2xx response as healthy.
func probeHealth(url string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	response, err := healthClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	return nil
}
