package main

import (
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

func TestRuntimeUsesSameCapabilityResolutionAsCatalog(t *testing.T) {
	t.Cleanup(config.SetProjectRootForTest(t.TempDir()))
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "m", Reasoning: modelsBoolPtr(true)}}
	if err := catalog.SaveModelMetadata(&catalog.ModelMetadataSnapshot{BaseURL: cfg.WopalAI.BaseURL, FetchedAt: time.Now(), Models: []config.ExternalModelConfig{{Name: "m", ContextWindow: modelsIntPtr(65536), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)}}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := resolved.WopalAI.Models[0]
	if m.EffectiveContextWindow() != 65536 || m.SupportsVision() || !m.SupportsReasoning() {
		t.Fatalf("runtime differs from catalog: %+v", m)
	}
	if cfg.WopalAI.Models[0].ContextWindow != nil || cfg.WopalAI.Models[0].Vision != nil {
		t.Fatal("resolution modified persisted overrides")
	}
}

func TestRuntimeResolvesExternalProviderProtocolAndCapabilities(t *testing.T) {
	t.Cleanup(config.SetProjectRootForTest(t.TempDir()))
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	cfg.Providers["external"] = config.ProviderConfig{
		BaseURL: "https://external.test/v1",
		Models: []config.ExternalModelConfig{
			{Name: "metadata-model", Vision: modelsBoolPtr(true)},
			{Name: "provider-default"},
			{Name: "unresolved"},
		},
	}
	if err := catalog.SaveProviderModelMetadata(&catalog.ProviderModelMetadataSnapshot{
		Provider:  "external",
		FetchedAt: time.Now().UTC(),
		Models: []catalog.ProviderModelMetadata{{
			Name: "metadata-model", Protocol: config.ProtocolOpenAIResponses,
			ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	providerCfg := resolved.Providers["external"]
	metadata := providerCfg.Models[0]
	if metadata.Protocol != config.ProtocolOpenAIResponses || metadata.EffectiveContextWindow() != 131072 || !metadata.SupportsVision() || metadata.SupportsReasoning() {
		t.Fatalf("metadata model = %+v", metadata)
	}
	if providerCfg.Models[2].Protocol != "" {
		t.Fatalf("unresolved model unexpectedly gained protocol: %+v", providerCfg.Models[2])
	}
	if cfg.Providers["external"].Models[0].Protocol != "" || cfg.Providers["external"].Models[0].ContextWindow != nil {
		t.Fatal("runtime resolution modified persisted external config")
	}
}

func TestRuntimeProviderKeysLoadsOnlyProtocolReadyProviders(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.Providers["ready-b"] = config.ProviderConfig{Models: []config.ExternalModelConfig{{Name: "b", Protocol: config.ProtocolGemini}}}
	cfg.Providers["unresolved"] = config.ProviderConfig{Models: []config.ExternalModelConfig{{Name: "u"}}}
	cfg.Providers["ready-a"] = config.ProviderConfig{Models: []config.ExternalModelConfig{{Name: "a", Protocol: config.ProtocolOpenAIChat}}}
	if got, want := runtimeProviderIDs(cfg), []string{"ready-a", "ready-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime provider ids = %#v, want %#v", got, want)
	}

	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })
	var accounts []string
	keychain.KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		for index, arg := range args {
			if arg == "-a" && index+1 < len(args) {
				accounts = append(accounts, args[index+1])
				_, _ = io.WriteString(stdout, "key-for-"+args[index+1])
				return nil
			}
		}
		return errors.New("missing account")
	}
	keys, err := loadRuntimeProviderKeys(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := accounts, []string{"ready-a", "ready-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keychain accounts = %#v, want %#v", got, want)
	}
	if keys["ready-a"] != "key-for-ready-a" || keys["ready-b"] != "key-for-ready-b" {
		t.Fatalf("provider keys = %#v", keys)
	}
}
