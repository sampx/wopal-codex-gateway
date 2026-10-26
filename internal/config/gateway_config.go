package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	DefaultListenHost     = "127.0.0.1"
	DefaultListenPort     = 11435
	DefaultContextWindow  = 1000000
	DefaultToolMode       = "code_mode_only"
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

	ProtocolOpenAIChat        = "openai-chat"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
	ProtocolGemini            = "gemini"
)

var providerIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
var providerHeaderNamePattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

var reservedProviderHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"x-api-key":           {},
	"x-goog-api-key":      {},
	"chatgpt-account-id":  {},
}

// ExternalModelConfig is one Wopal AI model entry in runtime/config.json. The
// optional fields stay pointers to distinguish user overrides (including false)
// from capabilities that should resolve through provider metadata and defaults.
type ExternalModelConfig struct {
	Name          string `json:"name"`
	Alias         string `json:"alias,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	ContextWindow *int   `json:"contextWindow,omitempty"`
	Vision        *bool  `json:"vision,omitempty"`
	Reasoning     *bool  `json:"reasoning,omitempty"`

	// extensions holds model fields this binary does not understand, so a
	// load/edit/save cycle preserves them. Clone reaches it from catalog.
	extensions extensionFields
}

func (m *ExternalModelConfig) UnmarshalJSON(data []byte) error {
	type plain ExternalModelConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "name", "alias", "protocol", "contextWindow", "vision", "reasoning")
	if err != nil {
		return err
	}
	*m = ExternalModelConfig(decoded)
	m.extensions = extensions
	return nil
}

func (m ExternalModelConfig) MarshalJSON() ([]byte, error) {
	type plain ExternalModelConfig
	return marshalWithExtensions(plain(m), m.extensions)
}

// Clone copies every field, including extensions, without sharing raw values.
func (m ExternalModelConfig) Clone() ExternalModelConfig {
	clone := m
	if m.ContextWindow != nil {
		contextWindow := *m.ContextWindow
		clone.ContextWindow = &contextWindow
	}
	if m.Vision != nil {
		vision := *m.Vision
		clone.Vision = &vision
	}
	if m.Reasoning != nil {
		reasoning := *m.Reasoning
		clone.Reasoning = &reasoning
	}
	clone.extensions = m.extensions.Clone()
	return clone
}

func (m ExternalModelConfig) EffectiveContextWindow() int {
	if m.ContextWindow == nil || *m.ContextWindow <= 0 {
		return DefaultContextWindow
	}
	return *m.ContextWindow
}

func (m ExternalModelConfig) SupportsVision() bool {
	return m.Vision == nil || *m.Vision
}

// SupportsReasoning defaults to true: a model without an explicit flag keeps
// offering the reasoning levels Codex expects.
func (m ExternalModelConfig) SupportsReasoning() bool {
	return m.Reasoning == nil || *m.Reasoning
}

// ProviderConfig is one user-configured external provider. `official` and
// `wopal-ai` are built-ins and never live in this map.
type ProviderConfig struct {
	BaseURL        string                `json:"baseUrl"`
	BaseURLs       map[string]string     `json:"baseUrls,omitempty"`
	ModelsURL      string                `json:"modelsUrl,omitempty"`
	ModelsAuth     string                `json:"modelsAuth,omitempty"`
	Headers        map[string]string     `json:"headers,omitempty"`
	Protocol       string                `json:"protocol,omitempty"`
	ToolMode       string                `json:"toolMode,omitempty"`
	ExcludedModels []string              `json:"excludedModels,omitempty"`
	Models         []ExternalModelConfig `json:"models,omitempty"`

	extensions extensionFields
}

func (c *ProviderConfig) UnmarshalJSON(data []byte) error {
	type plain ProviderConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "baseUrl", "baseUrls", "modelsUrl", "modelsAuth", "headers", "protocol", "toolMode", "excludedModels", "models")
	if err != nil {
		return err
	}
	*c = ProviderConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c ProviderConfig) MarshalJSON() ([]byte, error) {
	type plain ProviderConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

func (c ProviderConfig) Clone() ProviderConfig {
	clone := c
	if c.BaseURLs != nil {
		clone.BaseURLs = make(map[string]string, len(c.BaseURLs))
		for key, value := range c.BaseURLs {
			clone.BaseURLs[key] = value
		}
	}
	if c.Headers != nil {
		clone.Headers = make(map[string]string, len(c.Headers))
		for key, value := range c.Headers {
			clone.Headers[key] = value
		}
	}
	if c.ExcludedModels != nil {
		clone.ExcludedModels = append([]string(nil), c.ExcludedModels...)
	}
	if c.Models != nil {
		clone.Models = make([]ExternalModelConfig, len(c.Models))
		for index, model := range c.Models {
			clone.Models[index] = model.Clone()
		}
	}
	clone.extensions = c.extensions.Clone()
	return clone
}

func (c ProviderConfig) EffectiveToolMode() string {
	if strings.TrimSpace(c.ToolMode) == "" {
		return DefaultToolMode
	}
	return c.ToolMode
}

func (c ProviderConfig) ValidateToolMode() error {
	switch c.EffectiveToolMode() {
	case "direct", "code_mode", "code_mode_only":
		return nil
	default:
		return fmt.Errorf("provider toolMode must be direct, code_mode or code_mode_only; got %q", c.ToolMode)
	}
}

func (c ProviderConfig) IsModelExcluded(name string) bool {
	name = strings.TrimSpace(name)
	for _, excluded := range c.ExcludedModels {
		if strings.TrimSpace(excluded) == name {
			return true
		}
	}
	return false
}

// DiscoveryEnabled reports whether this provider uses the OpenAI-style
// models endpoint. An explicit modelsUrl always opts in. Without one, native
// Gemini/Anthropic providers are discovery-disabled and rely on models add;
// OpenAI-style or unspecified providers keep the baseUrl + /models default.
func (c ProviderConfig) DiscoveryEnabled() bool {
	if strings.TrimSpace(c.ModelsURL) != "" {
		return true
	}
	switch c.Protocol {
	case ProtocolGemini, ProtocolAnthropicMessages:
		return false
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return true
	}
	if len(c.Models) == 0 {
		return true
	}
	for _, model := range c.Models {
		switch model.Protocol {
		case ProtocolGemini, ProtocolAnthropicMessages:
			continue
		default:
			return true
		}
	}
	return false
}

func (c ProviderConfig) EffectiveModelsURL() string {
	if explicit := strings.TrimSpace(c.ModelsURL); explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/") + "/models"
}

func (c ProviderConfig) EffectiveModelsAuth() string {
	if c.ModelsAuth == "none" || c.ModelsAuth == "provider-key" {
		return c.ModelsAuth
	}
	base, baseErr := url.Parse(strings.TrimSpace(c.BaseURL))
	models, modelsErr := url.Parse(c.EffectiveModelsURL())
	if baseErr == nil && modelsErr == nil && strings.EqualFold(base.Scheme, models.Scheme) && strings.EqualFold(base.Host, models.Host) {
		return "provider-key"
	}
	return "none"
}

func (c ProviderConfig) BaseURLForProtocol(protocol string) string {
	if value := strings.TrimSpace(c.BaseURLs[protocol]); value != "" {
		return strings.TrimRight(value, "/")
	}
	return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
}

func ValidateProviderID(id string) error {
	if id == "official" || id == "wopal-ai" {
		return fmt.Errorf("provider id %q is reserved", id)
	}
	if !providerIDPattern.MatchString(id) {
		return fmt.Errorf("provider id %q must be a lowercase slug using letters, digits and hyphens", id)
	}
	return nil
}

func ValidateProtocol(protocol string) error {
	if protocol == "" {
		return nil
	}
	switch protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGemini:
		return nil
	default:
		return fmt.Errorf("protocol %q must be %s, %s, %s or %s", protocol, ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGemini)
	}
}

func ValidateProviderHeaders(headers map[string]string) error {
	for name, value := range headers {
		trimmedName := strings.TrimSpace(name)
		if trimmedName == "" || trimmedName != name || !providerHeaderNamePattern.MatchString(trimmedName) {
			return fmt.Errorf("provider header name %q is invalid", name)
		}
		if _, reserved := reservedProviderHeaders[strings.ToLower(trimmedName)]; reserved {
			return fmt.Errorf("provider header %q is reserved for credentials or account routing", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("provider header %q value contains a newline", name)
		}
		if strings.Contains(value, "$") && value != "$CPA-SESSION-ID" {
			return fmt.Errorf("provider header %q may use only exact $CPA-SESSION-ID expansion", name)
		}
	}
	return nil
}

func ValidateHTTPURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return err
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must use http or https and include a host")
	}
	return nil
}

type ListenConfig struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`

	extensions extensionFields
}

func (c *ListenConfig) UnmarshalJSON(data []byte) error {
	type plain ListenConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "host", "port")
	if err != nil {
		return err
	}
	*c = ListenConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c ListenConfig) MarshalJSON() ([]byte, error) {
	type plain ListenConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

type WopalAIConfig struct {
	ToolMode       string                `json:"toolMode,omitempty"`
	BaseURL        string                `json:"baseUrl,omitempty"`
	Models         []ExternalModelConfig `json:"models,omitempty"`
	ExcludedModels []string              `json:"excludedModels,omitempty"`

	extensions extensionFields
}

func (c *WopalAIConfig) UnmarshalJSON(data []byte) error {
	type plain WopalAIConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "toolMode", "baseUrl", "models", "excludedModels")
	if err != nil {
		return err
	}
	*c = WopalAIConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c WopalAIConfig) MarshalJSON() ([]byte, error) {
	type plain WopalAIConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

// EffectiveToolMode keeps provider entries on the same default tool surface.
func (c WopalAIConfig) EffectiveToolMode() string {
	if c.ToolMode == "" {
		return DefaultToolMode
	}
	return c.ToolMode
}

// ValidateToolMode rejects typos before Codex silently falls back to direct tools.
func (c WopalAIConfig) ValidateToolMode() error {
	switch c.EffectiveToolMode() {
	case "direct", "code_mode", "code_mode_only":
		return nil
	default:
		return fmt.Errorf("wopalAi.toolMode must be direct, code_mode or code_mode_only; got %q", c.ToolMode)
	}
}

type Logs struct {
	MaxSizeMB  int `json:"maxSizeMb,omitempty"`
	MaxBackups int `json:"maxBackups,omitempty"`

	extensions extensionFields
}

func (c *Logs) UnmarshalJSON(data []byte) error {
	type plain Logs
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "maxSizeMb", "maxBackups")
	if err != nil {
		return err
	}
	*c = Logs(decoded)
	c.extensions = extensions
	return nil
}

func (c Logs) MarshalJSON() ([]byte, error) {
	type plain Logs
	return marshalWithExtensions(plain(c), c.extensions)
}

// QuotaConfig tunes the learned quota guard. An absent or out-of-range value
// falls back to DefaultQuotaMaxStepCap, so a malformed config never disables
// the protection floor.
type QuotaConfig struct {
	MaxStepCap float64 `json:"maxStepCap,omitempty"`

	extensions extensionFields
}

func (c *QuotaConfig) UnmarshalJSON(data []byte) error {
	type plain QuotaConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "maxStepCap")
	if err != nil {
		return err
	}
	*c = QuotaConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c QuotaConfig) MarshalJSON() ([]byte, error) {
	type plain QuotaConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

const (
	DefaultModelRoleProvider = "wopal-ai"
	DefaultModelRoleModel    = "deepseek-v4.1-flash"
)

type ModelRoleRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	extensions extensionFields
}

func (r *ModelRoleRef) UnmarshalJSON(data []byte) error {
	type plain ModelRoleRef
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "provider", "model")
	if err != nil {
		return err
	}
	*r = ModelRoleRef(decoded)
	r.extensions = extensions
	return nil
}

func (r ModelRoleRef) MarshalJSON() ([]byte, error) {
	type plain ModelRoleRef
	return marshalWithExtensions(plain(r), r.extensions)
}

func (r ModelRoleRef) Clone() ModelRoleRef {
	clone := r
	clone.extensions = r.extensions.Clone()
	return clone
}

type ModelRolesConfig struct {
	OfficialFallback *ModelRoleRef `json:"officialFallback,omitempty"`
	Compaction       *ModelRoleRef `json:"compaction,omitempty"`

	extensions extensionFields
}

func (c *ModelRolesConfig) UnmarshalJSON(data []byte) error {
	type plain ModelRolesConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "officialFallback", "compaction")
	if err != nil {
		return err
	}
	*c = ModelRolesConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c ModelRolesConfig) MarshalJSON() ([]byte, error) {
	type plain ModelRolesConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

func (c ModelRolesConfig) Clone() ModelRolesConfig {
	clone := c
	if c.OfficialFallback != nil {
		value := c.OfficialFallback.Clone()
		clone.OfficialFallback = &value
	}
	if c.Compaction != nil {
		value := c.Compaction.Clone()
		clone.Compaction = &value
	}
	clone.extensions = c.extensions.Clone()
	return clone
}

func (c ModelRolesConfig) ReferencedRole(providerID string) string {
	providerID = strings.TrimSpace(providerID)
	if c.OfficialFallback != nil && strings.TrimSpace(c.OfficialFallback.Provider) == providerID {
		return "officialFallback"
	}
	if c.Compaction != nil && strings.TrimSpace(c.Compaction.Provider) == providerID {
		return "compaction"
	}
	return ""
}

// GatewayConfig is the single in-memory form of runtime/config.json. The CLI
// and the daemon read and write it through the same parser so a value can never
// mean one thing in `config show` and another in `serve`.
type GatewayConfig struct {
	Version       int                       `json:"version,omitempty"`
	Listen        ListenConfig              `json:"listen"`
	WopalAI       WopalAIConfig             `json:"wopalAi"`
	Providers     map[string]ProviderConfig `json:"providers,omitempty"`
	ModelRoles    ModelRolesConfig          `json:"modelRoles,omitempty"`
	Notifications NotificationsConfig       `json:"notifications,omitempty"`
	Logs          Logs                      `json:"logs,omitempty"`
	Quota         QuotaConfig               `json:"quota,omitempty"`

	extensions extensionFields
}

func (c *GatewayConfig) UnmarshalJSON(data []byte) error {
	type plain GatewayConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "version", "listen", "wopalAi", "providers", "modelRoles", "notifications", "logs", "quota")
	if err != nil {
		return err
	}
	*c = GatewayConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c GatewayConfig) MarshalJSON() ([]byte, error) {
	type plain GatewayConfig
	return marshalWithExtensions(plain(c), c.extensions)
}

// NotificationsConfig is one notifications block in runtime/config.json. It
// lives beside the rest of the schema so this package imports no internal
// package of its own.
type NotificationsConfig struct {
	Enabled *bool  `json:"enabled"`
	Sound   string `json:"sound"`
	Tool    string `json:"tool"`

	extensions extensionFields
}

func (c *NotificationsConfig) UnmarshalJSON(data []byte) error {
	type plain NotificationsConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extensions, err := decodeExtensionFields(data, "enabled", "sound", "tool")
	if err != nil {
		return err
	}
	*c = NotificationsConfig(decoded)
	c.extensions = extensions
	return nil
}

func (c NotificationsConfig) MarshalJSON() ([]byte, error) {
	type plain NotificationsConfig
	return marshalWithExtensions(plain(c), c.extensions)
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
	return &GatewayConfig{
		Version: gatewayConfigVersion,
		Listen:  ListenConfig{Host: DefaultListenHost, Port: DefaultListenPort},
		WopalAI: WopalAIConfig{
			ToolMode:       DefaultToolMode,
			Models:         []ExternalModelConfig{{Name: defaultWopalModelName}},
			ExcludedModels: []string{},
		},
		Providers: map[string]ProviderConfig{},
		Logs:      Logs{MaxSizeMB: DefaultLogMaxSizeMB, MaxBackups: DefaultLogMaxBackups},
		Quota:     QuotaConfig{MaxStepCap: DefaultQuotaMaxStepCap},
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
	if err := saved.WopalAI.ValidateToolMode(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := saved.ValidateProviders(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return MergeGatewayConfig(&saved), nil
}

func (c *GatewayConfig) ValidateProviders() error {
	if c == nil {
		return fmt.Errorf("gateway config is nil")
	}
	for id, provider := range c.Providers {
		if err := ValidateProviderID(id); err != nil {
			return err
		}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q baseUrl is required", id)
		}
		if err := ValidateHTTPURL(provider.BaseURL); err != nil {
			return fmt.Errorf("provider %q baseUrl %w", id, err)
		}
		if err := ValidateProtocol(provider.Protocol); err != nil {
			return fmt.Errorf("provider %q %w", id, err)
		}
		if err := provider.ValidateToolMode(); err != nil {
			return fmt.Errorf("provider %q: %w", id, err)
		}
		for protocol, baseURL := range provider.BaseURLs {
			if err := ValidateProtocol(protocol); err != nil || protocol == "" {
				if err == nil {
					err = fmt.Errorf("protocol is required")
				}
				return fmt.Errorf("provider %q baseUrls: %w", id, err)
			}
			if err := ValidateHTTPURL(baseURL); err != nil {
				return fmt.Errorf("provider %q baseUrls[%q] %w", id, protocol, err)
			}
		}
		if provider.ModelsURL != "" {
			if err := ValidateHTTPURL(provider.ModelsURL); err != nil {
				return fmt.Errorf("provider %q modelsUrl %w", id, err)
			}
		}
		if provider.ModelsAuth != "" && provider.ModelsAuth != "none" && provider.ModelsAuth != "provider-key" {
			return fmt.Errorf("provider %q modelsAuth must be none or provider-key", id)
		}
		if err := ValidateProviderHeaders(provider.Headers); err != nil {
			return fmt.Errorf("provider %q: %w", id, err)
		}
		for _, model := range provider.Models {
			if strings.TrimSpace(model.Name) == "" {
				return fmt.Errorf("provider %q has a model with an empty name", id)
			}
			if err := ValidateProtocol(model.Protocol); err != nil {
				return fmt.Errorf("provider %q model %q %w", id, model.Name, err)
			}
			if model.Alias != "" && strings.TrimSpace(model.Alias) == "" {
				return fmt.Errorf("provider %q model %q alias cannot be whitespace", id, model.Name)
			}
		}
	}
	return nil
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
	if merged.Providers == nil {
		merged.Providers = map[string]ProviderConfig{}
	}
	merged.WopalAI.BaseURL = strings.TrimSpace(merged.WopalAI.BaseURL)
	merged.WopalAI.ToolMode = merged.WopalAI.EffectiveToolMode()
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
	if err := cfg.WopalAI.ValidateToolMode(); err != nil {
		return err
	}
	if err := cfg.ValidateProviders(); err != nil {
		return err
	}
	// Resolve the root before any side effect: an unresolved root must fail the
	// save instead of writing a runtime directory relative to cwd.
	root, err := ProjectRootDir()
	if err != nil {
		return fmt.Errorf("resolve gateway config path: %w", err)
	}
	// Every write stamps the current schema version, so a file this binary
	// produced can never be rejected by the next run.
	cfg.Version = gatewayConfigVersion
	path := ConfigPathFor(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}

	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode gateway config: %w", err)
	}
	if err := WriteFileAtomic(path, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write gateway config: %w", err)
	}
	return nil
}
