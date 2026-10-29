package catalog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ArtifactVerificationError marks a failure while comparing a structurally
// valid official baseline with the authoritative native Codex artifact. The
// daemon may recover this class of failure by rebuilding the catalog; baseline
// load/validation errors remain hard startup failures.
type ArtifactVerificationError struct {
	Cause error
}

func (e *ArtifactVerificationError) Error() string {
	if e == nil || e.Cause == nil {
		return "official Codex artifact verification failed"
	}
	return e.Cause.Error()
}

func (e *ArtifactVerificationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsArtifactVerificationError(err error) bool {
	var target *ArtifactVerificationError
	return errors.As(err, &target)
}

// NativeArtifactIdentity identifies the executable that actually carries the
// bundled Codex catalog. CanonicalPath + SHA256 are authoritative; Size and
// ModTimeUnixNano are only cached stat hints for runtime revalidation.
type NativeArtifactIdentity struct {
	CanonicalPath   string `json:"canonicalPath"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size,omitempty"`
	ModTimeUnixNano int64  `json:"modTimeUnixNano,omitempty"`
}

func codexPlatformTarget() (targetTriple, platformPackage, binaryName string, err error) {
	binaryName = "codex"
	if runtime.GOOS == "windows" {
		binaryName = "codex.exe"
	}
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "aarch64-apple-darwin", "@openai/codex-darwin-arm64", binaryName, nil
	case "darwin/amd64":
		return "x86_64-apple-darwin", "@openai/codex-darwin-x64", binaryName, nil
	case "linux/arm64":
		return "aarch64-unknown-linux-musl", "@openai/codex-linux-arm64", binaryName, nil
	case "linux/amd64":
		return "x86_64-unknown-linux-musl", "@openai/codex-linux-x64", binaryName, nil
	case "windows/arm64":
		return "aarch64-pc-windows-msvc", "@openai/codex-win32-arm64", binaryName, nil
	case "windows/amd64":
		return "x86_64-pc-windows-msvc", "@openai/codex-win32-x64", binaryName, nil
	default:
		return "", "", "", fmt.Errorf("unsupported Codex platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// ResolveNativeCodexArtifact resolves the codex entrypoint without executing
// Node or Codex. A native entrypoint is used directly; a JS npm launcher is
// mapped to the platform package's vendor/<target>/bin/codex artifact.
func ResolveNativeCodexArtifact(command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("codex command path is empty")
	}
	abs, err := filepath.Abs(command)
	if err != nil {
		return "", fmt.Errorf("resolve codex command path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve codex command symlink %s: %w", abs, err)
	}
	ext := strings.ToLower(filepath.Ext(canonical))
	if ext != ".js" && ext != ".mjs" && ext != ".cjs" {
		if err := validateNativeArtifact(canonical); err != nil {
			return "", err
		}
		return canonical, nil
	}

	target, platformPackage, binaryName, err := codexPlatformTarget()
	if err != nil {
		return "", err
	}
	packageRoot := filepath.Dir(filepath.Dir(canonical))
	candidates := []string{
		filepath.Join(packageRoot, "vendor", target, "bin", binaryName),
	}
	for dir := packageRoot; ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "node_modules" {
			candidates = append(candidates, filepath.Join(dir, filepath.FromSlash(platformPackage), "vendor", target, "bin", binaryName))
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	for _, candidate := range candidates {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil {
			continue
		}
		if validateNativeArtifact(resolved) == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("native Codex artifact not found for launcher %s (target %s)", canonical, target)
}

func validateNativeArtifact(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat native Codex artifact %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("native Codex artifact is not a regular file: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("native Codex artifact is not executable: %s", path)
	}
	return nil
}

func ComputeNativeArtifactIdentity(path string) (NativeArtifactIdentity, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return NativeArtifactIdentity{}, fmt.Errorf("resolve native Codex artifact %s: %w", path, err)
	}
	if err := validateNativeArtifact(canonical); err != nil {
		return NativeArtifactIdentity{}, err
	}
	file, err := os.Open(canonical)
	if err != nil {
		return NativeArtifactIdentity{}, fmt.Errorf("open native Codex artifact %s: %w", canonical, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return NativeArtifactIdentity{}, fmt.Errorf("hash native Codex artifact %s: %w", canonical, err)
	}
	info, err := file.Stat()
	if err != nil {
		return NativeArtifactIdentity{}, fmt.Errorf("stat native Codex artifact %s: %w", canonical, err)
	}
	return NativeArtifactIdentity{
		CanonicalPath:   canonical,
		SHA256:          fmt.Sprintf("%x", hash.Sum(nil)),
		Size:            info.Size(),
		ModTimeUnixNano: info.ModTime().UnixNano(),
	}, nil
}

func sameNativeArtifactIdentity(left, right NativeArtifactIdentity) bool {
	return left.CanonicalPath == right.CanonicalPath && left.SHA256 != "" && left.SHA256 == right.SHA256
}

type OfficialArtifactVerifier struct {
	mu       sync.Mutex
	baseline NativeArtifactIdentity
	lastInfo os.FileInfo
}

func NewOfficialArtifactVerifier(baseline *OfficialModelsBaseline) (*OfficialArtifactVerifier, error) {
	if baseline == nil || strings.TrimSpace(baseline.Artifact.CanonicalPath) == "" || strings.TrimSpace(baseline.Artifact.SHA256) == "" {
		return nil, fmt.Errorf("official model baseline is unavailable; run catalog refresh")
	}
	return &OfficialArtifactVerifier{baseline: baseline.Artifact}, nil
}

func (v *OfficialArtifactVerifier) Verify() error {
	_, err := v.Check()
	return err
}

// Check verifies the current authoritative artifact and returns the identity
// that was successfully validated. Verification failures are typed so runtime
// recovery can distinguish artifact drift from an invalid baseline.
func (v *OfficialArtifactVerifier) Check() (NativeArtifactIdentity, error) {
	if v == nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: fmt.Errorf("official model verifier is unavailable")}
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	command, err := catalogResolveCodex()
	if err != nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: err}
	}
	artifactPath, err := catalogResolveNativeArtifact(command)
	if err != nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: fmt.Errorf("resolve authoritative native Codex artifact: %w", err)}
	}
	canonical, err := filepath.EvalSymlinks(artifactPath)
	if err != nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: fmt.Errorf("resolve native Codex artifact %s: %w", artifactPath, err)}
	}
	if canonical != v.baseline.CanonicalPath {
		return NativeArtifactIdentity{CanonicalPath: canonical}, &ArtifactVerificationError{Cause: fmt.Errorf("native Codex artifact path changed: baseline=%s current=%s; run catalog refresh", v.baseline.CanonicalPath, canonical)}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: fmt.Errorf("stat native Codex artifact %s: %w", canonical, err)}
	}
	if v.lastInfo != nil && os.SameFile(v.lastInfo, info) && v.lastInfo.Size() == info.Size() && v.lastInfo.ModTime().Equal(info.ModTime()) {
		return v.baseline, nil
	}
	identity, err := catalogComputeNativeIdentity(canonical)
	if err != nil {
		return NativeArtifactIdentity{}, &ArtifactVerificationError{Cause: err}
	}
	if !sameNativeArtifactIdentity(v.baseline, identity) {
		return identity, &ArtifactVerificationError{Cause: fmt.Errorf("native Codex artifact hash changed; run catalog refresh")}
	}
	v.lastInfo = info
	return identity, nil
}
