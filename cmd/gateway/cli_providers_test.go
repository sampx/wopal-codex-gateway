package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestProvidersSetCreatesAndUpdatesWithoutClearingOmittedFields(t *testing.T) {
	setupConfigAuthTestRoot(t)

	output, err := captureConfigAuthTestStdout(t, func() error {
		return runProvidersCommand("set", []string{
			"opencode-go",
			"--base-url", "https://opencode.example/openai/v1/",
			"--base-url-for", "gemini=https://opencode.example/google/",
			"--models-url", "https://opencode.example/models",
			"--models-auth", "none",
			"--header", "x-opencode-session=$CPA-SESSION-ID",
			"--protocol", "openai-responses",
			"--tool-mode", "direct",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "created\n" {
		t.Fatalf("create output = %q", output)
	}

	output, err = captureConfigAuthTestStdout(t, func() error {
		return runProvidersCommand("set", []string{"opencode-go", "--models-auth", "provider-key"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "updated\n" {
		t.Fatalf("update output = %q", output)
	}

	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["opencode-go"]
	if p.BaseURL != "https://opencode.example/openai/v1" || p.BaseURLs["gemini"] != "https://opencode.example/google" {
		t.Fatalf("update cleared URLs: %+v", p)
	}
	if p.ModelsURL != "https://opencode.example/models" || p.ModelsAuth != "provider-key" || p.Protocol != config.ProtocolOpenAIResponses || p.ToolMode != "direct" {
		t.Fatalf("provider = %+v", p)
	}
	if p.Headers["x-opencode-session"] != "$CPA-SESSION-ID" {
		t.Fatalf("headers = %#v", p.Headers)
	}
}

func TestProvidersSetRequiresBaseURLForCreateAndRejectsInvalidProtocol(t *testing.T) {
	root := setupConfigAuthTestRoot(t)
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing base", args: []string{"new-provider", "--models-auth", "none"}, want: "--base-url"},
		{name: "reserved id", args: []string{"official", "--base-url", "https://example.test"}, want: "reserved"},
		{name: "uppercase id", args: []string{"OpenCode", "--base-url", "https://example.test"}, want: "provider id"},
		{name: "invalid protocol", args: []string{"opencode", "--base-url", "https://example.test", "--protocol", "bogus"}, want: "protocol"},
		{name: "invalid tool mode", args: []string{"opencode", "--base-url", "https://example.test", "--tool-mode", "bogus"}, want: "tool"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := runProvidersCommand("set", testCase.args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(testCase.want)) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "runtime", "config.json")); err == nil {
		t.Fatal("invalid creates wrote config.json")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestProvidersRemoveDeletesProvider(t *testing.T) {
	setupConfigAuthTestRoot(t)
	if err := runProvidersCommand("set", []string{"p", "--base-url", "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	output, err := captureConfigAuthTestStdout(t, func() error { return runProvidersCommand("remove", []string{"p"}) })
	if err != nil {
		t.Fatal(err)
	}
	if output != "removed\n" {
		t.Fatalf("remove output = %q", output)
	}
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["p"]; ok {
		t.Fatalf("provider still present: %#v", cfg.Providers)
	}
}

func TestProvidersRemoveRejectsProviderReferencedByRole(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["p"] = config.ProviderConfig{BaseURL: "https://example.test", Models: []config.ExternalModelConfig{{Name: "m", Protocol: config.ProtocolOpenAIChat}}}
	cfg.ModelRoles.Compaction = &config.ModelRoleRef{Provider: "p", Model: "m"}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	err := runProvidersCommand("remove", []string{"p"})
	if err == nil || !strings.Contains(err.Error(), "compaction") {
		t.Fatalf("remove error = %v, want role reference", err)
	}
}

func TestProvidersSetRejectsExistingDuplicateClientNamesBeforeWrite(t *testing.T) {
	root := setupConfigAuthTestRoot(t)
	path := filepath.Join(root, "runtime", "config.json")
	body := `{"version":1,"wopalAi":{"baseUrl":"https://wopal.test/v1"},"providers":{"one":{"baseUrl":"https://one.test","models":[{"name":"a","alias":"shared"}]},"two":{"baseUrl":"https://two.test","models":[{"name":"b","alias":"shared"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runProvidersCommand("set", []string{"three", "--base-url", "https://three.test"})
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("error = %v, want duplicate shared name", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Fatalf("invalid registry changed config:\n%s", after)
	}
}
