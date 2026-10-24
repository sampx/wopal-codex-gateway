package main

import (
	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
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
	return &resolved, nil
}
