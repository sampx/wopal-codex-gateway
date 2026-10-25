package config

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

// projectRootSearchDepth bounds how far above the executable the root search
// climbs. The released binary lives in <root>/bin/, so the answer is one level
// up; the extra levels keep a nested build output or a scratch path working
// without letting the search adopt a sibling project.
const projectRootSearchDepth = 3

// ResolveProjectRoot returns the directory that owns bin/, runtime/ and logs/.
func ResolveProjectRoot(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve project root: %w", err)
		}
		return abs, nil
	}

	if executable, err := os.Executable(); err == nil {
		if root, ok := searchProjectRoot(filepath.Dir(executable)); ok {
			return root, nil
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

// searchProjectRoot walks from start upward, looking for the project root
// marker. It stops after projectRootSearchDepth levels.
func searchProjectRoot(start string) (string, bool) {
	dir := filepath.Clean(start)
	for range projectRootSearchDepth {
		if _, err := os.Stat(filepath.Join(dir, projectRootMarker)); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

// resolvedRoot is the project root used by control-plane commands. It is filled
// lazily on first use so the help path never touches the filesystem.
var resolvedRoot string

// SetProjectRoot pins the project root for this process. The daemon gets it
// from the LaunchAgent, and the control-plane commands accept the same flag so
// they always act on the runtime the operator named instead of guessing from cwd.
func SetProjectRoot(root string) error {
	if root == "" {
		return fmt.Errorf("project root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	resolvedRoot = abs
	return nil
}

// SetProjectRootForTest replaces the cached project root and returns a restore
// function, so tests can isolate every path helper in a temp directory.
func SetProjectRootForTest(root string) func() {
	previous := resolvedRoot
	resolvedRoot = root
	return func() { resolvedRoot = previous }
}

func ProjectRootDir() (string, error) {
	if resolvedRoot != "" {
		return resolvedRoot, nil
	}
	root, err := ResolveProjectRoot("")
	if err != nil {
		return "", err
	}
	resolvedRoot = root
	return root, nil
}

func MustProjectRoot() string {
	root, err := ProjectRootDir()
	if err != nil {
		return ""
	}
	return root
}

// BinDir owns the released executables. It holds build output, never program
// state, so nothing else is written here.
func BinDir() string      { return filepath.Join(MustProjectRoot(), "bin") }
func StateDir() string    { return filepath.Join(MustProjectRoot(), "runtime") }
func LogsDir() string     { return filepath.Join(MustProjectRoot(), "logs") }
func RecoveryDir() string { return filepath.Join(MustProjectRoot(), "recovery") }

func ConfigPath() string       { return filepath.Join(StateDir(), "config.json") }
func RestoreStatePath() string { return filepath.Join(StateDir(), "codex-restore.json") }
func ActivityLogPath() string  { return filepath.Join(LogsDir(), "gateway.log") }
func ServiceLogPath() string   { return filepath.Join(LogsDir(), "service.log") }
func ServiceErrorLogPath() string {
	return filepath.Join(LogsDir(), "service-error.log")
}

// GatewayBinaryPath is the single released executable: the LaunchAgent program,
// the serve target and the path documented for operators.
func GatewayBinaryPath() string {
	return filepath.Join(BinDir(), BinaryName)
}

func LaunchAgentPath() string {
	return filepath.Join(HomeDir(), "Library", "LaunchAgents", LaunchAgentLabel+".plist")
}

func LaunchAgentTemplatePath() string {
	return filepath.Join(MustProjectRoot(), "launchd", LaunchAgentLabel+".plist.template")
}

func HomeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return os.Getenv("HOME")
}

func CodexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(HomeDir(), ".codex")
}

func CodexConfigPath() string        { return filepath.Join(CodexHome(), "config.toml") }
func CodexNativeCatalogPath() string { return filepath.Join(CodexHome(), "models_cache.json") }
func CodexGatewayCatalogPath() string {
	return filepath.Join(CodexHome(), "wopal-codex-gateway-models.json")
}

// BinaryName is the released executable shared by the CLI, the LaunchAgent
// program path and the operator-facing messages.
const BinaryName = "wopal-codex-gateway"

// LaunchAgentLabel identifies the single user LaunchAgent.
const LaunchAgentLabel = "com.wopal.codex-gateway"
