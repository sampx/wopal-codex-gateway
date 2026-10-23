package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/config"
)

func setupConfigAuthTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(config.SetProjectRootForTest(root))
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	return root
}

func captureConfigAuthTestStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() { os.Stdout = original }()

	callErr := fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(output), callErr
}

func TestConfigCommandShowPrintsLoadedJSON(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatalf("config.LoadGatewayConfig() error = %v", err)
	}
	cfg.WopalAI.BaseURL = "https://api.example.com/v1"
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatalf("config.SaveGatewayConfig() error = %v", err)
	}

	output, err := captureConfigAuthTestStdout(t, func() error { return runConfigCommand("show", nil) })
	if err != nil {
		t.Fatalf("runConfigCommand(show) error = %v", err)
	}
	var printed config.GatewayConfig
	if err := json.Unmarshal([]byte(output), &printed); err != nil {
		t.Fatalf("config show output is not JSON: %v\n%s", err, output)
	}
	if printed.WopalAI.BaseURL != cfg.WopalAI.BaseURL {
		t.Fatalf("printed base URL = %q, want %q", printed.WopalAI.BaseURL, cfg.WopalAI.BaseURL)
	}
}

func TestConfigCommandWopalSavesModelsAndClearsExclusions(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatalf("config.LoadGatewayConfig() error = %v", err)
	}
	cfg.WopalAI.ExcludedModels = []string{"a", "keep"}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatalf("config.SaveGatewayConfig() error = %v", err)
	}

	output, err := captureConfigAuthTestStdout(t, func() error {
		return runConfigCommand("wopal", []string{
			"--base-url", "https://api.example.com///",
			"--model", "a",
			"--model", "b",
			"--context-window", "131072",
			"--vision",
		})
	})
	if err != nil {
		t.Fatalf("runConfigCommand(wopal) error = %v", err)
	}
	wantOutput := "Wopal AI configured: https://api.example.com; models=wopal-ai/a, wopal-ai/b\n"
	if output != wantOutput {
		t.Fatalf("output = %q, want %q", output, wantOutput)
	}

	saved, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatalf("config.LoadGatewayConfig() error = %v", err)
	}
	if saved.WopalAI.BaseURL != "https://api.example.com" {
		t.Fatalf("base URL = %q", saved.WopalAI.BaseURL)
	}
	if len(saved.WopalAI.Models) != 2 {
		t.Fatalf("models = %#v", saved.WopalAI.Models)
	}
	for index, model := range saved.WopalAI.Models {
		if model.Name != []string{"a", "b"}[index] {
			t.Errorf("model[%d].Name = %q", index, model.Name)
		}
		if model.ContextWindow == nil || *model.ContextWindow != 131072 {
			t.Errorf("model[%d].ContextWindow = %v", index, model.ContextWindow)
		}
		if model.Vision == nil || !*model.Vision {
			t.Errorf("model[%d].Vision = %v", index, model.Vision)
		}
		if model.Reasoning == nil || !*model.Reasoning {
			t.Errorf("model[%d].Reasoning = %v", index, model.Reasoning)
		}
	}
	if len(saved.WopalAI.ExcludedModels) != 1 || saved.WopalAI.ExcludedModels[0] != "keep" {
		t.Fatalf("excluded models = %#v", saved.WopalAI.ExcludedModels)
	}
}

func TestConfigCommandWopalRejectsUnsupportedBaseURLScheme(t *testing.T) {
	setupConfigAuthTestRoot(t)
	for _, baseURL := range []string{"ftp://example.com", "file:///tmp/models"} {
		t.Run(baseURL, func(t *testing.T) {
			err := runConfigCommand("wopal", []string{"--base-url", baseURL})
			if err == nil || err.Error() != "Wopal AI base URL must use http or https" {
				t.Fatalf("runConfigCommand(wopal) error = %v", err)
			}
		})
	}
}

func TestConfigCommandUnknownSubcommand(t *testing.T) {
	setupConfigAuthTestRoot(t)
	err := runConfigCommand("ollama", []string{"--base-url", "http://127.0.0.1:11434"})
	if err == nil || err.Error() != "unknown config subcommand: ollama" {
		t.Fatalf("runConfigCommand(ollama) error = %v", err)
	}
}

func TestConfigModelNamePrefixNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"wopal-ai/model-a": "model-a",
		"model-b":          "model-b",
		"":                 "",
	} {
		if got := stripConfigModelPrefix(input); got != want {
			t.Errorf("stripConfigModelPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConfigCommandWopalUsesDefaultMetadataAndReasoningFlag(t *testing.T) {
	setupConfigAuthTestRoot(t)
	output, err := captureConfigAuthTestStdout(t, func() error {
		return runConfigCommand("wopal", []string{"--model", "wopal-ai/a", "--no-reasoning"})
	})
	if err != nil {
		t.Fatalf("runConfigCommand(wopal) error = %v", err)
	}
	if !strings.Contains(output, "(base URL not set)") {
		t.Fatalf("output = %q", output)
	}
	saved, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatalf("config.LoadGatewayConfig() error = %v", err)
	}
	model := saved.WopalAI.Models[0]
	if model.ContextWindow == nil || *model.ContextWindow != config.DefaultContextWindow {
		t.Fatalf("ContextWindow = %v", model.ContextWindow)
	}
	if model.Vision == nil || *model.Vision {
		t.Fatalf("Vision = %v", model.Vision)
	}
	if model.Reasoning == nil || *model.Reasoning {
		t.Fatalf("Reasoning = %v", model.Reasoning)
	}
}
