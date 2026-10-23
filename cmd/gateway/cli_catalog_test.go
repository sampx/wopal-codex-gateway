package main

import (
	"strings"
	"testing"
)

func TestCatalogCommandRejectsUnknownSubcommand(t *testing.T) {
	err := runCatalogCommand("status", nil)
	if err == nil {
		t.Fatal("runCatalogCommand() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "catalog refresh") {
		t.Fatalf("error = %q, want catalog refresh usage", err.Error())
	}
}
