package provider

import (
	"testing"

	"wopal-codex-gateway/internal/config"
)

func TestResolveRoleUsesProviderAndUpstreamModelAcrossAliasChanges(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "upstream", Alias: "short", Protocol: config.ProtocolOpenAIResponses}},
	}
	cfg.ModelRoles.OfficialFallback = &config.ModelRoleRef{Provider: "vendor", Model: "upstream"}

	registry, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolveRole(cfg, registry, RoleOfficialFallback)
	if resolved.Status != RoleReady || resolved.Source != RoleExplicit || resolved.ClientModel != "short" {
		t.Fatalf("resolved role = %+v", resolved)
	}

	providerCfg := cfg.Providers["vendor"]
	providerCfg.Models[0].Alias = "renamed"
	cfg.Providers["vendor"] = providerCfg
	registry, err = Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved = ResolveRole(cfg, registry, RoleOfficialFallback)
	if resolved.ProviderID != "vendor" || resolved.UpstreamModel != "upstream" || resolved.ClientModel != "renamed" {
		t.Fatalf("role moved after alias rename: %+v", resolved)
	}
}

func TestResolveRoleDistinguishesUnavailableDefaultFromInvalidExplicit(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.WopalAI.Models = nil
	registry, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defaultRole := ResolveRole(cfg, registry, RoleCompaction)
	if defaultRole.Source != RoleDefault || defaultRole.Status != RoleUnavailable {
		t.Fatalf("default role = %+v", defaultRole)
	}

	cfg.ModelRoles.Compaction = &config.ModelRoleRef{Provider: "missing", Model: "nope"}
	explicitRole := ResolveRole(cfg, registry, RoleCompaction)
	if explicitRole.Source != RoleExplicit || explicitRole.Status != RoleInvalid {
		t.Fatalf("explicit role = %+v", explicitRole)
	}
}

func TestResolveRoleRejectsOfficialAndNeedsProtocolTargets(t *testing.T) {
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://wopal.test/v1"
	cfg.Providers["vendor"] = config.ProviderConfig{
		BaseURL: "https://vendor.test",
		Models:  []config.ExternalModelConfig{{Name: "unknown-protocol"}},
	}
	registry, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ModelRoles.OfficialFallback = &config.ModelRoleRef{Provider: OfficialID, Model: "gpt"}
	if got := ResolveRole(cfg, registry, RoleOfficialFallback); got.Status != RoleInvalid {
		t.Fatalf("official target = %+v", got)
	}
	cfg.ModelRoles.OfficialFallback = &config.ModelRoleRef{Provider: "vendor", Model: "unknown-protocol"}
	if got := ResolveRole(cfg, registry, RoleOfficialFallback); got.Status != RoleInvalid {
		t.Fatalf("needs-protocol target = %+v", got)
	}
}
