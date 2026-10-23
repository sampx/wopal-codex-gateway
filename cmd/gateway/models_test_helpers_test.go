package main

import (
	"wopal-codex-gateway/internal/config"
)

func modelsIntPtr(value int) *int { return &value }

func modelsBoolPtr(value bool) *bool { return &value }

func namesOfModels(models []config.ExternalModelConfig) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}
