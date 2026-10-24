package catalog

import (
	"strings"
	"testing"
	"wopal-codex-gateway/internal/config"
)

func TestCatalogKeepsOneCanonicalInstructionWithoutMutatingInput(t *testing.T) {
	for _, tc := range []struct {
		name, base, template string
		hasTemplate, removed bool
	}{
		{"duplicate", "same", "same", true, true},
		{"distinct", "fallback", "canonical", true, false},
		{"base only", "fallback", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := map[string]any{"slug": "official", "base_instructions": tc.base}
			if tc.hasTemplate {
				native["model_messages"] = map[string]any{"instructions_template": tc.template, "persistent_instructions": "preserved"}
			}
			got := buildCatalog([]map[string]any{native}, nil, config.DefaultToolMode)[0]
			_, exists := got["base_instructions"]
			if exists == tc.removed {
				t.Fatalf("base field exists=%t; removed=%t", exists, tc.removed)
			}
			if native["base_instructions"] != tc.base {
				t.Fatal("native input mutated")
			}
			if tc.hasTemplate && got["model_messages"].(map[string]any)["persistent_instructions"] != "preserved" {
				t.Fatal("instruction fields lost")
			}
		})
	}
}

func TestExternalCatalogOmitsRedundantFields(t *testing.T) {
	got := buildCatalog(nil, []config.ExternalModelConfig{{Name: "a"}}, config.DefaultToolMode)[0]
	for _, field := range strings.Fields("additional_speed_tiers apply_patch_tool_type auto_compact_token_limit availability_nux default_service_tier default_verbosity model_messages service_tiers supports_image_detail_original supports_parallel_tool_calls supports_reasoning_summaries supports_search_tool upgrade web_search_tool_type") {
		if _, exists := got[field]; exists {
			t.Errorf("redundant field %q present", field)
		}
	}
	for _, field := range []string{"base_instructions", "shell_type", "truncation_policy", "input_modalities", "tool_mode", "supported_reasoning_levels"} {
		if _, exists := got[field]; !exists {
			t.Errorf("required or intentional field %q missing", field)
		}
	}
}
