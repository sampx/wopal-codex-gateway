package codex

import (
	"regexp"
	"strings"
	"testing"
)

func TestCaptureRestoreStateReadsManagedKeys(t *testing.T) {
	text := `# user config
model = "gpt-5.4" # keep this comment
model_catalog_json = "/old/models.json" # old catalog
openai_base_url = "https://api.openai.com/v1"

[memories]
extract_model = "gpt-5-mini"
consolidation_model = "gpt-5-mini"
retention_days = 30
`

	state := captureRestoreState(text)
	if state.Model != (RestoreValue{HadValue: true, Value: "gpt-5.4"}) {
		t.Fatalf("model = %#v, want gpt-5.4", state.Model)
	}
	if state.ModelCatalogJSON != (RestoreValue{HadValue: true, Value: "/old/models.json"}) {
		t.Fatalf("model catalog = %#v", state.ModelCatalogJSON)
	}
	if state.OpenAIBaseURL != (RestoreValue{HadValue: true, Value: "https://api.openai.com/v1"}) {
		t.Fatalf("base URL = %#v", state.OpenAIBaseURL)
	}
	if state.MemoriesSectionExisted == nil || !*state.MemoriesSectionExisted {
		t.Fatalf("memoriesSectionExisted = %#v, want true", state.MemoriesSectionExisted)
	}
	if state.MemoryExtractModel == nil || *state.MemoryExtractModel != (RestoreValue{HadValue: true, Value: "gpt-5-mini"}) {
		t.Fatalf("extract model = %#v", state.MemoryExtractModel)
	}
	if state.MemoryConsolidationModel == nil || *state.MemoryConsolidationModel != (RestoreValue{HadValue: true, Value: "gpt-5-mini"}) {
		t.Fatalf("consolidation model = %#v", state.MemoryConsolidationModel)
	}
}

func TestCaptureRestoreApplyRestoreRoundTripPreservesUnrelatedContent(t *testing.T) {
	text := `# user config
model = "gpt-5.4" # keep this comment
approval_policy = "on-request" # keep me
model_catalog_json = "/old/models.json" # old catalog
openai_base_url = "https://api.openai.com/v1" # old base

[model_providers]
example = "kept"

[memories]
# memory comment
extract_model = "gpt-5-mini" # old extract
consolidation_model = "gpt-5-mini" # old consolidation
retention_days = 30

[projects."/tmp/example"]
trust_level = "trusted"
`

	state := captureRestoreState(text)
	applied := applyGatewayConfig(text, "/tmp/wopal-models.json", "http://127.0.0.1:11435/v1")

	if !strings.Contains(applied, `model = "gpt-5.4" # keep this comment`) {
		t.Fatalf("apply changed the model line:\n%s", applied)
	}
	if !strings.Contains(applied, `model_catalog_json = "/tmp/wopal-models.json" # old catalog`) {
		t.Fatalf("apply did not replace model_catalog_json while preserving its comment:\n%s", applied)
	}
	if !strings.Contains(applied, `openai_base_url = "http://127.0.0.1:11435/v1" # old base`) {
		t.Fatalf("apply did not replace openai_base_url while preserving its comment:\n%s", applied)
	}
	if !strings.Contains(applied, `extract_model = "wopal-ai/deepseek-v4.1-flash" # old extract`) {
		t.Fatalf("apply did not replace extract_model while preserving its comment:\n%s", applied)
	}
	if !strings.Contains(applied, `consolidation_model = "wopal-ai/deepseek-v4.1-flash" # old consolidation`) {
		t.Fatalf("apply did not replace consolidation_model while preserving its comment:\n%s", applied)
	}
	for _, untouched := range []string{
		`approval_policy = "on-request" # keep me`,
		`example = "kept"`,
		`# memory comment`,
		`retention_days = 30`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(applied, untouched) {
			t.Fatalf("apply lost unrelated line %q:\n%s", untouched, applied)
		}
	}

	restored := restoreGatewayConfig(applied, state)
	if restored != text {
		t.Fatalf("restore did not round-trip to the original text:\n--- got ---\n%s\n--- want ---\n%s", restored, text)
	}
}

func TestApplyGatewayDoesNotWriteModel(t *testing.T) {
	text := `approval_policy = "on-request"
`
	applied := applyGatewayConfig(text, "/tmp/wopal-models.json", "http://127.0.0.1:11435/v1")

	rootModel := regexp.MustCompile(`(?m)^\s*model\s*=`)
	if rootModel.MatchString(applied) {
		t.Fatalf("apply introduced a root model assignment:\n%s", applied)
	}
	if captureRestoreState(applied).Model.HadValue {
		t.Fatalf("captured model is present after apply:\n%s", applied)
	}
}

func TestApplyGatewayUsesConfiguredCompactionModel(t *testing.T) {
	text := `[memories]
extract_model = "old-extract"
consolidation_model = "old-consolidation"
`
	applied := applyGatewayConfigWithMemoryModel(
		text,
		"/tmp/wopal-models.json",
		"http://127.0.0.1:11435/v1",
		"vendor/friendly",
	)

	if !strings.Contains(applied, `extract_model = "vendor/friendly"`) {
		t.Fatalf("apply did not use compaction role for extract_model:\n%s", applied)
	}
	if !strings.Contains(applied, `consolidation_model = "vendor/friendly"`) {
		t.Fatalf("apply did not use compaction role for consolidation_model:\n%s", applied)
	}
}

func TestApplyGatewayQuotesStringsAsJSON(t *testing.T) {
	applied := applyGatewayConfig("", "/tmp/models.json", `http://127.0.0.1:11435/v1?name="quoted"`)
	state := captureRestoreState(applied)
	if state.OpenAIBaseURL != (RestoreValue{HadValue: true, Value: `http://127.0.0.1:11435/v1?name="quoted"`}) {
		t.Fatalf("base URL = %#v, want JSON-quoted round trip", state.OpenAIBaseURL)
	}
	if !strings.Contains(applied, `\"quoted\"`) {
		t.Fatalf("base URL was not JSON-escaped:\n%s", applied)
	}
}

func TestRestoreGatewayRestoresWopalModel(t *testing.T) {
	original := "model = \"gpt-5.4\" # user choice\n"
	state := captureRestoreState(original)
	applied := applyGatewayConfig(original, "/tmp/models.json", "http://127.0.0.1:11435/v1")
	managed := setRootString(applied, "model", "wopal-ai/deepseek-v4.1-flash")

	restored := restoreGatewayConfig(managed, state)
	if captureRestoreState(restored).Model != (RestoreValue{HadValue: true, Value: "gpt-5.4"}) {
		t.Fatalf("model was not restored:\n%s", restored)
	}
	if !strings.Contains(restored, "model = \"gpt-5.4\" # user choice") {
		t.Fatalf("model line comment was not preserved:\n%s", restored)
	}
}

func TestRestoreGatewayRemovesWopalModelWhenOriginallyMissing(t *testing.T) {
	original := "approval_policy = \"on-request\"\n"
	state := captureRestoreState(original)
	managed := setRootString(original, "model", "wopal-ai/deepseek-v4.1-flash")

	restored := restoreGatewayConfig(managed, state)
	if regexp.MustCompile(`(?m)^\s*model\s*=`).MatchString(restored) {
		t.Fatalf("managed model was not removed:\n%s", restored)
	}
}

func TestRestoreGatewayKeepsOfficialModel(t *testing.T) {
	original := "model = \"gpt-5.4\"\n"
	state := captureRestoreState(original)

	restored := restoreGatewayConfig("model = \"gpt-5.5\"\n", state)
	if captureRestoreState(restored).Model != (RestoreValue{HadValue: true, Value: "gpt-5.5"}) {
		t.Fatalf("official user model was overwritten:\n%s", restored)
	}
}

func TestRestoreGatewayRemovesMemoriesSectionWhenOriginallyAbsent(t *testing.T) {
	original := "# top\nmodel = \"gpt-5.4\" # user choice\n"
	state := captureRestoreState(original)
	if state.MemoriesSectionExisted == nil || *state.MemoriesSectionExisted {
		t.Fatalf("memoriesSectionExisted = %#v, want false", state.MemoriesSectionExisted)
	}

	applied := applyGatewayConfig(original, "/tmp/models.json", "http://127.0.0.1:11435/v1")
	if !strings.Contains(applied, "[memories]") {
		t.Fatalf("apply did not create [memories]:\n%s", applied)
	}

	restored := restoreGatewayConfig(applied, state)
	if strings.Contains(restored, "[memories]") {
		t.Fatalf("empty [memories] was not removed:\n%s", restored)
	}
	if restored != original {
		t.Fatalf("restore did not return to original text:\n--- got ---\n%s\n--- want ---\n%s", restored, original)
	}
}

func TestRestoreGatewayMergeStatePrefersSavedValues(t *testing.T) {
	existed := false
	saved := CodexRestoreState{
		Model:            RestoreValue{HadValue: true, Value: "saved-model"},
		ModelCatalogJSON: RestoreValue{HadValue: true, Value: "/saved/catalog.json"},
		OpenAIBaseURL:    RestoreValue{HadValue: true, Value: "https://saved.example/v1"},
	}
	current := CodexRestoreState{
		Model:                    RestoreValue{HadValue: true, Value: "current-model"},
		ModelCatalogJSON:         RestoreValue{HadValue: true, Value: "/current/catalog.json"},
		OpenAIBaseURL:            RestoreValue{HadValue: true, Value: "https://current.example/v1"},
		MemoriesSectionExisted:   &existed,
		MemoryExtractModel:       &RestoreValue{HadValue: true, Value: "current-extract"},
		MemoryConsolidationModel: &RestoreValue{HadValue: false},
	}

	merged := mergeGatewayRestoreState(saved, current)
	if merged.Model.Value != "saved-model" || merged.ModelCatalogJSON.Value != "/saved/catalog.json" || merged.OpenAIBaseURL.Value != "https://saved.example/v1" {
		t.Fatalf("saved root values did not win: %#v", merged)
	}
	if merged.MemoriesSectionExisted == nil || *merged.MemoriesSectionExisted {
		t.Fatalf("nil memoriesSectionExisted was not filled from current: %#v", merged.MemoriesSectionExisted)
	}
	if merged.MemoryExtractModel == nil || merged.MemoryExtractModel.Value != "current-extract" {
		t.Fatalf("nil extract model was not filled from current: %#v", merged.MemoryExtractModel)
	}
	if merged.MemoryConsolidationModel == nil || merged.MemoryConsolidationModel.HadValue {
		t.Fatalf("nil consolidation model was not filled from current: %#v", merged.MemoryConsolidationModel)
	}
}
