package launchd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
)

const (
	// healthRequestTimeout bounds every single probe while waiting for launchd
	// to start the daemon.
	healthRequestTimeout = 500 * time.Millisecond
	// StatusHealthTimeout bounds the single probe used by `service status`.
	StatusHealthTimeout = time.Second
	// portProbeTimeout bounds one port-availability dial.
	portProbeTimeout = 200 * time.Millisecond
)

// HealthWaitTimeout and HealthRetryDelay are variables so tests can exercise
// the 90s production schedule without actually waiting 90 seconds.
var (
	HealthWaitTimeout = 90 * time.Second
	HealthRetryDelay  = 250 * time.Millisecond

	// PortReleaseTimeout and PortReleaseRetryDelay bound how long an install
	// waits for the previous listener to disappear after bootout.
	PortReleaseTimeout    = 3 * time.Second
	PortReleaseRetryDelay = 50 * time.Millisecond
)

// HealthClient performs every health probe. Tests replace it so no test needs a
// live gateway.
var HealthClient = &http.Client{Timeout: StatusHealthTimeout}

// InstallLaunchAgentHook, WaitForHealthHook and RequireGatewayBinaryHook are
// seams so a caller can prove the install/restore flow without touching the
// real LaunchAgent.
var (
	InstallLaunchAgentHook   = InstallLaunchAgent
	WaitForHealthHook        = WaitForHealth
	RequireGatewayBinaryHook = RequireGatewayBinary
	UninstallLaunchAgentHook = UninstallLaunchAgent
)

// WaitForInstanceHook is the install-time health gate. It is a variable so
// orchestration tests can isolate plist/bootstrap ordering; WaitForInstance
// itself is covered directly against httptest.
var WaitForInstanceHook = WaitForInstance

// PortProbe reports whether something is listening on addr. It is a seam so
// tests never dial the real gateway port.
var PortProbe = func(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// RunLaunchctl is the only place this binary talks to launchd. Tests replace it
// so no test can bootstrap, kickstart or bootout a real user job.
var RunLaunchctl = func(args ...string) error {
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

// LaunchdDomain is the per-user launchd domain.
func LaunchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// LaunchAgentTarget is the service target used by print/kickstart calls.
func LaunchAgentTarget() string {
	return LaunchdDomain() + "/" + config.LaunchAgentLabel
}

// RequireGatewayBinary makes an install refuse to register a job that launchd
// could never start. Without it `service install` would report a launchd
// success while the daemon is missing.
func RequireGatewayBinary() error {
	path := config.GatewayBinaryPath()
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

// InstallLaunchAgent renders the plist, waits for the configured port to be
// free, bootstraps the job and kickstarts it.
func InstallLaunchAgent(host string, port int) error {
	root, err := config.ProjectRootDir()
	if err != nil {
		return err
	}

	agentsDir := filepath.Dir(config.LaunchAgentPath())
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	for _, dir := range []string{config.StateDir(), config.LogsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	template, err := os.ReadFile(config.LaunchAgentTemplatePath())
	if err != nil {
		return fmt.Errorf("read LaunchAgent template: %w", err)
	}
	// A launch-scoped nonce keeps the health check bound to the daemon this
	// install starts: any 2xx from an unrelated process on the same port does
	// not carry it.
	instance, err := newInstanceID()
	if err != nil {
		return err
	}
	plist := RenderLaunchAgentPlist(string(template), LaunchAgentTemplateValues{
		Instance:     instance,
		Label:        config.LaunchAgentLabel,
		ProgramPath:  config.GatewayBinaryPath(),
		ProjectRoot:  root,
		LogPath:      config.ServiceLogPath(),
		ErrorLogPath: config.ServiceErrorLogPath(),
	})

	if !strings.Contains(plist, instance) {
		return fmt.Errorf("rendered LaunchAgent plist is missing the instance id; check %s for {{INSTANCE}}", config.LaunchAgentTemplatePath())
	}

	path := config.LaunchAgentPath()
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent plist: %w", err)
	}
	// WriteFile keeps the mode of an existing file, so make 0644 explicit.
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("set LaunchAgent plist permissions: %w", err)
	}

	domain := LaunchdDomain()
	// A previous job may still be registered; that is expected before install.
	_ = RunLaunchctl("bootout", domain, path)
	if err := WaitPortAvailable(host, port); err != nil {
		return err
	}
	if err := RunLaunchctl("bootstrap", domain, path); err != nil {
		return fmt.Errorf("launchctl bootstrap %s failed: %w", path, err)
	}
	if err := RunLaunchctl("kickstart", "-k", LaunchAgentTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart %s failed: %w", LaunchAgentTarget(), err)
	}
	// Wait for the daemon to load this exact instance, not just any healthy
	// process that happens to answer on the port.
	if err := WaitForInstanceHook(host, port, instance); err != nil {
		return fmt.Errorf("%w; check %s and %s", err, config.ServiceLogPath(), config.ServiceErrorLogPath())
	}
	return nil
}

// newInstanceID returns a launch-scoped identifier for one `service install`.
func newInstanceID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate install instance id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// WaitPortAvailable polls until the configured address has no listener. A
// launchd install must not report success when an older or unrelated process
// still owns the port and the new job therefore cannot bind it.
func WaitPortAvailable(host string, port int) error {
	addr := net.JoinHostPort(probeHost(host), strconv.Itoa(port))
	deadline := time.Now().Add(PortReleaseTimeout)
	for {
		if err := PortProbe(addr, portProbeTimeout); err != nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		pause := PortReleaseRetryDelay
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
	return fmt.Errorf("port %s:%d is still in use after stopping the previous service; stop the process holding it and retry", host, port)
}

// probeHost maps wildcard bind addresses onto loopback; dialing 0.0.0.0 or ::
// does not reliably probe the process that owns the port.
func probeHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return host
	}
}

// UninstallLaunchAgent boots the job out and removes the plist.
func UninstallLaunchAgent() error {
	path := config.LaunchAgentPath()
	// Not being loaded is fine; the plist still has to be removed.
	_ = RunLaunchctl("bootout", LaunchdDomain(), path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	return nil
}

// LaunchAgentTemplateValues are the placeholder values substituted into the
// plist template.
type LaunchAgentTemplateValues struct {
	// Instance identifies this launch. `service install` generates it, writes it
	// into the plist and then requires /_health to echo it back, so a health
	// 2xx can only come from the daemon this install started.
	Instance     string
	Label        string
	ProgramPath  string
	ProjectRoot  string
	LogPath      string
	ErrorLogPath string
}

// RenderLaunchAgentPlist substitutes every placeholder exactly once and XML
// escapes each value so a path containing & or < cannot break the plist.
func RenderLaunchAgentPlist(template string, values LaunchAgentTemplateValues) string {
	replacer := strings.NewReplacer(
		"{{LABEL}}", xmlEscape(values.Label),
		"{{PROGRAM_PATH}}", xmlEscape(values.ProgramPath),
		"{{PROJECT_ROOT}}", xmlEscape(values.ProjectRoot),
		"{{LOG_PATH}}", xmlEscape(values.LogPath),
		"{{ERROR_LOG_PATH}}", xmlEscape(values.ErrorLogPath),
		"{{INSTANCE}}", xmlEscape(values.Instance),
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

func HealthURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d/_health", host, port)
}

// WaitForHealth blocks until /_health reports a 2xx status or 90s pass.
// InstanceHealth is the /_health payload the installer can verify against.
type InstanceHealth struct {
	OK       bool   `json:"ok"`
	Instance string `json:"instance,omitempty"`
}

// ProbeInstance reads /_health and reports the instance that answered. Any
// non-2xx response is an error, so a startup-in-progress is retried.
func ProbeInstance(url string, timeout time.Duration) (InstanceHealth, error) {
	var health InstanceHealth
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return health, fmt.Errorf("build health request: %w", err)
	}
	response, err := HealthClient.Do(request)
	if err != nil {
		return health, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return health, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	if readErr != nil {
		return health, fmt.Errorf("read health response: %w", readErr)
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return health, fmt.Errorf("health response is not a JSON object: %w", err)
	}
	return health, nil
}

// WaitForInstance waits until /_health reports the expected instance id. A
// different instance id means some other daemon holds the port, which is a
// failure even though its health check succeeded.
func WaitForInstance(host string, port int, instance string) error {
	if instance == "" {
		return WaitForHealth(host, port)
	}
	url := HealthURL(host, port)
	deadline := time.Now().Add(HealthWaitTimeout)
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
		health, err := ProbeInstance(url, requestTimeout)
		switch {
		case err != nil:
			lastErr = err
		case health.Instance == instance:
			return nil
		case health.Instance == "":
			lastErr = fmt.Errorf("health response carries no instance id (expected %s); is an older gateway still serving %s?", instance, url)
		default:
			lastErr = fmt.Errorf("health is answered by instance %s, expected %s", health.Instance, instance)
		}

		pause := HealthRetryDelay
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
	return fmt.Errorf("gateway instance %s did not become healthy at %s within %dms: %v", instance, url, HealthWaitTimeout.Milliseconds(), lastErr)
}

func WaitForHealth(host string, port int) error {
	url := HealthURL(host, port)
	deadline := time.Now().Add(HealthWaitTimeout)
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
		if err := ProbeHealth(url, requestTimeout); err == nil {
			return nil
		} else {
			lastErr = err
		}

		pause := HealthRetryDelay
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
	return fmt.Errorf("gateway service did not become healthy at %s within %dms: %v", url, HealthWaitTimeout.Milliseconds(), lastErr)
}

// ProbeHealth performs one GET and treats only a 2xx response as healthy.
func ProbeHealth(url string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	response, err := HealthClient.Do(request)
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
