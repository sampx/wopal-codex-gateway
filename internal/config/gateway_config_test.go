package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeGatewayConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGatewayConfigRejectsMissingVersion(t *testing.T) {
	path := writeGatewayConfigFile(t, `{"listen":{"host":"127.0.0.1","port":11435},"wopalAi":{"baseUrl":"https://example.test/v1"}}`)
	_, err := LoadGatewayConfig(path)
	if err == nil {
		t.Fatal("config without a version was accepted")
	}
	for _, want := range []string{"version", "expected 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestGatewayConfigRejectsUnknownVersion(t *testing.T) {
	path := writeGatewayConfigFile(t, `{"version":2,"wopalAi":{"baseUrl":"https://example.test/v1"}}`)
	_, err := LoadGatewayConfig(path)
	if err == nil {
		t.Fatal("config with version 2 was accepted")
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("error does not report the version: %v", err)
	}
}

func TestGatewayConfigRejectsWrongFieldType(t *testing.T) {
	path := writeGatewayConfigFile(t, `{"version":1,"listen":{"port":"eleven"},"wopalAi":{"baseUrl":"https://example.test/v1"}}`)
	_, err := LoadGatewayConfig(path)
	if err == nil {
		t.Fatal("config with a string port was accepted")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("error does not name the offending section: %v", err)
	}
}

func TestGatewayConfigDefaultsAndLogs(t *testing.T) {
	path := writeGatewayConfigFile(t, `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"logs":{"maxSizeMb":20}}`)
	cfg, err := LoadGatewayConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.Host != DefaultListenHost || cfg.Listen.Port != DefaultListenPort {
		t.Fatalf("listen defaults = %+v", cfg.Listen)
	}
	if cfg.Logs.MaxSizeMB != 20 {
		t.Fatalf("maxSizeMb = %d, want the configured 20", cfg.Logs.MaxSizeMB)
	}
	if cfg.Logs.MaxBackups != DefaultLogMaxBackups {
		t.Fatalf("maxBackups = %d, want the default %d", cfg.Logs.MaxBackups, DefaultLogMaxBackups)
	}
}

func TestGatewayConfigQuotaMaxStepCap(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{
			name: "configured",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"quota":{"maxStepCap":3}}`,
			want: 3,
		},
		{
			name: "missing",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"}}`,
			want: DefaultQuotaMaxStepCap,
		},
		{
			name: "zero",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"quota":{"maxStepCap":0}}`,
			want: DefaultQuotaMaxStepCap,
		},
		{
			name: "above range",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"quota":{"maxStepCap":11}}`,
			want: DefaultQuotaMaxStepCap,
		},
		{
			name: "below range",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"quota":{"maxStepCap":0.5}}`,
			want: DefaultQuotaMaxStepCap,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg, err := LoadGatewayConfig(writeGatewayConfigFile(t, testCase.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Quota.MaxStepCap != testCase.want {
				t.Fatalf("maxStepCap = %v, want %v", cfg.Quota.MaxStepCap, testCase.want)
			}
		})
	}
}

func TestGatewayConfigMissingFileYieldsVersionedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	cfg, err := LoadGatewayConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != gatewayConfigVersion {
		t.Fatalf("default version = %d, want %d", cfg.Version, gatewayConfigVersion)
	}
	if cfg.Logs.MaxSizeMB != DefaultLogMaxSizeMB || cfg.Logs.MaxBackups != DefaultLogMaxBackups {
		t.Fatalf("log defaults = %+v", cfg.Logs)
	}
}

func TestSaveGatewayConfigStampsVersion(t *testing.T) {
	root := useTempGatewayRoot(t)
	cfg := &GatewayConfig{WopalAI: WopalAIConfig{BaseURL: "https://example.test/v1"}}
	if err := SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(filepath.Join(root, "runtime", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"version": 1`) {
		t.Fatalf("saved config has no version: %s", saved)
	}

	// A write followed by a read must round-trip through the same parser.
	reloaded, err := LoadGatewayConfig(filepath.Join(root, "runtime", "config.json"))
	if err != nil {
		t.Fatalf("reloading a config this binary wrote failed: %v", err)
	}
	if reloaded.WopalAI.BaseURL != "https://example.test/v1" {
		t.Fatalf("round trip changed the base URL: %q", reloaded.WopalAI.BaseURL)
	}
}

func TestLoadInstallConfigRequiresVersion(t *testing.T) {
	root := useTempGatewayRoot(t)
	path := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"wopalAi":{"baseUrl":"https://example.test/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadInstallConfig()
	if err == nil {
		t.Fatal("install accepted a config without a version")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the config path: %v", err)
	}
}

// TestGatewayConfigRejectsLegacyOllamaSection covers W-01: Ollama is not part of
// the version-1 schema. A stale block must fail loudly with a fix, instead of
// being accepted into the canonical struct and re-emitted by `config show`.
func TestGatewayConfigRejectsLegacyOllamaSection(t *testing.T) {
	body := `{"version":1,"wopalAi":{"baseUrl":"https://example.test/v1"},"ollama":{"enabled":true,"baseUrl":"http://127.0.0.1:11434"}}`
	_, err := LoadGatewayConfig(writeGatewayConfigFile(t, body))
	if err == nil {
		t.Fatal("a version-1 config with an ollama block was accepted")
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Fatalf("error = %v, want it to name the removed section", err)
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Fatalf("error = %v, want it to say how to fix the file", err)
	}
}

// TestGatewayConfigShapeHasNoOllamaField covers the other half of W-01: the
// canonical type must not carry the removed provider, so neither `config show`
// nor a save can emit it.
func TestGatewayConfigShapeHasNoOllamaField(t *testing.T) {
	cfg := DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "ollama") {
		t.Fatalf("canonical config still serializes an ollama field: %s", payload)
	}
	if _, ok := reflect.TypeOf(*cfg).FieldByName("Ollama"); ok {
		t.Fatal("GatewayConfig still declares an Olama field")
	}
}

func TestGatewayConfigLoadsExternalProvidersAndPreservesExtensions(t *testing.T) {
	useTempGatewayRoot(t)
	body := `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"opencode-go":{"baseUrl":"https://opencode.example/openai/v1","baseUrls":{"gemini":"https://opencode.example/google"},"modelsUrl":"https://opencode.example/models","modelsAuth":"none","headers":{"x-opencode-session":"literal"},"protocol":"openai-responses","models":[{"name":"gpt-x","alias":"fast","protocol":"openai-responses"}],"future":{"keep":true}}},"futureRoot":{"keep":true}}`
	cfg, err := LoadGatewayConfig(writeGatewayConfigFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["opencode-go"]
	if !ok {
		t.Fatalf("providers = %#v, want opencode-go", cfg.Providers)
	}
	if p.BaseURL != "https://opencode.example/openai/v1" || p.BaseURLs["gemini"] != "https://opencode.example/google" {
		t.Fatalf("provider URLs = %+v", p)
	}
	if p.ModelsURL != "https://opencode.example/models" || p.ModelsAuth != "none" || p.Protocol != ProtocolOpenAIResponses {
		t.Fatalf("provider discovery/protocol = %+v", p)
	}
	if len(p.Models) != 1 || p.Models[0].Alias != "fast" || p.Models[0].Protocol != ProtocolOpenAIResponses {
		t.Fatalf("provider models = %#v", p.Models)
	}
	if err := SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(saved, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["futureRoot"]; !ok {
		t.Fatalf("top-level extension lost: %s", saved)
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(root["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	providerFields := decodeObject(t, providers["opencode-go"], "provider")
	if _, ok := providerFields["future"]; !ok {
		t.Fatalf("provider extension lost: %s", providers["opencode-go"])
	}
}

func TestGatewayConfigRejectsInvalidProviderIdentityAndProtocol(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "reserved provider",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"official":{"baseUrl":"https://example.test"}}}`,
			want: "reserved",
		},
		{
			name: "uppercase provider",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"OpenCode":{"baseUrl":"https://example.test"}}}`,
			want: "provider id",
		},
		{
			name: "invalid provider protocol",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"opencode":{"baseUrl":"https://example.test","protocol":"bogus"}}}`,
			want: "protocol",
		},
		{
			name: "invalid model protocol",
			body: `{"version":1,"wopalAi":{"baseUrl":"https://wopal.example/v1"},"providers":{"opencode":{"baseUrl":"https://example.test","models":[{"name":"m","protocol":"bogus"}]}}}`,
			want: "protocol",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := LoadGatewayConfig(writeGatewayConfigFile(t, testCase.body))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(testCase.want)) {
				t.Fatalf("LoadGatewayConfig() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestProviderDiscoveryDefaults(t *testing.T) {
	tests := []struct {
		name        string
		provider    ProviderConfig
		wantEnabled bool
		wantURL     string
		wantAuth    string
	}{
		{
			name:        "same origin openai style",
			provider:    ProviderConfig{BaseURL: "https://api.example.test/v1"},
			wantEnabled: true,
			wantURL:     "https://api.example.test/v1/models",
			wantAuth:    "provider-key",
		},
		{
			name:        "cross origin models endpoint",
			provider:    ProviderConfig{BaseURL: "https://api.example.test/v1", ModelsURL: "https://catalog.example.test/models"},
			wantEnabled: true,
			wantURL:     "https://catalog.example.test/models",
			wantAuth:    "none",
		},
		{
			name:        "explicit auth wins",
			provider:    ProviderConfig{BaseURL: "https://api.example.test/v1", ModelsURL: "https://catalog.example.test/models", ModelsAuth: "provider-key"},
			wantEnabled: true,
			wantURL:     "https://catalog.example.test/models",
			wantAuth:    "provider-key",
		},
		{
			name:        "native gemini disabled",
			provider:    ProviderConfig{BaseURL: "https://generativelanguage.googleapis.com", Protocol: ProtocolGemini},
			wantEnabled: false,
			wantURL:     "https://generativelanguage.googleapis.com/models",
			wantAuth:    "provider-key",
		},
		{
			name:        "native anthropic inferred from models",
			provider:    ProviderConfig{BaseURL: "https://api.anthropic.com", Models: []ExternalModelConfig{{Name: "claude", Protocol: ProtocolAnthropicMessages}}},
			wantEnabled: false,
			wantURL:     "https://api.anthropic.com/models",
			wantAuth:    "provider-key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.provider.DiscoveryEnabled(); got != tc.wantEnabled {
				t.Fatalf("DiscoveryEnabled=%v want %v", got, tc.wantEnabled)
			}
			if got := tc.provider.EffectiveModelsURL(); got != tc.wantURL {
				t.Fatalf("EffectiveModelsURL=%q want %q", got, tc.wantURL)
			}
			if got := tc.provider.EffectiveModelsAuth(); got != tc.wantAuth {
				t.Fatalf("EffectiveModelsAuth=%q want %q", got, tc.wantAuth)
			}
		})
	}
}
