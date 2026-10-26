package catalog

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveNativeCodexArtifactUsesCanonicalNativeBinary(t *testing.T) {
	dir := t.TempDir()
	native := filepath.Join(dir, "CodexCLI")
	if err := os.WriteFile(native, []byte("native-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "codex")
	if err := os.Symlink(native, link); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveNativeCodexArtifact(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(native)
	if got != want {
		t.Fatalf("artifact=%q want=%q", got, want)
	}
}

func TestResolveNativeCodexArtifactMapsJSLauncherToPlatformPackage(t *testing.T) {
	target, platformPackage, binaryName, err := codexPlatformTarget()
	if err != nil {
		t.Skipf("platform unsupported by Codex launcher: %v", err)
	}
	nodeModules := filepath.Join(t.TempDir(), "node_modules")
	launcher := filepath.Join(nodeModules, "@openai", "codex", "bin", "codex.js")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(nodeModules, filepath.FromSlash(platformPackage), "vendor", target, "bin", binaryName)
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("platform-native"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveNativeCodexArtifact(launcher)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(native)
	if got != want {
		t.Fatalf("artifact=%q want=%q", got, want)
	}
}

func TestNativeArtifactIdentityUsesSHA256AndCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	native := filepath.Join(dir, "codex")
	content := []byte("authoritative native artifact")
	if err := os.WriteFile(native, content, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, err := ComputeNativeArtifactIdentity(native)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(native)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(content))
	if identity.SHA256 != wantHash || identity.CanonicalPath != canonical || identity.Size != int64(len(content)) {
		t.Fatalf("identity=%+v want path=%q hash=%q size=%d", identity, canonical, wantHash, len(content))
	}
	if identity.ModTimeUnixNano == 0 {
		t.Fatal("identity omitted stat cache metadata")
	}
}

func TestCodexPlatformTargetMatchesRuntime(t *testing.T) {
	target, platformPackage, binaryName, err := codexPlatformTarget()
	if err != nil {
		if runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows" {
			t.Fatalf("supported runtime rejected: %v", err)
		}
		return
	}
	if target == "" || platformPackage == "" || binaryName == "" {
		t.Fatalf("incomplete target: %q %q %q", target, platformPackage, binaryName)
	}
}
