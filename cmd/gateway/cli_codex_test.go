package main

import (
	"strings"
	"testing"
)

func TestCodexCommandRejectsUnknownSubcommand(t *testing.T) {
	err := runCodexCommand("bogus", nil)
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "codex install|restore") {
		t.Fatalf("error = %q, want explicit usage", err)
	}
}
