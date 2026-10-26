package main

import (
	"fmt"
	"sort"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

func resolveRuntimeModels(cfg *config.GatewayConfig) (*config.GatewayConfig, error) {
	snapshot, err := catalog.LoadModelMetadata(cfg.WopalAI.BaseURL)
	if err != nil {
		return nil, err
	}
	resolved := *cfg
	resolved.WopalAI.Models = make([]config.ExternalModelConfig, 0, len(cfg.WopalAI.Models))
	for _, model := range cfg.WopalAI.Models {
		effective, _ := catalog.ResolveModel(model, snapshot)
		resolved.WopalAI.Models = append(resolved.WopalAI.Models, effective)
	}
	resolved.Providers = make(map[string]config.ProviderConfig, len(cfg.Providers))
	for providerID, providerCfg := range cfg.Providers {
		providerSnapshot, err := catalog.LoadProviderModelMetadata(providerID)
		if err != nil {
			return nil, err
		}
		effectiveProvider := providerCfg.Clone()
		effectiveProvider.Models = make([]config.ExternalModelConfig, 0, len(providerCfg.Models))
		for _, model := range providerCfg.Models {
			effective, _ := catalog.ResolveProviderModel(model, providerCfg, providerSnapshot)
			effectiveProvider.Models = append(effectiveProvider.Models, effective)
		}
		resolved.Providers[providerID] = effectiveProvider
	}
	return &resolved, nil
}

func runtimeProviderIDs(cfg *config.GatewayConfig) []string {
	if cfg == nil {
		return nil
	}
	ids := make([]string, 0, len(cfg.Providers))
	for providerID, providerCfg := range cfg.Providers {
		ready := false
		for _, model := range providerCfg.Models {
			if strings.TrimSpace(model.Protocol) != "" {
				ready = true
				break
			}
		}
		if ready {
			ids = append(ids, providerID)
		}
	}
	sort.Strings(ids)
	return ids
}

func loadRuntimeProviderKeys(cfg *config.GatewayConfig) (map[string]string, error) {
	ids := runtimeProviderIDs(cfg)
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make(map[string]string, len(ids))
	for _, providerID := range ids {
		key, err := keychain.GetProviderAPIKey(providerID)
		if err != nil {
			return nil, fmt.Errorf("load provider %q API key: %w", providerID, err)
		}
		keys[providerID] = key
	}
	return keys, nil
}
