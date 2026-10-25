package catalog

import (
	"encoding/json"
	"reflect"
	"testing"

	"wopal-codex-gateway/internal/config"
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
	existing := config.ExternalModelConfig{
		Name:          "glm-5.3-flash-free",
		ContextWindow: modelsIntPtr(524288),
		Vision:        modelsBoolPtr(true),
		Reasoning:     modelsBoolPtr(false),
	}
	tests := []struct {
		name         string
		current      []config.ExternalModelConfig
		available    []string
		excluded     []string
		wantModels   []config.ExternalModelConfig
		wantSkipped  []string
		wantExcluded []string
	}{
		{
			name:      "keeps existing metadata and skips non-chat models",
			current:   []config.ExternalModelConfig{existing},
			available: []string{"glm-5.3-flash-free", "deepseek-v4-flash-free", "deepseek-v4-flash-free", "nomic-embed-text-v2", "bge-RERANKER-v2-m3"},
			wantModels: []config.ExternalModelConfig{
				existing,
				{Name: "deepseek-v4-flash-free"},
			},
			wantSkipped:  []string{"nomic-embed-text-v2", "bge-RERANKER-v2-m3"},
			wantExcluded: []string{},
		},
		{
			name:         "normalizes and records excluded models before skip checks",
			available:    []string{"keep", "broken-model", "embed-demo", "other-embedding-model", "another-reranker"},
			excluded:     []string{" wopal-ai/broken-model ", "embed-demo", ""},
			wantModels:   []config.ExternalModelConfig{{Name: "keep"}},
			wantSkipped:  []string{"other-embedding-model", "another-reranker"},
			wantExcluded: []string{"broken-model", "embed-demo"},
		},
		{
			name:      "deduplicates trimmed available ids in API order",
			available: []string{" glm-5.3 ", "glm-5.3", "kimi-k3"},
			wantModels: []config.ExternalModelConfig{
				{Name: "glm-5.3"},
				{Name: "kimi-k3"},
			},
			wantSkipped:  []string{},
			wantExcluded: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SyncWopalModelConfigs(test.current, test.available, test.excluded)
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
	existing := config.ExternalModelConfig{
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
		current      []config.ExternalModelConfig
		names        []string
		defaults     ModelPatch
		wantModels   []config.ExternalModelConfig
		wantAdded    []string
		wantExisting []string
	}{
		{
			name:     "adds normalized unique models with defaults and preserves existing entries",
			current:  []config.ExternalModelConfig{existing},
			names:    []string{"wopal-ai/existing", "new-model", " new-model ", "wopal-ai/other"},
			defaults: fullDefaults,
			wantModels: []config.ExternalModelConfig{
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
			wantModels:   []config.ExternalModelConfig{{Name: "plain"}},
			wantAdded:    []string{"plain"},
			wantExisting: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := AddWopalModelConfigs(test.current, test.names, test.defaults)
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
	alpha := config.ExternalModelConfig{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)}
	beta := config.ExternalModelConfig{Name: "beta", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}
	tests := []struct {
		name        string
		current     []config.ExternalModelConfig
		names       []string
		patch       ModelPatch
		wantModels  []config.ExternalModelConfig
		wantUpdated []string
		wantMissing []string
	}{
		{
			name:    "updates only requested metadata and reports missing models",
			current: []config.ExternalModelConfig{alpha, beta},
			names:   []string{"wopal-ai/alpha", "missing", "alpha"},
			patch:   ModelPatch{Vision: modelsBoolPtr(true)},
			wantModels: []config.ExternalModelConfig{
				{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)},
				beta,
			},
			wantUpdated: []string{"alpha"},
			wantMissing: []string{"missing"},
		},
		{
			name:        "reports a missing model without creating it",
			current:     []config.ExternalModelConfig{{Name: "alpha", ContextWindow: modelsIntPtr(262144)}},
			names:       []string{"missing"},
			patch:       ModelPatch{ContextWindow: modelsIntPtr(1000000)},
			wantModels:  []config.ExternalModelConfig{{Name: "alpha", ContextWindow: modelsIntPtr(262144)}},
			wantUpdated: []string{},
			wantMissing: []string{"missing"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := UpdateWopalModelConfigs(test.current, test.names, test.patch)
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
	current := []config.ExternalModelConfig{{Name: "alpha", Vision: modelsBoolPtr(false)}}

	UpdateWopalModelConfigs(current, []string{"alpha"}, ModelPatch{Vision: modelsBoolPtr(true)})

	if *current[0].Vision {
		t.Fatal("update mutated the caller's model")
	}
}

func TestWopalModelRemove(t *testing.T) {
	tests := []struct {
		name        string
		current     []config.ExternalModelConfig
		names       []string
		wantModels  []config.ExternalModelConfig
		wantRemoved []string
		wantMissing []string
	}{
		{
			name:        "removes configured models and reports missing names in request order",
			current:     []config.ExternalModelConfig{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}},
			names:       []string{"wopal-ai/alpha", "missing", "alpha", "gamma"},
			wantModels:  []config.ExternalModelConfig{{Name: "beta"}},
			wantRemoved: []string{"alpha", "gamma"},
			wantMissing: []string{"missing"},
		},
		{
			name:        "reports only missing models when nothing matches",
			current:     []config.ExternalModelConfig{{Name: "alpha"}},
			names:       []string{"missing", "also-missing"},
			wantModels:  []config.ExternalModelConfig{{Name: "alpha"}},
			wantRemoved: []string{},
			wantMissing: []string{"missing", "also-missing"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RemoveWopalModelConfigs(test.current, test.names)
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

func namesOfModels(models []config.ExternalModelConfig) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}

func TestModelPatchReset(t *testing.T) {
	original := config.ExternalModelConfig{Name: "a", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)}
	for _, tc := range []struct {
		name  string
		patch ModelPatch
		want  config.ExternalModelConfig
	}{
		{"context", ModelPatch{ResetContextWindow: true}, config.ExternalModelConfig{Name: "a", Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)}},
		{"vision", ModelPatch{ResetVision: true}, config.ExternalModelConfig{Name: "a", ContextWindow: modelsIntPtr(131072), Reasoning: modelsBoolPtr(false)}},
		{"reasoning", ModelPatch{ResetReasoning: true}, config.ExternalModelConfig{Name: "a", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false)}},
		{"all", ModelPatch{ResetContextWindow: true, ResetVision: true, ResetReasoning: true}, config.ExternalModelConfig{Name: "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := UpdateWopalModelConfigs([]config.ExternalModelConfig{original, {Name: "untouched"}}, []string{"a"}, tc.patch)
			if !reflect.DeepEqual(got.Models, []config.ExternalModelConfig{tc.want, {Name: "untouched"}}) {
				t.Fatalf("reset = %+v", got.Models)
			}
			if original.ContextWindow == nil || *original.ContextWindow != 131072 || *original.Vision || *original.Reasoning {
				t.Fatal("reset mutated original")
			}
		})
	}
}

func TestSyncPreservesUnsetAndFalseOverrides(t *testing.T) {
	current := []config.ExternalModelConfig{{Name: "bare"}, {Name: "explicit", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)}}
	got := SyncWopalModelConfigs(current, []string{"bare", "explicit", "new"}, nil)
	want := append(append([]config.ExternalModelConfig{}, current...), config.ExternalModelConfig{Name: "new"})
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("sync = %+v, want %+v", got.Models, want)
	}
	*got.Models[1].ContextWindow = 9
	*got.Models[1].Vision = true
	*got.Models[1].Reasoning = true
	if *current[1].ContextWindow != 262144 || *current[1].Vision || *current[1].Reasoning {
		t.Fatal("sync shares pointers with current")
	}
}

func TestWopalModelOperationsPreserveExtensions(t *testing.T) {
	original := config.ExternalModelConfig{Name: "alpha"}
	if err := json.Unmarshal([]byte(`{"name":"alpha","vision":false,"future":{"keep":1}}`), &original); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, model config.ExternalModelConfig) {
		t.Helper()
		if model.Name != "alpha" || model.Vision == nil || *model.Vision {
			t.Fatalf("typed fields changed: %+v", model)
		}
		raw, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["future"]) != `{"keep":1}` {
			t.Fatalf("extension lost: %s", raw)
		}
	}

	t.Run("sync", func(t *testing.T) {
		result := SyncWopalModelConfigs([]config.ExternalModelConfig{original}, []string{"alpha"}, nil)
		if len(result.Models) != 1 {
			t.Fatalf("models = %+v", result.Models)
		}
		check(t, result.Models[0])
	})
	t.Run("add", func(t *testing.T) {
		result := AddWopalModelConfigs([]config.ExternalModelConfig{original}, []string{"alpha", "beta"}, ModelPatch{})
		if len(result.Models) != 2 {
			t.Fatalf("models = %+v", result.Models)
		}
		check(t, result.Models[0])
	})
	t.Run("update unrelated field", func(t *testing.T) {
		result := UpdateWopalModelConfigs([]config.ExternalModelConfig{original}, []string{"alpha"}, ModelPatch{ContextWindow: modelsIntPtr(131072)})
		if len(result.Models) != 1 {
			t.Fatalf("models = %+v", result.Models)
		}
		check(t, result.Models[0])
	})
	t.Run("resolve", func(t *testing.T) {
		resolved, _ := ResolveModel(original, nil)
		check(t, resolved)
	})
}
