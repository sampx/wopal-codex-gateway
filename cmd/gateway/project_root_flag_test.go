package main

import (
	"path/filepath"
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestApplyProjectRootFlagPinsControlPlaneCommands(t *testing.T) {
	root := t.TempDir()
	remaining, err := applyProjectRootFlag([]string{"refresh", "--project-root", root})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != "refresh" {
		t.Fatalf("remaining args = %v, want [refresh]", remaining)
	}
	got, err := config.ProjectRootDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(root) {
		t.Fatalf("project root = %q, want %q", got, filepath.Clean(root))
	}
}

func TestApplyProjectRootFlagRejectsMissingValue(t *testing.T) {
	if _, err := applyProjectRootFlag([]string{"refresh", "--project-root"}); err == nil {
		t.Fatal("missing --project-root value was accepted")
	}
}
