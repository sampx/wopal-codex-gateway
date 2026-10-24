package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"wopal-codex-gateway/internal/config"
)

// Opt in to the installed Codex reader without changing its active catalog or
// invoking inference. This guards the generated wire format across upgrades.
func TestCompactCatalogInstalledCodexCompatibility(t *testing.T) {
	if os.Getenv("WCG_CODEX_COMPAT") != "1" {
		t.Skip("set WCG_CODEX_COMPAT=1 to use the installed Codex reader")
	}
	command, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(command, "debug", "models", "--bundled").Output()
	if err != nil {
		t.Fatal(err)
	}
	var native struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &native); err != nil {
		t.Fatal(err)
	}
	full := buildCatalog(nil, []config.ExternalModelConfig{{Name: "compat-model"}}, config.DefaultToolMode)
	// The former expanded defaults must normalize identically when omitted.
	for k, v := range map[string]any{
		"additional_speed_tiers": []any{}, "apply_patch_tool_type": nil,
		"auto_compact_token_limit": nil, "availability_nux": nil,
		"default_service_tier": nil, "default_verbosity": nil, "model_messages": nil,
		"service_tiers": []any{}, "supports_image_detail_original": false,
		"supports_parallel_tool_calls": true, "supports_reasoning_summaries": false,
		"supports_search_tool": false, "upgrade": nil, "web_search_tool_type": "text",
	} {
		full[0][k] = v
	}
	for _, entry := range native.Models {
		copy := make(map[string]any, len(entry))
		for k, v := range entry {
			copy[k] = v
		}
		copy["supported_in_api"] = false
		full = append(full, copy)
	}
	compact := buildCatalog(native.Models, []config.ExternalModelConfig{{Name: "compat-model"}}, config.DefaultToolMode)
	read := func(name string, models []map[string]any) []map[string]any {
		path := filepath.Join(t.TempDir(), name+".json")
		if err := writeCatalog(path, models); err != nil {
			t.Fatal(err)
		}
		override := fmt.Sprintf("model_catalog_json=%q", path)
		cmd := exec.Command(command, "-c", override, "debug", "models")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("Codex rejected %s: %v", name, err)
		}
		var result struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		return result.Models
	}
	if !reflect.DeepEqual(read("expanded", full), read("compact", compact)) {
		t.Fatal("slimming changed effective Codex model metadata")
	}
	version, err := exec.Command(command, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d models normalize identically", version, len(compact))
}
