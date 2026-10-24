package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultListenHost     = "127.0.0.1"
	DefaultListenPort     = 11435
	DefaultContextWindow  = 262144
	defaultWopalModelName = "qwen3-coder"

	// gatewayConfigVersion is the only schema version this binary understands.
	// A file with a missing or different version is rejected before any side
	// effect, so an old or future schema never half-starts a service.
	gatewayConfigVersion = 1

	DefaultLogMaxSizeMB  = 10
	DefaultLogMaxBackups = 5

	// DefaultQuotaMaxStepCap is the default upper bound for one observed quota
	// sampling step. Operators may lower or raise it within [1, 10].
	DefaultQuotaMaxStepCap = 5
	minQuotaMaxStepCap     = 1
	maxQuotaMaxStepCap     = 10
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

func (m ExternalModelConfig) EffectiveContextWindow() int {
	if m.ContextWindow == nil || *m.ContextWindow <= 0 {
		return DefaultContextWindow
	}
	return *m.ContextWindow
}

func (m ExternalModelConfig) SupportsVision() bool {
	return m.Vision != nil && *m.Vision
}

// SupportsReasoning defaults to true: a model without an explicit flag keeps
// offering the reasoning levels Codex expects.
func (m ExternalModelConfig) SupportsReasoning() bool {
	return m.Reasoning == nil || *m.Reasoning
}

type ListenConfig struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
}

type WopalAIConfig struct {
	BaseURL        string                `json:"baseUrl,omitempty"`
	Models         []ExternalModelConfig `json:"models,omitempty"`
	ExcludedModels []string              `json:"excludedModels,omitempty"`
}

type Logs struct {
	MaxSizeMB  int `json:"maxSizeMb,omitempty"`
	MaxBackups int `json:"maxBackups,omitempty"`
}

// QuotaConfig tunes the learned quota guard. An absent or out-of-range value
// falls back to DefaultQuotaMaxStepCap, so a malformed config never disables
// the protection floor.
type QuotaConfig struct {
	MaxStepCap float64 `json:"maxStepCap,omitempty"`
}

// GatewayConfig is the single in-memory form of runtime/config.json. The CLI
// and the daemon read and write it through the same parser so a value can never
// mean one thing in `config show` and another in `serve`.
type GatewayConfig struct {
	Version       int                 `json:"version,omitempty"`
	Listen        ListenConfig        `json:"listen"`
	WopalAI       WopalAIConfig       `json:"wopalAi"`
	Notifications NotificationsConfig `json:"notifications,omitempty"`
	Logs          Logs                `json:"logs,omitempty"`
	Quota         QuotaConfig         `json:"quota,omitempty"`
}

// NotificationsConfig is one notifications block in runtime/config.json. It
// lives beside the rest of the schema so this package imports no internal
// package of its own.
type NotificationsConfig struct {
	Enabled *bool  `json:"enabled"`
	Sound   string `json:"sound"`
	Tool    string `json:"tool"`
}

func (c NotificationsConfig) NotificationsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// NormalizedSound maps the sentinel values onto "no sound flag"; anything else
// is passed to the notification tool as a macOS sound name.
func (c NotificationsConfig) NormalizedSound() string {
	sound := strings.TrimSpace(c.Sound)
	if sound == "" {
		return "default"
	}
	if strings.EqualFold(sound, "none") || strings.EqualFold(sound, "off") {
		return ""
	}
	return sound
}

func DefaultGatewayConfig() *GatewayConfig {
	ContextWindow := DefaultContextWindow
	reasoning := true
	return &GatewayConfig{
		Version: gatewayConfigVersion,
		Listen:  ListenConfig{Host: DefaultListenHost, Port: DefaultListenPort},
		WopalAI: WopalAIConfig{
			Models: []ExternalModelConfig{{
				Name:          defaultWopalModelName,
				ContextWindow: &ContextWindow,
				Reasoning:     &reasoning,
			}},
			ExcludedModels: []string{},
		},
		Logs:  Logs{MaxSizeMB: DefaultLogMaxSizeMB, MaxBackups: DefaultLogMaxBackups},
		Quota: QuotaConfig{MaxStepCap: DefaultQuotaMaxStepCap},
	}
}

// LoadGatewayConfig reads runtime/config.json. A missing file yields the default
// configuration so first-run commands work, and every other read or parse error
// is reported instead of being treated as an empty configuration.
func LoadGatewayConfig(path string) (*GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultGatewayConfig(), nil
		}
		return nil, fmt.Errorf("read gateway config: %w", err)
	}

	var saved GatewayConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("parse gateway config %s: %w", path, err)
	}
	if saved.Version != gatewayConfigVersion {
		return nil, fmt.Errorf("%s: unsupported gateway config version %d (expected %d)", path, saved.Version, gatewayConfigVersion)
	}
	if err := rejectRemovedSections(data, path); err != nil {
		return nil, err
	}
	return MergeGatewayConfig(&saved), nil
}

// removedConfigSection names a section that earlier configuration versions
// carried and that version 1 no longer supports.
type removedConfigSection struct {
	key  string
	hint string
}

// removedConfigSections is the single place that knows which legacy sections
// were dropped. Keeping it explicit means a stale config fails loudly with a
// fix instead of being silently ignored, and no removed provider survives as a
// live field that `config show` or a save would re-emit.
var removedConfigSections = []removedConfigSection{
	{key: "ollama", hint: "Ollama support was removed; delete the \"ollama\" block"},
}

func rejectRemovedSections(data []byte, path string) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil // the typed unmarshal above already reported the parse error
	}
	for _, section := range removedConfigSections {
		if _, present := probe[section.key]; present {
			return fmt.Errorf("%s: unsupported configuration section %q. %s", path, section.key, section.hint)
		}
	}
	return nil
}

// MergeGatewayConfig applies defaults to a parsed configuration while keeping
// every section the CLI does not manage intact across a load/save round trip.
func MergeGatewayConfig(saved *GatewayConfig) *GatewayConfig {
	defaults := DefaultGatewayConfig()
	merged := *saved

	if strings.TrimSpace(merged.Listen.Host) == "" {
		merged.Listen.Host = defaults.Listen.Host
	}
	if merged.Listen.Port == 0 {
		merged.Listen.Port = defaults.Listen.Port
	}
	merged.Version = gatewayConfigVersion
	merged.WopalAI.BaseURL = strings.TrimSpace(merged.WopalAI.BaseURL)
	if merged.WopalAI.Models == nil {
		merged.WopalAI.Models = defaults.WopalAI.Models
	}
	if merged.WopalAI.ExcludedModels == nil {
		merged.WopalAI.ExcludedModels = []string{}
	}
	if merged.Logs.MaxSizeMB <= 0 {
		merged.Logs.MaxSizeMB = DefaultLogMaxSizeMB
	}
	if merged.Logs.MaxBackups <= 0 {
		merged.Logs.MaxBackups = DefaultLogMaxBackups
	}
	merged.Quota.MaxStepCap = normalizeQuotaMaxStepCap(merged.Quota.MaxStepCap)
	return &merged
}

func normalizeQuotaMaxStepCap(value float64) float64 {
	if value < minQuotaMaxStepCap || value > maxQuotaMaxStepCap {
		return DefaultQuotaMaxStepCap
	}
	return value
}

// ValidateServeConfig rejects configurations the daemon cannot serve. It runs
// before any side effect so a bad file never leaves a half-started service.
func ValidateServeConfig(cfg *GatewayConfig) error {
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return fmt.Errorf("wopalAi.baseUrl is required")
	}
	return nil
}

// LoadInstallConfig validates the configuration an install depends on before
// any filesystem or launchd side effect.
func LoadInstallConfig() (*GatewayConfig, error) {
	cfg, err := LoadGatewayConfig(ConfigPath())
	if err != nil {
		return nil, err
	}
	if err := ValidateServeConfig(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	return cfg, nil
}

// SaveGatewayConfig writes the configuration atomically with 0600 permissions.
func SaveGatewayConfig(cfg *GatewayConfig) error {
	// Every write stamps the current schema version, so a file this binary
	// produced can never be rejected by the next run.
	cfg.Version = gatewayConfigVersion
	path := ConfigPath()
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
