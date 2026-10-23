package config

import (
	"os"
	"path/filepath"
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
