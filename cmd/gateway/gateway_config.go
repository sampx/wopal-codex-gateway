package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultListenHost     = "127.0.0.1"
	defaultListenPort     = 11435
	defaultContextWindow  = 262144
	defaultWopalModelName = "qwen3-coder"
)

// ExternalModelConfig is one Wopal AI model entry in runtime/config.json. The
// optional fields stay pointers so an entry that omits them keeps Codex's
// defaults instead of silently claiming vision or no reasoning.
type ExternalModelConfig struct {
	Name          string `json:"name"`
	ContextWindow *int   `json:"contextWindow,omitempty"`
	Vision        *bool  `json:"vision,omitempty"`
	Reasoning     *bool  `json:"reasoning,omitempty"`
}

func (m ExternalModelConfig) contextWindow() int {
	if m.ContextWindow == nil || *m.ContextWindow <= 0 {
		return defaultContextWindow
	}
	return *m.ContextWindow
}

func (m ExternalModelConfig) supportsVision() bool {
	return m.Vision != nil && *m.Vision
}

// supportsReasoning defaults to true: a model without an explicit flag keeps
// offering the reasoning levels Codex expects.
func (m ExternalModelConfig) supportsReasoning() bool {
	return m.Reasoning == nil || *m.Reasoning
}

type listenConfig struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
}

type wopalAIConfig struct {
	BaseURL        string                `json:"baseUrl,omitempty"`
	Models         []ExternalModelConfig `json:"models,omitempty"`
	ExcludedModels []string              `json:"excludedModels,omitempty"`
}

type logsConfig struct {
	MaxSizeMB  int `json:"maxSizeMb,omitempty"`
	MaxBackups int `json:"maxBackups,omitempty"`
}

type ollamaConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
}

// GatewayConfig is the single in-memory form of runtime/config.json. The CLI
// and the daemon read and write it through the same parser so a value can never
// mean one thing in `config show` and another in `serve`.
type GatewayConfig struct {
	Version       int                 `json:"version,omitempty"`
	Listen        listenConfig        `json:"listen"`
	WopalAI       wopalAIConfig       `json:"wopalAi"`
	Notifications notificationsConfig `json:"notifications,omitempty"`
	Logs          logsConfig          `json:"logs,omitempty"`
	Ollama        ollamaConfig        `json:"ollama,omitempty"`
}

func defaultGatewayConfig() *GatewayConfig {
	contextWindow := defaultContextWindow
	reasoning := true
	return &GatewayConfig{
		Listen: listenConfig{Host: defaultListenHost, Port: defaultListenPort},
		WopalAI: wopalAIConfig{
			Models: []ExternalModelConfig{{
				Name:          defaultWopalModelName,
				ContextWindow: &contextWindow,
				Reasoning:     &reasoning,
			}},
			ExcludedModels: []string{},
		},
	}
}

// loadGatewayConfig reads runtime/config.json. A missing file yields the default
// configuration so first-run commands work, and every other read or parse error
// is reported instead of being treated as an empty configuration.
func loadGatewayConfig(path string) (*GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultGatewayConfig(), nil
		}
		return nil, fmt.Errorf("read gateway config: %w", err)
	}

	var saved GatewayConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("parse gateway config %s: %w", path, err)
	}
	return mergeGatewayConfig(&saved), nil
}

// mergeGatewayConfig applies defaults to a parsed configuration while keeping
// every section the CLI does not manage intact across a load/save round trip.
func mergeGatewayConfig(saved *GatewayConfig) *GatewayConfig {
	defaults := defaultGatewayConfig()
	merged := *saved

	if strings.TrimSpace(merged.Listen.Host) == "" {
		merged.Listen.Host = defaults.Listen.Host
	}
	if merged.Listen.Port == 0 {
		merged.Listen.Port = defaults.Listen.Port
	}
	merged.WopalAI.BaseURL = strings.TrimSpace(merged.WopalAI.BaseURL)
	if merged.WopalAI.Models == nil {
		merged.WopalAI.Models = defaults.WopalAI.Models
	}
	if merged.WopalAI.ExcludedModels == nil {
		merged.WopalAI.ExcludedModels = []string{}
	}
	return &merged
}

// validateServeConfig rejects configurations the daemon cannot serve. It runs
// before any side effect so a bad file never leaves a half-started service.
func validateServeConfig(cfg *GatewayConfig) error {
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return fmt.Errorf("wopalAi.baseUrl is required")
	}
	if cfg.Ollama.Enabled {
		return fmt.Errorf("Ollama integration must remain disabled for this gateway build")
	}
	return nil
}

// saveGatewayConfig writes the configuration atomically with 0600 permissions.
func saveGatewayConfig(cfg *GatewayConfig) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}

	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode gateway config: %w", err)
	}

	temp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(temp, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write gateway config: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("replace gateway config: %w", err)
	}
	return nil
}
