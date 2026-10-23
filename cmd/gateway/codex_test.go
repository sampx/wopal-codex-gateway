package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexFileAtomicWriteUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeCodexFileAtomic(path, "first\n"); err != nil {
		t.Fatalf("writeCodexFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first\n" {
		t.Fatalf("content = %q", data)
	}

	if err := writeCodexFileAtomic(path, "second\n"); err != nil {
		t.Fatalf("rewrite writeCodexFileAtomic: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten: %v", err)
	}
	if string(data) != "second\n" {
		t.Fatalf("rewritten content = %q", data)
	}
	matches, err := filepath.Glob(path + ".wopal-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

func TestCodexCommandRejectsUnknownSubcommand(t *testing.T) {
	err := runCodexCommand("bogus", nil)
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "codex install|restore") {
		t.Fatalf("error = %q, want explicit usage", err)
	}
}
