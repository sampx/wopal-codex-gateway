package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var catalogEntryKeys = []string{
	"additional_speed_tiers",
	"apply_patch_tool_type",
	"auto_compact_token_limit",
	"availability_nux",
	"base_instructions",
	"context_window",
	"default_reasoning_level",
	"default_reasoning_summary",
	"default_service_tier",
	"default_verbosity",
	"description",
	"display_name",
	"effective_context_window_percent",
	"experimental_supported_tools",
	"include_apps_usage_instructions",
	"include_plugin_usage_instructions",
	"include_skills_usage_instructions",
	"input_modalities",
	"max_context_window",
	"model_messages",
	"priority",
	"service_tiers",
	"shell_type",
	"slug",
	"support_verbosity",
	"supported_in_api",
	"supported_reasoning_levels",
	"supports_image_detail_original",
	"supports_parallel_tool_calls",
	"supports_reasoning_summaries",
	"supports_reasoning_summary_parameter",
	"supports_search_tool",
	"truncation_policy",
	"upgrade",
	"visibility",
	"web_search_tool_type",
}

func catalogTestValue(t *testing.T, entry map[string]any, key string) any {
	t.Helper()
	value, ok := entry[key]
	if !ok {
		t.Fatalf("catalog entry is missing %q: %#v", key, entry)
	}
	return value
}

func catalogTestString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	value, ok := catalogTestValue(t, entry, key).(string)
	if !ok {
		t.Fatalf("catalog entry %q = %#v, want string", key, entry[key])
	}
	return value
}

func TestBuildCatalogWopalContractAndNativeCopy(t *testing.T) {
	contextWindow := 128000
	vision := true
	reasoning := false
	wopalModels := []ExternalModelConfig{
		{Name: "vision-model", ContextWindow: &contextWindow, Vision: &vision, Reasoning: &reasoning},
		{Name: "default-model"},
	}
	native := []map[string]any{
		{"slug": "native-hidden", "visibility": "hide", "supported_in_api": true, "custom": "kept"},
		{"slug": "native-visible", "visibility": "list", "supported_in_api": true},
		{"slug": "native-unlisted"},
	}

	got := buildCatalog(native, wopalModels)
	if len(got) != 5 {
		t.Fatalf("buildCatalog() returned %d entries, want 5", len(got))
	}
	if slug := catalogTestString(t, got[0], "slug"); slug != "wopal-ai/vision-model" {
		t.Fatalf("first entry slug = %q, want wopal-ai/vision-model", slug)
	}
	if slug := catalogTestString(t, got[1], "slug"); slug != "wopal-ai/default-model" {
		t.Fatalf("second entry slug = %q, want wopal-ai/default-model", slug)
	}
	if slug := catalogTestString(t, got[2], "slug"); slug != "native-hidden" {
		t.Fatalf("native entry did not follow Wopal entries: got %q", slug)
	}

	first := got[0]
	if len(first) != len(catalogEntryKeys) {
		t.Fatalf("Wopal entry has %d fields, want %d: %#v", len(first), len(catalogEntryKeys), first)
	}
	for _, key := range catalogEntryKeys {
		if _, ok := first[key]; !ok {
			t.Fatalf("Wopal entry is missing field %q", key)
		}
	}

	if value := catalogTestString(t, first, "display_name"); value != "Wopal AI · vision-model" {
		t.Fatalf("display_name = %q", value)
	}
	if value := catalogTestString(t, first, "description"); value != "Wopal AI (New API) model" {
		t.Fatalf("description = %q", value)
	}
	if value := catalogTestString(t, first, "visibility"); value != "list" {
		t.Fatalf("visibility = %q, want list", value)
	}
	if value, ok := catalogTestValue(t, first, "priority").(int); !ok || value != -200 {
		t.Fatalf("priority = %#v, want int -200", first["priority"])
	}
	if value, ok := catalogTestValue(t, first, "context_window").(int); !ok || value != contextWindow {
		t.Fatalf("context_window = %#v, want %d", first["context_window"], contextWindow)
	}
	if value, ok := catalogTestValue(t, first, "max_context_window").(int); !ok || value != contextWindow {
		t.Fatalf("max_context_window = %#v, want %d", first["max_context_window"], contextWindow)
	}
	if value, ok := catalogTestValue(t, first, "supported_in_api").(bool); !ok || !value {
		t.Fatalf("supported_in_api = %#v, want true", first["supported_in_api"])
	}
	if value := catalogTestString(t, first, "default_reasoning_level"); value != "none" {
		t.Fatalf("default_reasoning_level = %q, want none", value)
	}
	if value := catalogTestValue(t, first, "input_modalities"); !reflect.DeepEqual(value, []any{"text", "image"}) {
		t.Fatalf("input_modalities = %#v, want [text image]", value)
	}

	levels, ok := catalogTestValue(t, first, "supported_reasoning_levels").([]any)
	if !ok || len(levels) != 1 {
		t.Fatalf("supported_reasoning_levels = %#v, want one level", first["supported_reasoning_levels"])
	}
	if !reflect.DeepEqual(levels[0], map[string]any{"effort": "none", "description": "Turn reasoning off"}) {
		t.Fatalf("disabled reasoning level = %#v", levels[0])
	}

	second := got[1]
	if value, ok := catalogTestValue(t, second, "priority").(int); !ok || value != -199 {
		t.Fatalf("second priority = %#v, want int -199", second["priority"])
	}
	if value, ok := catalogTestValue(t, second, "context_window").(int); !ok || value != defaultContextWindow {
		t.Fatalf("default context_window = %#v, want %d", second["context_window"], defaultContextWindow)
	}
	if value := catalogTestValue(t, second, "input_modalities"); !reflect.DeepEqual(value, []any{"text"}) {
		t.Fatalf("default input_modalities = %#v, want [text]", value)
	}
	if value := catalogTestString(t, second, "default_reasoning_level"); value != "high" {
		t.Fatalf("default_reasoning_level = %q, want high", value)
	}
	levels, ok = catalogTestValue(t, second, "supported_reasoning_levels").([]any)
	if !ok || len(levels) != 4 {
		t.Fatalf("default supported_reasoning_levels = %#v, want four levels", second["supported_reasoning_levels"])
	}
	for index, effort := range []string{"none", "low", "medium", "high"} {
		level, ok := levels[index].(map[string]any)
		if !ok || level["effort"] != effort {
			t.Fatalf("reasoning level %d = %#v, want effort %q", index, levels[index], effort)
		}
	}

	if value := catalogTestString(t, second, "base_instructions"); value != "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals." {
		t.Fatalf("base_instructions = %q", value)
	}
	if value := catalogTestValue(t, second, "truncation_policy"); !reflect.DeepEqual(value, map[string]any{"limit": 10000, "mode": "tokens"}) {
		t.Fatalf("truncation_policy = %#v", value)
	}

	if value, ok := catalogTestValue(t, got[2], "supported_in_api").(bool); !ok || value {
		t.Fatalf("native supported_in_api = %#v, want false", got[2]["supported_in_api"])
	}
	if value := catalogTestString(t, got[2], "visibility"); value != "hide" {
		t.Fatalf("native hidden visibility = %q, want hide", value)
	}
	if value := catalogTestString(t, got[2], "custom"); value != "kept" {
		t.Fatalf("native custom field = %q, want kept", value)
	}
	if value := catalogTestString(t, got[3], "visibility"); value != "list" {
		t.Fatalf("native visible visibility = %q, want list", value)
	}
	if _, ok := got[4]["visibility"]; ok {
		t.Fatalf("native entry without visibility gained one: %#v", got[4])
	}
	if value, ok := native[0]["supported_in_api"].(bool); !ok || !value {
		t.Fatalf("buildCatalog mutated the native input: %#v", native[0])
	}
}

func TestCatalogWriteFileContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog := []map[string]any{{"slug": "wopal-ai/test", "visibility": "list"}}
	if err := writeCatalog(path, catalog); err != nil {
		t.Fatalf("writeCatalog() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("catalog mode = %o, want 600", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("catalog does not end with a newline: %q", string(data))
	}
	if !strings.Contains(string(data), "  \"models\": [\n") {
		t.Fatalf("catalog is not indented with two spaces: %q", string(data))
	}

	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("catalog JSON is invalid: %v", err)
	}
	if len(payload.Models) != 1 || payload.Models[0]["slug"] != "wopal-ai/test" {
		t.Fatalf("catalog payload = %#v", payload.Models)
	}
}

func TestCatalogLoadBundledDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		output     catalogCommandOutput
		resolveErr error
		want       []string
		notWant    string
	}{
		{
			name:    "command missing",
			output:  catalogCommandOutput{err: exec.ErrNotFound},
			want:    []string{"codex command not found", "/fake/codex"},
			notWant: "",
		},
		{
			name: "exit code and bounded stderr",
			output: catalogCommandOutput{
				stderr:   []byte(strings.Repeat("stderr-detail ", 100) + "secret-tail"),
				exitCode: 7,
				err:      errors.New("exit status 7"),
			},
			want:    []string{"exit code 7", "stderr-detail"},
			notWant: "secret-tail",
		},
		{
			name:   "invalid JSON",
			output: catalogCommandOutput{stdout: []byte("not-json")},
			want:   []string{"invalid JSON"},
		},
		{
			name:   "models is not an array",
			output: catalogCommandOutput{stdout: []byte(`{"models":{}}`)},
			want:   []string{"invalid JSON"},
		},
		{
			name:   "empty models",
			output: catalogCommandOutput{stdout: []byte(`{"models":[]}`)},
			want:   []string{"Codex bundled model catalog is empty: /fake/codex"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalogTestStubCommand(t, test.output, test.resolveErr)
			_, err := loadBundledNativeCatalog()
			if err == nil {
				t.Fatal("loadBundledNativeCatalog() error = nil, want error")
			}
			for _, substring := range test.want {
				if !strings.Contains(err.Error(), substring) {
					t.Fatalf("error %q does not contain %q", err.Error(), substring)
				}
			}
			if test.notWant != "" && strings.Contains(err.Error(), test.notWant) {
				t.Fatalf("error %q contains unbounded stderr", err.Error())
			}
		})
	}
}

func TestCatalogLoadBundledExecutesResolvedCodexCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	local := filepath.Join(home, ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	description := strings.Repeat("x", 128*1024)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"debug\" ] || [ \"$2\" != \"models\" ] || [ \"$3\" != \"--bundled\" ]; then\n" +
		"  echo unexpected-args >&2\n" +
		"  exit 9\n" +
		"fi\n" +
		"printf '%s' '{\"models\":[{\"slug\":\"native-large\",\"description\":\"" + description + "\"}]}'\n"
	if err := os.WriteFile(local, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	oldResolve := catalogResolveCodex
	oldExecute := catalogExecuteCodex
	t.Cleanup(func() {
		catalogResolveCodex = oldResolve
		catalogExecuteCodex = oldExecute
	})
	catalogResolveCodex = resolveCodexCommand
	catalogExecuteCodex = runCodexDebugModels

	models, err := loadBundledNativeCatalog()
	if err != nil {
		t.Fatalf("loadBundledNativeCatalog() error = %v", err)
	}
	if len(models) != 1 || models[0]["slug"] != "native-large" {
		t.Fatalf("models = %#v", models)
	}
	if got, ok := models[0]["description"].(string); !ok || len(got) != len(description) {
		t.Fatalf("large description length = %d, want %d", len(got), len(description))
	}
}

func TestRefreshCatalogWritesNativeAndWopalModels(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "nested", "codex")
	t.Setenv("CODEX_HOME", codexHome)
	catalogTestStubCommand(t, catalogCommandOutput{
		stdout: []byte(`{"models":[
			{"slug":"native-hidden","visibility":"hide"},
			{"slug":"native-visible","visibility":"list"}
		]}`),
	}, nil)

	cfg := &GatewayConfig{WopalAI: wopalAIConfig{Models: []ExternalModelConfig{
		{Name: "first"},
		{Name: "second"},
	}}}
	result, err := refreshCatalog(cfg)
	if err != nil {
		t.Fatalf("refreshCatalog() error = %v", err)
	}
	if result.Native != 2 || result.NativeVisible != 1 || result.NativeHidden != 1 || result.WopalAI != 2 {
		t.Fatalf("refresh result = %#v", result)
	}

	path := filepath.Join(codexHome, "wopal-codex-gateway-models.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("catalog file missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("catalog mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 4 {
		t.Fatalf("written models = %d, want 4", len(payload.Models))
	}
	if payload.Models[0]["slug"] != "wopal-ai/first" || payload.Models[1]["slug"] != "wopal-ai/second" {
		t.Fatalf("Wopal entries are not first: %#v", payload.Models[:2])
	}
	if payload.Models[2]["slug"] != "native-hidden" || payload.Models[2]["supported_in_api"] != false {
		t.Fatalf("native hidden entry = %#v", payload.Models[2])
	}
}

func TestCatalogResolveCodexCommand(t *testing.T) {
	t.Run("prefers executable local command", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())
		local := filepath.Join(home, ".local", "bin", "codex")
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveCodexCommand()
		if err != nil || got != local {
			t.Fatalf("resolveCodexCommand() = %q, %v; want %q", got, err, local)
		}
	})

	t.Run("falls back to PATH when local is absent", func(t *testing.T) {
		home := t.TempDir()
		binDir := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", binDir)
		pathCommand := filepath.Join(binDir, "codex")
		if err := os.WriteFile(pathCommand, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveCodexCommand()
		if err != nil || got != pathCommand {
			t.Fatalf("resolveCodexCommand() = %q, %v; want %q", got, err, pathCommand)
		}
	})

	t.Run("reports attempted paths when no command is executable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())
		local := filepath.Join(home, ".local", "bin", "codex")
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("not executable"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := resolveCodexCommand()
		if err == nil {
			t.Fatal("resolveCodexCommand() error = nil, want error")
		}
		if !strings.Contains(err.Error(), "codex command not found") || !strings.Contains(err.Error(), local) {
			t.Fatalf("error = %q, want missing command and %q", err.Error(), local)
		}
	})
}

func catalogTestStubCommand(t *testing.T, output catalogCommandOutput, resolveErr error) {
	t.Helper()
	oldResolve := catalogResolveCodex
	oldExecute := catalogExecuteCodex
	t.Cleanup(func() {
		catalogResolveCodex = oldResolve
		catalogExecuteCodex = oldExecute
	})
	catalogResolveCodex = func() (string, error) {
		if resolveErr != nil {
			return "", resolveErr
		}
		return "/fake/codex", nil
	}
	catalogExecuteCodex = func(string) catalogCommandOutput { return output }
}

// TestRefreshCatalogKeepsPreviousCatalogWhenCodexFails protects AC#5: a failed
// refresh must report the cause and leave the last usable catalog in place.
func TestRefreshCatalogKeepsPreviousCatalogWhenCodexFails(t *testing.T) {
	oldResolve := catalogResolveCodex
	oldExecute := catalogExecuteCodex
	t.Cleanup(func() {
		catalogResolveCodex = oldResolve
		catalogExecuteCodex = oldExecute
	})

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := codexGatewayCatalogPath()
	previous := []byte("{\"models\":[{\"slug\":\"wopal-ai/previous\"}]}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"

	for name, setup := range map[string]func(){
		"missing command": func() {
			catalogResolveCodex = func() (string, error) {
				return "", errors.New("codex command not found")
			}
		},
		"invalid output": func() {
			catalogResolveCodex = func() (string, error) { return "/usr/bin/codex", nil }
			catalogExecuteCodex = func(string) catalogCommandOutput {
				return catalogCommandOutput{stdout: []byte("not json")}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup()
			if _, err := refreshCatalog(cfg); err == nil {
				t.Fatal("refresh succeeded although Codex failed")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(previous) {
				t.Fatalf("previous catalog was overwritten: %s", got)
			}
		})
	}
}
