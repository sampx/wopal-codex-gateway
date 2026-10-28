package provider

import (
	"strings"
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestBuildRegistryIncludesBuiltinsAndExternalModels(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "wopal-model"}}
	cfg.Providers = map[string]config.ProviderConfig{
		"opencode-go": {
			BaseURL:  "https://opencode.example/openai/v1",
			Protocol: config.ProtocolOpenAIResponses,
			Models: []config.ExternalModelConfig{
				{Name: "gpt-x"},
				{Name: "claude-x", Alias: "fast-claude", Protocol: config.ProtocolAnthropicMessages},
			},
		},
	}

	registry, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{OfficialID, WopalAIID, "opencode-go"} {
		if _, ok := registry.Provider(id); !ok {
			t.Fatalf("provider %q missing", id)
		}
	}
	tests := map[string]ModelRef{
		"wopal-ai/wopal-model": {ProviderID: WopalAIID, UpstreamModel: "wopal-model", ClientModel: "wopal-ai/wopal-model", Protocol: config.ProtocolOpenAIChat},
		"opencode-go/gpt-x":    {ProviderID: "opencode-go", UpstreamModel: "gpt-x", ClientModel: "opencode-go/gpt-x", Protocol: config.ProtocolOpenAIResponses},
		"fast-claude":          {ProviderID: "opencode-go", UpstreamModel: "claude-x", ClientModel: "fast-claude", Protocol: config.ProtocolAnthropicMessages},
	}
	for client, want := range tests {
		got, ok := registry.Model(client)
		if !ok || got != want {
			t.Fatalf("Model(%q) = %+v, %v; want %+v", client, got, ok, want)
		}
	}
}

func TestBuildRegistryRejectsDuplicateClientVisibleNames(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "same"}}
	cfg.Providers = map[string]config.ProviderConfig{
		"one": {BaseURL: "https://one.test", Models: []config.ExternalModelConfig{{Name: "a", Alias: "shared"}}},
		"two": {BaseURL: "https://two.test", Models: []config.ExternalModelConfig{{Name: "b", Alias: "shared"}}},
	}
	_, err := Build(cfg)
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("Build() error = %v, want duplicate shared name", err)
	}

	cfg.Providers["two"] = config.ProviderConfig{BaseURL: "https://two.test", Models: []config.ExternalModelConfig{{Name: "b", Alias: "wopal-ai/same"}}}
	_, err = Build(cfg)
	if err == nil || !strings.Contains(err.Error(), "wopal-ai/same") {
		t.Fatalf("Build() error = %v, want collision with Wopal name", err)
	}
}

func TestClientVisibleNameUsesAliasOrProviderNamespace(t *testing.T) {
	if got := ClientVisibleName("p", config.ExternalModelConfig{Name: "m"}); got != "p/m" {
		t.Fatalf("default client name = %q", got)
	}
	if got := ClientVisibleName("p", config.ExternalModelConfig{Name: "m", Alias: "short"}); got != "short" {
		t.Fatalf("alias client name = %q", got)
	}
}
