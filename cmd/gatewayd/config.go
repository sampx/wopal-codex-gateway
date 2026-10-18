package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const localAccessKey = "wopal-codex-gateway-local"

type externalModelConfig struct {
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
	Vision        bool   `json:"vision"`
	Reasoning     bool   `json:"reasoning"`
}

type gatewayConfig struct {
	Listen struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"listen"`
	WopalAI struct {
		BaseURL string                `json:"baseUrl"`
		Models  []externalModelConfig `json:"models"`
	} `json:"wopalAi"`
	Ollama struct {
		Enabled bool `json:"enabled"`
	} `json:"ollama"`
	Notifications notificationsConfig `json:"notifications"`
}

func loadGatewayConfig(path string) (*gatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway config: %w", err)
	}
	var cfg gatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse gateway config: %w", err)
	}
	if strings.TrimSpace(cfg.Listen.Host) == "" {
		cfg.Listen.Host = "127.0.0.1"
	}
	if cfg.Listen.Port == 0 {
		cfg.Listen.Port = 11435
	}
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return nil, fmt.Errorf("wopalAi.baseUrl is required")
	}
	if cfg.Ollama.Enabled {
		return nil, fmt.Errorf("Ollama integration must remain disabled for this gateway build")
	}
	return &cfg, nil
}

func loadWopalAPIKey() (string, error) {
	cmd := exec.Command("/usr/bin/security", "find-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-w")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read Wopal API key from macOS Keychain: %w", err)
	}
	key := strings.TrimSpace(string(output))
	if key == "" {
		return "", fmt.Errorf("Wopal API key in macOS Keychain is empty")
	}
	return key, nil
}

func buildCLIProxyConfig(root string, gateway *gatewayConfig, wopalKey string) (*sdkconfig.Config, string, error) {
	models := make([]map[string]any, 0, len(gateway.WopalAI.Models))
	for _, model := range gateway.WopalAI.Models {
		entry := map[string]any{
			"name":               model.Name,
			"alias":              "wopal-ai/" + model.Name,
			"display-name":       "Wopal AI / " + model.Name,
			"max-context-length": model.ContextWindow,
			"force-mapping":      true,
			"input-modalities":   []string{"text"},
		}
		if model.ContextWindow <= 0 {
			entry["max-context-length"] = 262144
		}
		if model.Vision {
			entry["input-modalities"] = []string{"text", "image"}
		}
		models = append(models, entry)
	}

	runtimeDir := filepath.Join(root, "runtime")
	configPath := filepath.Join(runtimeDir, "cliproxy-runtime.json")
	authDir := filepath.Join(runtimeDir, "cliproxy-auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create CLIProxyAPI auth dir: %w", err)
	}

	requestRetry := 0
	raw := map[string]any{
		"host":                     gateway.Listen.Host,
		"port":                     gateway.Listen.Port,
		"api-keys":                 []string{localAccessKey},
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
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
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
	return cfg, configPath, nil
}
