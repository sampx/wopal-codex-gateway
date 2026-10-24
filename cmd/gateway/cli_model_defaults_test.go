package main

import (
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestModelsAddPersistsOnlyExplicitCapabilities(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{}
	f := newModelsCommandFixture(cfg)
	if err := f.runner().run("add", []string{"plain"}); err != nil {
		t.Fatal(err)
	}
	m := cfg.WopalAI.Models[0]
	if m.ContextWindow != nil || m.Vision != nil || m.Reasoning != nil {
		t.Fatalf("defaults became user overrides: %#v", m)
	}
}

func TestConfigWopalPersistsOnlyExplicitCapabilities(t *testing.T) {
	setupConfigAuthTestRoot(t)
	if _, err := captureConfigAuthTestStdout(t, func() error {
		return runConfigWopal([]string{"--model", "plain"})
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.WopalAI.Models[0]
	if m.ContextWindow != nil || m.Vision != nil || m.Reasoning != nil {
		t.Fatalf("defaults became user overrides: %#v", m)
	}
}
