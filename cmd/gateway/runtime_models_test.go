package main

import (
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
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
