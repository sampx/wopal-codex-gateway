package main

import (
	"reflect"
	"testing"
)

func modelsIntPtr(value int) *int    { return &value }
func modelsBoolPtr(value bool) *bool { return &value }

func TestWopalModelNormalizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "blank", in: "   ", want: ""},
		{name: "trim", in: "  glm-5.3  ", want: "glm-5.3"},
		{name: "prefix", in: "wopal-ai/glm-5.3", want: "glm-5.3"},
		{name: "trim then prefix", in: "  wopal-ai/glm-5.3  ", want: "glm-5.3"},
		{name: "only one prefix", in: "wopal-ai/wopal-ai/glm-5.3", want: "wopal-ai/glm-5.3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeWopalModelName(test.in); got != test.want {
				t.Fatalf("normalizeWopalModelName(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestWopalModelSync(t *testing.T) {
	existing := ExternalModelConfig{
		Name:          "glm-5.3-flash-free",
		ContextWindow: modelsIntPtr(524288),
		Vision:        modelsBoolPtr(true),
		Reasoning:     modelsBoolPtr(false),
	}
	tests := []struct {
		name         string
		current      []ExternalModelConfig
		available    []string
		excluded     []string
		wantModels   []ExternalModelConfig
		wantSkipped  []string
		wantExcluded []string
	}{
		{
			name:      "keeps existing metadata and skips non-chat models",
			current:   []ExternalModelConfig{existing},
			available: []string{"glm-5.3-flash-free", "deepseek-v4-flash-free", "deepseek-v4-flash-free", "nomic-embed-text-v2", "bge-RERANKER-v2-m3"},
			wantModels: []ExternalModelConfig{
				existing,
				{Name: "deepseek-v4-flash-free", ContextWindow: modelsIntPtr(defaultContextWindow), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)},
			},
			wantSkipped:  []string{"nomic-embed-text-v2", "bge-RERANKER-v2-m3"},
			wantExcluded: []string{},
		},
		{
			name:         "normalizes and records excluded models before skip checks",
			available:    []string{"keep", "broken-model", "embed-demo", "other-embedding-model", "another-reranker"},
			excluded:     []string{" wopal-ai/broken-model ", "embed-demo", ""},
			wantModels:   []ExternalModelConfig{{Name: "keep", ContextWindow: modelsIntPtr(defaultContextWindow), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)}},
			wantSkipped:  []string{"other-embedding-model", "another-reranker"},
			wantExcluded: []string{"broken-model", "embed-demo"},
		},
		{
			name:      "deduplicates trimmed available ids in API order",
			available: []string{" glm-5.3 ", "glm-5.3", "kimi-k3"},
			wantModels: []ExternalModelConfig{
				{Name: "glm-5.3", ContextWindow: modelsIntPtr(defaultContextWindow), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)},
				{Name: "kimi-k3", ContextWindow: modelsIntPtr(defaultContextWindow), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)},
			},
			wantSkipped:  []string{},
			wantExcluded: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := syncWopalModelConfigs(test.current, test.available, test.excluded)
			if !reflect.DeepEqual(got.Models, test.wantModels) {
				t.Fatalf("Models = %#v, want %#v", got.Models, test.wantModels)
			}
			if !reflect.DeepEqual(got.Skipped, test.wantSkipped) {
				t.Fatalf("Skipped = %#v, want %#v", got.Skipped, test.wantSkipped)
			}
			if !reflect.DeepEqual(got.Excluded, test.wantExcluded) {
				t.Fatalf("Excluded = %#v, want %#v", got.Excluded, test.wantExcluded)
			}
		})
	}
}

func TestWopalModelAdd(t *testing.T) {
	existing := ExternalModelConfig{
		Name:          "existing",
		ContextWindow: modelsIntPtr(524288),
		Vision:        modelsBoolPtr(true),
		Reasoning:     modelsBoolPtr(false),
	}
	fullDefaults := ModelPatch{
		ContextWindow: modelsIntPtr(131072),
		Vision:        modelsBoolPtr(false),
		Reasoning:     modelsBoolPtr(true),
	}
	tests := []struct {
		name         string
		current      []ExternalModelConfig
		names        []string
		defaults     ModelPatch
		wantModels   []ExternalModelConfig
		wantAdded    []string
		wantExisting []string
	}{
		{
			name:     "adds normalized unique models with defaults and preserves existing entries",
			current:  []ExternalModelConfig{existing},
			names:    []string{"wopal-ai/existing", "new-model", " new-model ", "wopal-ai/other"},
			defaults: fullDefaults,
			wantModels: []ExternalModelConfig{
				existing,
				{Name: "new-model", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)},
				{Name: "other", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)},
			},
			wantAdded:    []string{"new-model", "other"},
			wantExisting: []string{"existing", "new-model"},
		},
		{
			name:         "leaves nil default fields unset and skips blank names",
			names:        []string{"", "   ", "wopal-ai/plain"},
			wantModels:   []ExternalModelConfig{{Name: "plain"}},
			wantAdded:    []string{"plain"},
			wantExisting: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := addWopalModelConfigs(test.current, test.names, test.defaults)
			if !reflect.DeepEqual(got.Models, test.wantModels) {
				t.Fatalf("Models = %#v, want %#v", got.Models, test.wantModels)
			}
			if !reflect.DeepEqual(got.Added, test.wantAdded) {
				t.Fatalf("Added = %#v, want %#v", got.Added, test.wantAdded)
			}
			if !reflect.DeepEqual(got.Existing, test.wantExisting) {
				t.Fatalf("Existing = %#v, want %#v", got.Existing, test.wantExisting)
			}
		})
	}
}

func TestWopalModelUpdate(t *testing.T) {
	alpha := ExternalModelConfig{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)}
	beta := ExternalModelConfig{Name: "beta", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}
	tests := []struct {
		name        string
		current     []ExternalModelConfig
		names       []string
		patch       ModelPatch
		wantModels  []ExternalModelConfig
		wantUpdated []string
		wantMissing []string
	}{
		{
			name:    "updates only requested metadata and reports missing models",
			current: []ExternalModelConfig{alpha, beta},
			names:   []string{"wopal-ai/alpha", "missing", "alpha"},
			patch:   ModelPatch{Vision: modelsBoolPtr(true)},
			wantModels: []ExternalModelConfig{
				{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)},
				beta,
			},
			wantUpdated: []string{"alpha"},
			wantMissing: []string{"missing"},
		},
		{
			name:        "reports a missing model without creating it",
			current:     []ExternalModelConfig{{Name: "alpha", ContextWindow: modelsIntPtr(262144)}},
			names:       []string{"missing"},
			patch:       ModelPatch{ContextWindow: modelsIntPtr(1000000)},
			wantModels:  []ExternalModelConfig{{Name: "alpha", ContextWindow: modelsIntPtr(262144)}},
			wantUpdated: []string{},
			wantMissing: []string{"missing"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := updateWopalModelConfigs(test.current, test.names, test.patch)
			if !reflect.DeepEqual(got.Models, test.wantModels) {
				t.Fatalf("Models = %#v, want %#v", got.Models, test.wantModels)
			}
			if !reflect.DeepEqual(got.Updated, test.wantUpdated) {
				t.Fatalf("Updated = %#v, want %#v", got.Updated, test.wantUpdated)
			}
			if !reflect.DeepEqual(got.Missing, test.wantMissing) {
				t.Fatalf("Missing = %#v, want %#v", got.Missing, test.wantMissing)
			}
		})
	}
}

func TestWopalModelUpdateDoesNotMutateInput(t *testing.T) {
	current := []ExternalModelConfig{{Name: "alpha", Vision: modelsBoolPtr(false)}}

	updateWopalModelConfigs(current, []string{"alpha"}, ModelPatch{Vision: modelsBoolPtr(true)})

	if *current[0].Vision {
		t.Fatal("update mutated the caller's model")
	}
}

func TestWopalModelRemove(t *testing.T) {
	tests := []struct {
		name        string
		current     []ExternalModelConfig
		names       []string
		wantModels  []ExternalModelConfig
		wantRemoved []string
		wantMissing []string
	}{
		{
			name:        "removes configured models and reports missing names in request order",
			current:     []ExternalModelConfig{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}},
			names:       []string{"wopal-ai/alpha", "missing", "alpha", "gamma"},
			wantModels:  []ExternalModelConfig{{Name: "beta"}},
			wantRemoved: []string{"alpha", "gamma"},
			wantMissing: []string{"missing"},
		},
		{
			name:        "reports only missing models when nothing matches",
			current:     []ExternalModelConfig{{Name: "alpha"}},
			names:       []string{"missing", "also-missing"},
			wantModels:  []ExternalModelConfig{{Name: "alpha"}},
			wantRemoved: []string{},
			wantMissing: []string{"missing", "also-missing"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := removeWopalModelConfigs(test.current, test.names)
			if !reflect.DeepEqual(got.Models, test.wantModels) {
				t.Fatalf("Models = %#v, want %#v", got.Models, test.wantModels)
			}
			if !reflect.DeepEqual(got.Removed, test.wantRemoved) {
				t.Fatalf("Removed = %#v, want %#v", got.Removed, test.wantRemoved)
			}
			if !reflect.DeepEqual(got.Missing, test.wantMissing) {
				t.Fatalf("Missing = %#v, want %#v", got.Missing, test.wantMissing)
			}
		})
	}
}

func namesOfModels(models []ExternalModelConfig) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}
