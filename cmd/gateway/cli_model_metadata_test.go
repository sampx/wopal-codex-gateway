package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

func TestModelsInspectShowsSourcesAndPreservesCodexConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	content := "model_context_window = 1000000\nmodel_auto_compact_token_limit = 850000\nsecret = 'DO-NOT-PRINT'\n[profiles.test]\nmodel_context_window = 42\n"
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "m", Vision: modelsBoolPtr(false)}, {Name: "other"}}
	f := newModelsCommandFixture(cfg)
	r := f.runner()
	r.loadMetadata = func(string) (*catalog.ModelMetadataSnapshot, error) {
		return &catalog.ModelMetadataSnapshot{Models: []config.ExternalModelConfig{{Name: "m", ContextWindow: modelsIntPtr(131072)}}}, nil
	}
	if err := r.run("inspect", []string{"wopal-ai/m"}); err != nil {
		t.Fatal(err)
	}
	out := f.output.String()
	for _, want := range []string{"context=131072 [provider]", "vision=false [user]", "reasoning=true [default]", "global model_context_window", "global model_auto_compact_token_limit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "DO-NOT-PRINT") || strings.Contains(out, "wopal-ai/other") || len(f.saved) != 0 || f.fetchCalls != 0 {
		t.Fatalf("inspect leaked or mutated: %s", out)
	}
	after, _ := os.ReadFile(path)
	if string(after) != content {
		t.Fatal("Codex config changed")
	}
}

func TestModelsSetClearsOverridesAndRejectsConflicts(t *testing.T) {
	for _, args := range [][]string{{"m", "--auto", "--vision"}, {"m", "--reset-context-window", "--context-window", "2000"}, {"m", "--reset-reasoning", "--no-reasoning"}, {"m", "--context-window"}} {
		f := newModelsCommandFixture(config.DefaultGatewayConfig())
		if err := f.runner().run("set", args); err == nil || len(f.saved) != 0 {
			t.Fatalf("accepted invalid patch %v: %v", args, err)
		}
	}
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "m", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)}}
	f := newModelsCommandFixture(cfg)
	if err := f.runner().run("set", []string{"m", "--auto"}); err != nil {
		t.Fatal(err)
	}
	m := cfg.WopalAI.Models[0]
	if m.ContextWindow != nil || m.Vision != nil || m.Reasoning != nil {
		t.Fatalf("overrides retained: %+v", m)
	}
}

func TestModelsSyncMetadataFailurePreservesConfiguration(t *testing.T) {
	for _, failFetch := range []bool{true, false} {
		cfg := config.DefaultGatewayConfig()
		cfg.WopalAI.BaseURL = "https://example.test/v1"
		f := newModelsCommandFixture(cfg)
		r := f.runner()
		r.fetchMetadata = func(string, string) (*catalog.ModelMetadataSnapshot, error) {
			if failFetch {
				return nil, errors.New("fetch failed")
			}
			return &catalog.ModelMetadataSnapshot{Models: []config.ExternalModelConfig{{Name: "new"}}}, nil
		}
		r.saveMetadata = func(*catalog.ModelMetadataSnapshot) error { return errors.New("save failed") }
		if err := r.run("sync", nil); err == nil || len(f.saved) != 0 || f.refreshCalls != 0 || cfg.WopalAI.Models[0].Name == "new" {
			t.Fatalf("failure changed config: %v", err)
		}
	}
}

func TestModelsSyncKeepsProviderMetadataOutOfUserOverrides(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	cfg.WopalAI.Models = []config.ExternalModelConfig{}
	f := newModelsCommandFixture(cfg)
	r := f.runner()
	saved := false
	snapshot := &catalog.ModelMetadataSnapshot{Models: []config.ExternalModelConfig{{Name: "m", Vision: modelsBoolPtr(false)}}}
	r.fetchMetadata = func(string, string) (*catalog.ModelMetadataSnapshot, error) { return snapshot, nil }
	r.saveMetadata = func(got *catalog.ModelMetadataSnapshot) error { saved = got == snapshot; return nil }
	if err := r.run("sync", nil); err != nil {
		t.Fatal(err)
	}
	if !saved || len(cfg.WopalAI.Models) != 1 || cfg.WopalAI.Models[0].Vision != nil {
		t.Fatal("provider data became a user override")
	}
}
