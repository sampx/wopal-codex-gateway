package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const LocalAccessKey = "wopal-codex-gateway-local"

// BuildCLIProxyConfig turns the gateway configuration into the embedded
// CLIProxyAPI config. The Wopal API key is injected into the parsed struct only,
// so the file written next to the config never contains the credential.
func BuildCLIProxyConfig(root string, gateway *GatewayConfig, wopalKey string) (*sdkconfig.Config, string, error) {
	if gateway == nil {
		return nil, "", fmt.Errorf("build CLIProxyAPI config: gateway config is nil")
	}
	root, err := requireRoot(root)
	if err != nil {
		return nil, "", err
	}
	models := make([]map[string]any, 0, len(gateway.WopalAI.Models))
	for _, model := range gateway.WopalAI.Models {
		entry := map[string]any{
			"name":               model.Name,
			"alias":              "wopal-ai/" + model.Name,
			"display-name":       "Wopal AI / " + model.Name,
			"max-context-length": model.EffectiveContextWindow(),
			"force-mapping":      true,
			"input-modalities":   []string{"text"},
		}
		if model.SupportsVision() {
			entry["input-modalities"] = []string{"text", "image"}
		}
		models = append(models, entry)
	}

	runtimeConfigPath := CLIProxyRuntimeConfigPathFor(root)
	authDir := CLIProxyAuthDirFor(root)
	if err := os.MkdirAll(StateDirFor(root), 0o700); err != nil {
		return nil, "", fmt.Errorf("create CLIProxyAPI runtime dir: %w", err)
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create CLIProxyAPI auth dir: %w", err)
	}

	requestRetry := 0
	raw := map[string]any{
		"host":                     gateway.Listen.Host,
		"port":                     gateway.Listen.Port,
		"api-keys":                 []string{LocalAccessKey},
		"auth-dir":                 authDir,
		"logging-to-file":          false,
		"usage-statistics-enabled": false,
		"remote-management": map[string]any{
			"allow-remote":          false,
			"disable-control-panel": true,
		},
		"openai-compatibility": []map[string]any{{
			"name":          "wopal-ai",
			"base-url":      strings.TrimRight(gateway.WopalAI.BaseURL, "/"),
			"request-retry": requestRetry,
			"api-key-entries": []map[string]any{{
				"api-key": "__KEYCHAIN__",
			}},
			"models": models,
		}},
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("encode CLIProxyAPI config: %w", err)
	}
	if err := WriteFileAtomic(runtimeConfigPath, append(data, '\n'), 0o600); err != nil {
		return nil, "", fmt.Errorf("write sanitized CLIProxyAPI config: %w", err)
	}
	cfg, err := sdkconfig.ParseConfigBytes(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse CLIProxyAPI config: %w", err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 1 {
		return nil, "", fmt.Errorf("unexpected CLIProxyAPI OpenAI compatibility config shape")
	}
	cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey = wopalKey
	return cfg, runtimeConfigPath, nil
}

// requireRoot rejects an empty explicit root before any write. A relative root
// stays supported and is normalized against cwd, matching --project-root.
func requireRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("project root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	return abs, nil
}
