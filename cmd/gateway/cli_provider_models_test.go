package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

type providerModelsFixture struct {
	cfg                  *config.GatewayConfig
	available            []string
	metadata             *catalog.ProviderModelMetadataSnapshot
	modelsDev            *catalog.ModelsDevProvider
	modelsDevNotModified bool
	modelsDevErr         error
	fetchDiscoveryCalls  int
	fetchModelsDevCalls  int
	getKeyCalls          int
	savedMetadata        []*catalog.ProviderModelMetadataSnapshot
	saveConfigCalls      int
	refreshCalls         int
	out                  bytes.Buffer
}

func newProviderModelsFixture(providerID string, provider config.ProviderConfig) *providerModelsFixture {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.example/v1"
	cfg.Providers[providerID] = provider
	return &providerModelsFixture{cfg: cfg}
}

func (f *providerModelsFixture) runner() wopalModelsRunner {
	return wopalModelsRunner{
		loadConfig: func() (*config.GatewayConfig, error) { return f.cfg, nil },
		saveConfig: func(*config.GatewayConfig) error { f.saveConfigCalls++; return nil },
		fetchProviderModels: func(modelsURL, apiKey string, useProviderKey bool) ([]string, error) {
			f.fetchDiscoveryCalls++
			return append([]string(nil), f.available...), nil
		},
		fetchModelsDevProvider: func(providerID, etag string) (*catalog.ModelsDevProvider, bool, error) {
			f.fetchModelsDevCalls++
			return f.modelsDev, f.modelsDevNotModified, f.modelsDevErr
		},
		loadProviderMetadata: func(providerID string) (*catalog.ProviderModelMetadataSnapshot, error) {
			if f.metadata == nil {
				return nil, nil
			}
			clone := *f.metadata
			clone.Models = append([]catalog.ProviderModelMetadata(nil), f.metadata.Models...)
			return &clone, nil
		},
		saveProviderMetadata: func(snapshot *catalog.ProviderModelMetadataSnapshot) error {
			copy := *snapshot
			copy.Models = append([]catalog.ProviderModelMetadata(nil), snapshot.Models...)
			f.savedMetadata = append(f.savedMetadata, &copy)
			f.metadata = &copy
			return nil
		},
		getProviderAPIKey:     func(providerID string) (string, error) { f.getKeyCalls++; return "provider-key", nil },
		validateOfficialNames: func(*config.GatewayConfig) error { return nil },
		refreshCatalog: func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
			f.refreshCalls++
			return &catalog.CatalogRefreshResult{}, nil
		},
		out: &f.out,
	}
}

func TestProviderModelsListShowsAvailableProtocolSourceWithoutRegistering(t *testing.T) {
	providerID := "opencode-go"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{
		BaseURL:  "https://api.example.test/v1",
		Models:   []config.ExternalModelConfig{{Name: "known"}},
		Protocol: config.ProtocolOpenAIChat,
	})
	fixture.available = []string{"known", "remote-only"}
	fixture.metadata = &catalog.ProviderModelMetadataSnapshot{
		Provider: providerID, FetchedAt: time.Now().UTC(),
		Models: []catalog.ProviderModelMetadata{{Name: "known", Protocol: config.ProtocolOpenAIResponses, ProtocolSource: "metadata"}},
	}

	if err := fixture.runner().run("list", []string{"--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	got := fixture.out.String()
	for _, want := range []string{
		"known  protocol=openai-responses [metadata]  configured=true",
		"remote-only  protocol=openai-chat [provider-default]  configured=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q missing %q", got, want)
		}
	}
	if fixture.saveConfigCalls != 0 {
		t.Fatalf("models list changed config: %d saves", fixture.saveConfigCalls)
	}
	if len(fixture.cfg.Providers[providerID].Models) != 1 {
		t.Fatalf("configured models changed: %#v", fixture.cfg.Providers[providerID].Models)
	}
}

func TestProviderModelsListMarksNeedsProtocol(t *testing.T) {
	providerID := "external"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{BaseURL: "https://api.example.test/v1"})
	fixture.available = []string{"m"}
	if err := fixture.runner().run("list", []string{"--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	if got := fixture.out.String(); !strings.Contains(got, "m  protocol=needs_protocol [needs_protocol]") {
		t.Fatalf("output=%q", got)
	}
}

func TestProviderModelsSyncWritesMetadataOnlyAndReportsProtocolChange(t *testing.T) {
	providerID := "opencode-go"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{
		BaseURL: "https://api.example.test/v1",
		Models:  []config.ExternalModelConfig{{Name: "configured", Protocol: config.ProtocolOpenAIChat}},
	})
	fixture.available = []string{"configured", "available-only"}
	fixture.metadata = &catalog.ProviderModelMetadataSnapshot{
		Provider: providerID, ETag: "old", FetchedAt: time.Now().Add(-time.Hour).UTC(),
		Models: []catalog.ProviderModelMetadata{{Name: "configured", Protocol: config.ProtocolOpenAIChat, ProtocolSource: "metadata"}},
	}
	fixture.modelsDev = &catalog.ModelsDevProvider{
		Provider: providerID, ETag: "new", FetchedAt: time.Now().UTC(), NPM: "@ai-sdk/openai-compatible",
		Models: map[string]catalog.ModelsDevModel{
			"configured": {Name: "configured", Shape: "responses"},
		},
	}
	before := append([]config.ExternalModelConfig(nil), fixture.cfg.Providers[providerID].Models...)

	if err := fixture.runner().run("sync", []string{"--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	if fixture.saveConfigCalls != 0 {
		t.Fatalf("external sync wrote config %d times", fixture.saveConfigCalls)
	}
	if !reflect.DeepEqual(fixture.cfg.Providers[providerID].Models, before) {
		t.Fatalf("explicit config changed: %#v", fixture.cfg.Providers[providerID].Models)
	}
	if len(fixture.savedMetadata) != 1 {
		t.Fatalf("metadata saves=%d", len(fixture.savedMetadata))
	}
	if got := fixture.savedMetadata[0].Models; len(got) != 2 || got[0].Name != "configured" || got[1].Name != "available-only" {
		t.Fatalf("joined metadata=%#v", got)
	}
	if !strings.Contains(fixture.out.String(), "configured protocol changed: openai-chat -> openai-responses") {
		t.Fatalf("output=%q", fixture.out.String())
	}
	if !strings.Contains(fixture.out.String(), "Warning: configured explicit protocol openai-chat differs from metadata openai-responses") {
		t.Fatalf("missing explicit conflict warning: %q", fixture.out.String())
	}
}

func TestProviderModelsSyncDiscoveryDisabledUsesConfiguredOnly(t *testing.T) {
	providerID := "anthropic"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{
		BaseURL:  "https://api.anthropic.com",
		Protocol: config.ProtocolAnthropicMessages,
		Models:   []config.ExternalModelConfig{{Name: "claude-a"}},
	})
	fixture.modelsDev = &catalog.ModelsDevProvider{
		Provider: providerID, ETag: "v1", FetchedAt: time.Now().UTC(), NPM: "@ai-sdk/anthropic",
		Models: map[string]catalog.ModelsDevModel{"claude-a": {Name: "claude-a"}, "not-configured": {Name: "not-configured"}},
	}
	if err := fixture.runner().run("sync", []string{"--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	if fixture.fetchDiscoveryCalls != 0 || fixture.getKeyCalls != 0 {
		t.Fatalf("disabled discovery called network/key: discovery=%d key=%d", fixture.fetchDiscoveryCalls, fixture.getKeyCalls)
	}
	if len(fixture.savedMetadata) != 1 || len(fixture.savedMetadata[0].Models) != 1 || fixture.savedMetadata[0].Models[0].Name != "claude-a" {
		t.Fatalf("metadata=%#v", fixture.savedMetadata)
	}
}

func TestProviderModelsSyncKeepsPreviousMetadataOnModelsDevFailure(t *testing.T) {
	providerID := "p"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{BaseURL: "https://api.example.test/v1"})
	fixture.available = []string{"m"}
	fixture.metadata = &catalog.ProviderModelMetadataSnapshot{Provider: providerID, ETag: "old", FetchedAt: time.Now().UTC(), Models: []catalog.ProviderModelMetadata{{Name: "m", Protocol: config.ProtocolGemini}}}
	fixture.modelsDevErr = errors.New("offline")
	if err := fixture.runner().run("sync", []string{"--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.savedMetadata) != 0 {
		t.Fatalf("failure overwrote previous metadata")
	}
	if !strings.Contains(fixture.out.String(), "Warning: models.dev refresh failed; keeping previous metadata") {
		t.Fatalf("output=%q", fixture.out.String())
	}
}

func TestProviderModelsAddSetClearAliasAndRemove(t *testing.T) {
	providerID := "p"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{BaseURL: "https://api.example.test/v1"})
	runner := fixture.runner()
	if err := runner.run("add", []string{"m", "--provider", providerID, "--alias", "short", "--protocol", "gemini"}); err != nil {
		t.Fatal(err)
	}
	model := fixture.cfg.Providers[providerID].Models[0]
	if model.Name != "m" || model.Alias != "short" || model.Protocol != config.ProtocolGemini {
		t.Fatalf("added=%+v", model)
	}
	if err := runner.run("set", []string{"m", "--provider", providerID, "--alias", "", "--protocol", "openai-responses"}); err != nil {
		t.Fatal(err)
	}
	model = fixture.cfg.Providers[providerID].Models[0]
	if model.Alias != "" || model.Protocol != config.ProtocolOpenAIResponses {
		t.Fatalf("updated=%+v", model)
	}
	if err := runner.run("remove", []string{"m", "--provider", providerID}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.cfg.Providers[providerID].Models) != 0 {
		t.Fatalf("models=%#v", fixture.cfg.Providers[providerID].Models)
	}
}

func TestProviderModelsAliasCollisionFailsBeforeSave(t *testing.T) {
	providerID := "p"
	fixture := newProviderModelsFixture(providerID, config.ProviderConfig{BaseURL: "https://api.example.test/v1"})
	runner := fixture.runner()
	runner.validateOfficialNames = func(cfg *config.GatewayConfig) error {
		for _, model := range cfg.Providers[providerID].Models {
			if model.Alias == "official-a" {
				return errors.New(`provider "p" model "m" client-visible model name "official-a" collides with official model`)
			}
		}
		return nil
	}
	err := runner.run("add", []string{"m", "--provider", providerID, "--alias", "official-a", "--protocol", "openai-chat"})
	if err == nil || !strings.Contains(err.Error(), "collides with official model") {
		t.Fatalf("error=%v", err)
	}
	if fixture.saveConfigCalls != 0 {
		t.Fatalf("collision wrote config %d times", fixture.saveConfigCalls)
	}
}
