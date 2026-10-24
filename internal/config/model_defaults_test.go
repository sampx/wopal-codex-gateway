package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestExternalModelCapabilityDefaults(t *testing.T) {
	m := ExternalModelConfig{Name: "any-model"}
	if DefaultContextWindow != 1000000 || m.EffectiveContextWindow() != 1000000 || !m.SupportsVision() || !m.SupportsReasoning() {
		t.Fatalf("defaults = %d, %v, %v", m.EffectiveContextWindow(), m.SupportsVision(), m.SupportsReasoning())
	}
	var explicit ExternalModelConfig
	if err := json.Unmarshal([]byte(`{"name":"a","contextWindow":131072,"vision":false,"reasoning":false}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.EffectiveContextWindow() != 131072 || explicit.SupportsVision() || explicit.SupportsReasoning() {
		t.Fatalf("lost overrides: %+v", explicit)
	}
	encoded, err := json.Marshal(explicit)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["vision"] != false || fields["reasoning"] != false {
		t.Fatalf("lost explicit false: %s", encoded)
	}
}

func TestDefaultConfiguredModelsAreNameOnly(t *testing.T) {
	root := useTempGatewayRoot(t)
	cfg, err := LoadGatewayConfig(root + "/absent.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WopalAI.Models) != 1 || cfg.WopalAI.Models[0].Name == "" {
		t.Fatalf("default models = %+v", cfg.WopalAI.Models)
	}
	for _, m := range cfg.WopalAI.Models {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fields, map[string]any{"name": m.Name}) {
			t.Fatalf("default writes overrides: %s", raw)
		}
	}
	if err := SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadGatewayConfig(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.WopalAI.Models, cfg.WopalAI.Models) {
		t.Fatal("round trip fabricated overrides")
	}
}
