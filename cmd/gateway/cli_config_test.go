package main

import (
	"encoding/json"
	"fmt"
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
	originalSignal := signalGatewayReload
	signalGatewayReload = func() error { return nil }
	t.Cleanup(func() { signalGatewayReload = originalSignal })
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
		if model.Reasoning != nil || !model.SupportsReasoning() {
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
	if model.ContextWindow != nil || model.EffectiveContextWindow() != config.DefaultContextWindow {
		t.Fatalf("ContextWindow = %v", model.ContextWindow)
	}
	if model.Vision != nil || !model.SupportsVision() {
		t.Fatalf("Vision = %v", model.Vision)
	}
	if model.Reasoning == nil || *model.Reasoning {
		t.Fatalf("Reasoning = %v", model.Reasoning)
	}
}

func TestConfigToolModeFlagPersistsAndRejectsInvalidValue(t *testing.T) {
	root := setupConfigAuthTestRoot(t)
	for _, mode := range []string{"code_mode", "direct", "code_mode_only"} {
		_, err := captureConfigAuthTestStdout(t, func() error { return runConfigWopal([]string{"--tool-mode", mode}) })
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadGatewayConfig(config.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WopalAI.ToolMode != mode {
			t.Fatalf("saved mode = %q", cfg.WopalAI.ToolMode)
		}
	}
	path := filepath.Join(root, "runtime", "config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--tool-mode", "bogus"}, {"--tool-mode"}, {"--tool-mode", "--vision"}} {
		if err := runConfigWopal(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("invalid mode changed config: %v", args)
		}
	}
}

func TestConfigRoleSetListAndReset(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "upstream", Alias: "friendly", Protocol: config.ProtocolOpenAIResponses}},
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if err := runConfigCommand("role", []string{"set", "official-fallback", "--provider", "vendor", "--model", "upstream"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureConfigAuthTestStdout(t, func() error { return runConfigCommand("role", nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"officialFallback provider=vendor upstream=upstream client=friendly source=explicit status=ready",
		"compaction provider=wopal-ai upstream=deepseek-v4.1-flash source=default status=unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("role output missing %q:\n%s", want, out)
		}
	}

	if err := runConfigCommand("role", []string{"reset", "official-fallback"}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if saved.ModelRoles.OfficialFallback != nil {
		t.Fatalf("role reset left explicit config: %+v", saved.ModelRoles.OfficialFallback)
	}
}

func TestConfigRoleCompactionUpdatesInstalledCodexMemories(t *testing.T) {
	setupConfigAuthTestRoot(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Dir(config.CodexConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.WopalAI.Models = append(cfg.WopalAI.Models, config.ExternalModelConfig{Name: "deepseek-v4.1-flash"})
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "upstream", Alias: "friendly", Protocol: config.ProtocolOpenAIResponses}},
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	gatewayBaseURL := fmt.Sprintf("http://%s:%d/v1", cfg.Listen.Host, cfg.Listen.Port)
	codexConfig := "model_catalog_json = " + fmt.Sprintf("%q", config.CodexGatewayCatalogPath()) + "\n" +
		"openai_base_url = " + fmt.Sprintf("%q", gatewayBaseURL) + "\n\n" +
		"[memories]\n" +
		"extract_model = \"wopal-ai/old\"\n" +
		"consolidation_model = \"wopal-ai/old\"\n\n" +
		"[other]\nkeep = true\n"
	if err := os.WriteFile(config.CodexConfigPath(), []byte(codexConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	readCodexConfig := func() string {
		t.Helper()
		data, err := os.ReadFile(config.CodexConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if err := runConfigCommand("role", []string{"set", "compaction", "--provider", "vendor", "--model", "upstream"}); err != nil {
		t.Fatal(err)
	}
	if got := readCodexConfig(); strings.Count(got, "\"friendly\"") != 2 || !strings.Contains(got, "[other]\nkeep = true") {
		t.Fatalf("compaction role set did not update only managed memories keys:\n%s", got)
	}

	if err := runConfigCommand("role", []string{"reset", "compaction"}); err != nil {
		t.Fatal(err)
	}
	if got := readCodexConfig(); strings.Count(got, "\"wopal-ai/deepseek-v4.1-flash\"") != 2 {
		t.Fatalf("compaction role reset did not restore the default memory model:\n%s", got)
	}
}

func TestConfigRoleCompactionResetWritesUnavailableDefaultTarget(t *testing.T) {
	setupConfigAuthTestRoot(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	if err := os.MkdirAll(filepath.Dir(config.CodexConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "upstream", Alias: "friendly", Protocol: config.ProtocolOpenAIResponses}},
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	gatewayBaseURL := fmt.Sprintf("http://%s:%d/v1", cfg.Listen.Host, cfg.Listen.Port)
	codexConfig := "model_catalog_json = " + fmt.Sprintf("%q", config.CodexGatewayCatalogPath()) + "\n" +
		"openai_base_url = " + fmt.Sprintf("%q", gatewayBaseURL) + "\n\n" +
		"[memories]\n" +
		"extract_model = \"wopal-ai/old\"\n" +
		"consolidation_model = \"wopal-ai/old\"\n"
	if err := os.WriteFile(config.CodexConfigPath(), []byte(codexConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runConfigCommand("role", []string{"set", "compaction", "--provider", "vendor", "--model", "upstream"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCommand("role", []string{"reset", "compaction"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.CodexConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "\"friendly\"") {
		t.Fatalf("unavailable default reset kept the stale explicit role:\n%s", got)
	}
	if strings.Count(got, "\"wopal-ai/deepseek-v4.1-flash\"") != 2 {
		t.Fatalf("reset did not write the fail-closed default target:\n%s", got)
	}
}

func TestConfigRoleSetRejectsMissingOrUnreadyTargetBeforeWrite(t *testing.T) {
	root := setupConfigAuthTestRoot(t)
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "needs-protocol"}},
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "runtime", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"set", "official-fallback", "--provider", "missing", "--model", "x"},
		{"set", "compaction", "--provider", "vendor", "--model", "needs-protocol"},
		{"set", "compaction", "--provider", "official", "--model", "gpt"},
	} {
		if err := runConfigCommand("role", args); err == nil {
			t.Fatalf("role set %v unexpectedly succeeded", args)
		}
	}
	after, err := os.ReadFile(filepath.Join(root, "runtime", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("invalid role set changed config")
	}
}
