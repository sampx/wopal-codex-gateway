package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// projectRootMarker is the file that identifies the project root. It must stay
// present for every runtime layout: the built binary infers the root from its
// own location, and the marker keeps a stray binary from adopting an
// unrelated directory.
const projectRootMarker = "go.mod"

// resolveProjectRoot returns the directory that owns runtime/ and logs/.
func resolveProjectRoot(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve project root: %w", err)
		}
		return abs, nil
	}

	if executable, err := os.Executable(); err == nil {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", ".."))
		if _, err := os.Stat(filepath.Join(candidate, projectRootMarker)); err == nil {
			return candidate, nil
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, projectRootMarker)); err != nil {
		return "", fmt.Errorf("cannot infer project root; pass --project-root")
	}
	return cwd, nil
}

// resolvedRoot is the project root used by control-plane commands. It is filled
// lazily on first use so `help` never touches the filesystem.
var resolvedRoot string

func projectRootDir() (string, error) {
	if resolvedRoot != "" {
		return resolvedRoot, nil
	}
	root, err := resolveProjectRoot("")
	if err != nil {
		return "", err
	}
	resolvedRoot = root
	return root, nil
}

func mustProjectRoot() string {
	root, err := projectRootDir()
	if err != nil {
		return ""
	}
	return root
}

func stateDir() string         { return filepath.Join(mustProjectRoot(), "runtime") }
func logsDir() string          { return filepath.Join(mustProjectRoot(), "logs") }
func configPath() string       { return filepath.Join(stateDir(), "config.json") }
func restoreStatePath() string { return filepath.Join(stateDir(), "codex-restore.json") }
func activityLogPath() string  { return filepath.Join(logsDir(), "gateway.log") }
func serviceLogPath() string   { return filepath.Join(logsDir(), "service.log") }
func serviceErrorLogPath() string {
	return filepath.Join(logsDir(), "service-error.log")
}

// gatewayBinaryPath is the single runtime executable: the LaunchAgent program,
// the `serve` target and the path documented for operators.
func gatewayBinaryPath() string {
	return filepath.Join(stateDir(), "bin", binaryName)
}

func launchAgentPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func launchAgentTemplatePath() string {
	return filepath.Join(mustProjectRoot(), "launchd", launchAgentLabel+".plist.template")
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return os.Getenv("HOME")
}

func codexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(homeDir(), ".codex")
}

func codexConfigPath() string        { return filepath.Join(codexHome(), "config.toml") }
func codexNativeCatalogPath() string { return filepath.Join(codexHome(), "models_cache.json") }
func codexGatewayCatalogPath() string {
	return filepath.Join(codexHome(), "wopal-codex-gateway-models.json")
}

// launchAgentLabel identifies the single user LaunchAgent.
const launchAgentLabel = "com.wopal.codex-gateway"
