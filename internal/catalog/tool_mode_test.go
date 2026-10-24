package catalog

import (
	"testing"
	"wopal-codex-gateway/internal/config"
)

func TestCatalogUsesProviderToolModeAndPreservesOfficialMode(t *testing.T) {
	for _, mode := range []string{"direct", "code_mode", "code_mode_only"} {
		native := []map[string]any{{"slug": "official", "tool_mode": "code_mode_only", "visibility": "hide"}}
		models := []config.ExternalModelConfig{{Name: "a"}, {Name: "b"}}
		got := buildCatalog(native, models, mode)
		for _, entry := range got[:2] {
			if entry["tool_mode"] != mode {
				t.Fatalf("%s: %v", mode, entry)
			}
		}
		if got[2]["tool_mode"] != "code_mode_only" || got[2]["visibility"] != "hide" {
			t.Fatal("official metadata changed")
		}
	}
}
