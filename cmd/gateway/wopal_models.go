package main

import (
	"regexp"
	"strings"
)

// WopalModelSyncResult reports what models sync changed.
type WopalModelSyncResult struct {
	Models   []ExternalModelConfig
	Skipped  []string
	Excluded []string
}

// AddWopalModelsResult reports what models add changed.
type AddWopalModelsResult struct {
	Models   []ExternalModelConfig
	Added    []string
	Existing []string
}

// UpdateWopalModelsResult reports what models set changed.
type UpdateWopalModelsResult struct {
	Models  []ExternalModelConfig
	Updated []string
	Missing []string
}

// RemoveWopalModelsResult reports what models remove changed.
type RemoveWopalModelsResult struct {
	Models  []ExternalModelConfig
	Removed []string
	Missing []string
}

// ModelPatch carries the metadata a caller wants to change. A nil field is left
// untouched so `models set --vision` never clears the context window.
type ModelPatch struct {
	ContextWindow *int
	Vision        *bool
	Reasoning     *bool
}

var nonChatModelPattern = regexp.MustCompile(`(?i)(embed(ding)?|rerank)`)

func syncWopalModelConfigs(current []ExternalModelConfig, availableIDs, excludedNames []string) WopalModelSyncResult {
	existing := make(map[string]ExternalModelConfig, len(current))
	for _, model := range current {
		existing[model.Name] = model
	}

	excludedSet := make(map[string]struct{}, len(excludedNames))
	for _, name := range excludedNames {
		name = normalizeWopalModelName(name)
		if name != "" {
			excludedSet[name] = struct{}{}
		}
	}

	result := WopalModelSyncResult{
		Models:   make([]ExternalModelConfig, 0, len(availableIDs)),
		Skipped:  []string{},
		Excluded: []string{},
	}
	seen := make(map[string]struct{}, len(availableIDs))
	for _, rawID := range availableIDs {
		name := strings.TrimSpace(rawID)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if _, ok := excludedSet[name]; ok {
			result.Excluded = append(result.Excluded, name)
			continue
		}
		if nonChatModelPattern.MatchString(name) {
			result.Skipped = append(result.Skipped, name)
			continue
		}

		if model, ok := existing[name]; ok {
			result.Models = append(result.Models, cloneExternalModel(model))
			continue
		}
		result.Models = append(result.Models, defaultExternalModelConfig(name))
	}
	return result
}

func addWopalModelConfigs(current []ExternalModelConfig, names []string, defaults ModelPatch) AddWopalModelsResult {
	models := make([]ExternalModelConfig, 0, len(current)+len(names))
	known := make(map[string]struct{}, len(current)+len(names))
	for _, model := range current {
		models = append(models, cloneExternalModel(model))
		known[model.Name] = struct{}{}
	}

	result := AddWopalModelsResult{Models: models, Added: []string{}, Existing: []string{}}
	reportedExisting := make(map[string]struct{}, len(names))
	for _, rawName := range names {
		name := normalizeWopalModelName(rawName)
		if name == "" {
			continue
		}
		if _, ok := known[name]; ok {
			if _, reported := reportedExisting[name]; !reported {
				reportedExisting[name] = struct{}{}
				result.Existing = append(result.Existing, name)
			}
			continue
		}

		known[name] = struct{}{}
		result.Models = append(result.Models, applyModelPatch(ExternalModelConfig{Name: name}, defaults))
		result.Added = append(result.Added, name)
	}
	return result
}

func updateWopalModelConfigs(current []ExternalModelConfig, names []string, patch ModelPatch) UpdateWopalModelsResult {
	requested := uniqueNormalizedModelNames(names)
	requestedSet := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		requestedSet[name] = struct{}{}
	}
	existing := make(map[string]struct{}, len(current))
	for _, model := range current {
		existing[model.Name] = struct{}{}
	}

	result := UpdateWopalModelsResult{
		Models:  make([]ExternalModelConfig, 0, len(current)),
		Updated: []string{},
		Missing: []string{},
	}
	for _, name := range requested {
		if _, ok := existing[name]; ok {
			result.Updated = append(result.Updated, name)
		} else {
			result.Missing = append(result.Missing, name)
		}
	}
	for _, model := range current {
		clone := cloneExternalModel(model)
		if _, ok := requestedSet[model.Name]; ok {
			clone = applyModelPatch(clone, patch)
		}
		result.Models = append(result.Models, clone)
	}
	return result
}

func removeWopalModelConfigs(current []ExternalModelConfig, names []string) RemoveWopalModelsResult {
	requested := uniqueNormalizedModelNames(names)
	existing := make(map[string]struct{}, len(current))
	for _, model := range current {
		existing[model.Name] = struct{}{}
	}

	remove := make(map[string]struct{}, len(requested))
	result := RemoveWopalModelsResult{Models: []ExternalModelConfig{}, Removed: []string{}, Missing: []string{}}
	for _, name := range requested {
		if _, ok := existing[name]; ok {
			remove[name] = struct{}{}
			result.Removed = append(result.Removed, name)
		} else {
			result.Missing = append(result.Missing, name)
		}
	}
	for _, model := range current {
		if _, ok := remove[model.Name]; ok {
			continue
		}
		result.Models = append(result.Models, cloneExternalModel(model))
	}
	return result
}

func normalizeWopalModelName(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "wopal-ai/") {
		return strings.TrimPrefix(name, "wopal-ai/")
	}
	return name
}

func uniqueNormalizedModelNames(names []string) []string {
	unique := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, rawName := range names {
		name := normalizeWopalModelName(rawName)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	return unique
}

func defaultExternalModelConfig(name string) ExternalModelConfig {
	contextWindow := defaultContextWindow
	vision := false
	reasoning := true
	return ExternalModelConfig{
		Name:          name,
		ContextWindow: &contextWindow,
		Vision:        &vision,
		Reasoning:     &reasoning,
	}
}

func cloneExternalModel(model ExternalModelConfig) ExternalModelConfig {
	clone := ExternalModelConfig{Name: model.Name}
	if model.ContextWindow != nil {
		contextWindow := *model.ContextWindow
		clone.ContextWindow = &contextWindow
	}
	if model.Vision != nil {
		vision := *model.Vision
		clone.Vision = &vision
	}
	if model.Reasoning != nil {
		reasoning := *model.Reasoning
		clone.Reasoning = &reasoning
	}
	return clone
}

func applyModelPatch(model ExternalModelConfig, patch ModelPatch) ExternalModelConfig {
	if patch.ContextWindow != nil {
		contextWindow := *patch.ContextWindow
		model.ContextWindow = &contextWindow
	}
	if patch.Vision != nil {
		vision := *patch.Vision
		model.Vision = &vision
	}
	if patch.Reasoning != nil {
		reasoning := *patch.Reasoning
		model.Reasoning = &reasoning
	}
	return model
}
