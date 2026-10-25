package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRootMarker creates the marker that identifies a project root.
func writeRootMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, projectRootMarker), []byte("module example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGatewayBinaryPathLivesInBinDir pins the layout contract: released
// executables live in <root>/bin, never in the state directory.
func TestGatewayBinaryPathLivesInBinDir(t *testing.T) {
	root := t.TempDir()
	restore := SetProjectRootForTest(root)
	defer restore()

	want := filepath.Join(root, "bin", BinaryName)
	if got := GatewayBinaryPath(); got != want {
		t.Fatalf("gateway binary path = %q, want %q", got, want)
	}
	if got := StateDir(); got != filepath.Join(root, "runtime") {
		t.Fatalf("state dir = %q, want runtime under root", got)
	}
	if got := LogsDir(); got != filepath.Join(root, "logs") {
		t.Fatalf("logs dir = %q, want logs under root", got)
	}
	if got := RecoveryDir(); got != filepath.Join(root, "recovery") {
		t.Fatalf("recovery dir = %q, want recovery under root", got)
	}
}

// TestSearchProjectRootFindsAncestor covers the released layout: the binary
// sits one level below the root.
func TestSearchProjectRootFindsAncestor(t *testing.T) {
	root := t.TempDir()
	writeRootMarker(t, root)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}

	got, ok := searchProjectRoot(binDir)
	if !ok {
		t.Fatal("searchProjectRoot did not find the marker above bin/")
	}
	if got != root {
		t.Fatalf("searchProjectRoot = %q, want %q", got, root)
	}
}

// TestSearchProjectRootStopsAtDepth keeps a nested build output from walking
// into an unrelated ancestor that happens to hold a go.mod.
func TestSearchProjectRootStopsAtDepth(t *testing.T) {
	outer := t.TempDir()
	writeRootMarker(t, outer)
	deep := filepath.Join(outer, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}

	if got, ok := searchProjectRoot(deep); ok {
		t.Fatalf("searchProjectRoot found %q beyond the depth bound", got)
	}
}

// TestResolveProjectRootPrefersExplicit keeps --project-root authoritative.
func TestResolveProjectRootPrefersExplicit(t *testing.T) {
	explicit := t.TempDir()
	got, err := ResolveProjectRoot(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("ResolveProjectRoot = %q, want %q", got, explicit)
	}
}
