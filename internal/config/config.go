package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const LocalAccessKey = "wopal-codex-gateway-local"

// BuildCLIProxyConfig turns the gateway configuration into the embedded
// CLIProxyAPI config. Real credentials are injected into the parsed struct only;
// the runtime file contains __KEYCHAIN__ placeholders for every provider.
func BuildCLIProxyConfig(root string, gateway *GatewayConfig, wopalKey string, providerKeys map[string]string) (*sdkconfig.Config, string, error) {
	if gateway == nil {
		return nil, "", fmt.Errorf("build CLIProxyAPI config: gateway config is nil")
	}
	if err := gateway.ValidateProviders(); err != nil {
		return nil, "", fmt.Errorf("build CLIProxyAPI config: %w", err)
	}
	root, err := requireRoot(root)
	if err != nil {
		return nil, "", err
	}
	wopalModels := make([]map[string]any, 0, len(gateway.WopalAI.Models))
	for _, model := range gateway.WopalAI.Models {
		wopalModels = append(wopalModels, openAICompatibilityModelEntry("wopal-ai", model))
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
	openAICompatibility := []map[string]any{{
		"name":          "wopal-ai",
		"base-url":      strings.TrimRight(gateway.WopalAI.BaseURL, "/"),
		"request-retry": requestRetry,
		"api-key-entries": []map[string]any{{
			"api-key": "__KEYCHAIN__",
		}},
		"models": wopalModels,
	}}
	var codexKeys, claudeKeys, geminiKeys []map[string]any
	var chatProviderIDs, responsesProviderIDs, messagesProviderIDs, geminiProviderIDs []string

	providerIDs := make([]string, 0, len(gateway.Providers))
	for providerID := range gateway.Providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		provider := gateway.Providers[providerID]
		buckets := map[string][]ExternalModelConfig{}
		for _, model := range provider.Models {
			protocol := strings.TrimSpace(model.Protocol)
			if protocol == "" {
				continue
			}
			if err := ValidateProtocol(protocol); err != nil {
				return nil, "", fmt.Errorf("provider %q model %q: %w", providerID, model.Name, err)
			}
			buckets[protocol] = append(buckets[protocol], model)
		}
		if len(buckets) == 0 {
			continue
		}
		providerKey := strings.TrimSpace(providerKeys[providerID])
		if providerKey == "" {
			return nil, "", fmt.Errorf("provider %q API key is not available", providerID)
		}
		headers := cloneStringMap(provider.Headers)

		if models := buckets[ProtocolOpenAIChat]; len(models) > 0 {
			compiled := make([]map[string]any, 0, len(models))
			for _, model := range models {
				compiled = append(compiled, openAICompatibilityModelEntry(providerID, model))
			}
			entry := map[string]any{
				"name":            providerID + "-openai-chat",
				"base-url":        provider.BaseURLForProtocol(ProtocolOpenAIChat),
				"request-retry":   requestRetry,
				"api-key-entries": []map[string]any{{"api-key": "__KEYCHAIN__"}},
				"models":          compiled,
			}
			if len(headers) > 0 {
				entry["headers"] = headers
			}
			openAICompatibility = append(openAICompatibility, entry)
			chatProviderIDs = append(chatProviderIDs, providerID)
		}

		if models := buckets[ProtocolOpenAIResponses]; len(models) > 0 {
			compiled := make([]map[string]any, 0, len(models))
			for _, model := range models {
				entry := nativeModelEntry(providerID, model)
				entry["is-compat"] = true
				compiled = append(compiled, entry)
			}
			entry := map[string]any{
				"api-key":                "__KEYCHAIN__",
				"base-url":               provider.BaseURLForProtocol(ProtocolOpenAIResponses),
				"models":                 compiled,
				"headers":                headers,
				"disable-codex-cloaking": true,
				"request-retry":          requestRetry,
			}
			codexKeys = append(codexKeys, entry)
			responsesProviderIDs = append(responsesProviderIDs, providerID)
		}

		if models := buckets[ProtocolAnthropicMessages]; len(models) > 0 {
			compiled := make([]map[string]any, 0, len(models))
			for _, model := range models {
				compiled = append(compiled, nativeModelEntry(providerID, model))
			}
			claudeKeys = append(claudeKeys, map[string]any{
				"api-key":       "__KEYCHAIN__",
				"base-url":      provider.BaseURLForProtocol(ProtocolAnthropicMessages),
				"models":        compiled,
				"headers":       headers,
				"request-retry": requestRetry,
			})
			messagesProviderIDs = append(messagesProviderIDs, providerID)
		}

		if models := buckets[ProtocolGemini]; len(models) > 0 {
			compiled := make([]map[string]any, 0, len(models))
			for _, model := range models {
				compiled = append(compiled, nativeModelEntry(providerID, model))
			}
			geminiKeys = append(geminiKeys, map[string]any{
				"api-key":       "__KEYCHAIN__",
				"base-url":      provider.BaseURLForProtocol(ProtocolGemini),
				"models":        compiled,
				"headers":       headers,
				"request-retry": requestRetry,
			})
			geminiProviderIDs = append(geminiProviderIDs, providerID)
		}
	}

	raw := map[string]any{
		"host":     gateway.Listen.Host,
		"port":     gateway.Listen.Port,
		"api-keys": []string{LocalAccessKey},
		"auth-dir": authDir,
		// Codex separates a parent agent from the sub-agent it spawns by putting
		// the delegated task in a tool argument the client may encrypt. With this
		// off, the SDK forwards the resulting agent_message untouched, and a chat
		// completions target drops it: the sub-agent receives its role but no
		// task. Turning it on strips the encryption flag from the collaboration
		// tools and rewrites agent_message into a plain user message, which is
		// the only shape a third-party model can read.
		"client": map[string]any{
			"codex": map[string]any{
				"optimize-multi-agent-v2": true,
			},
		},
		"logging-to-file":          false,
		"usage-statistics-enabled": false,
		"remote-management": map[string]any{
			"allow-remote":          false,
			"disable-control-panel": true,
		},
		"openai-compatibility": openAICompatibility,
	}
	if len(codexKeys) > 0 {
		raw["codex-api-key"] = codexKeys
	}
	if len(claudeKeys) > 0 {
		raw["claude-api-key"] = claudeKeys
	}
	if len(geminiKeys) > 0 {
		raw["gemini-api-key"] = geminiKeys
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
	if len(cfg.OpenAICompatibility) != 1+len(chatProviderIDs) || len(cfg.OpenAICompatibility[0].APIKeyEntries) != 1 {
		return nil, "", fmt.Errorf("unexpected CLIProxyAPI OpenAI compatibility config shape")
	}
	cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey = wopalKey
	for index, providerID := range chatProviderIDs {
		entry := &cfg.OpenAICompatibility[index+1]
		if len(entry.APIKeyEntries) != 1 {
			return nil, "", fmt.Errorf("unexpected CLIProxyAPI chat key shape for provider %q", providerID)
		}
		entry.APIKeyEntries[0].APIKey = providerKeys[providerID]
	}
	if len(cfg.CodexKey) != len(responsesProviderIDs) || len(cfg.ClaudeKey) != len(messagesProviderIDs) || len(cfg.GeminiKey) != len(geminiProviderIDs) {
		return nil, "", fmt.Errorf("unexpected CLIProxyAPI external provider config shape")
	}
	for index, providerID := range responsesProviderIDs {
		cfg.CodexKey[index].APIKey = providerKeys[providerID]
	}
	for index, providerID := range messagesProviderIDs {
		cfg.ClaudeKey[index].APIKey = providerKeys[providerID]
	}
	for index, providerID := range geminiProviderIDs {
		cfg.GeminiKey[index].APIKey = providerKeys[providerID]
	}
	return cfg, runtimeConfigPath, nil
}

func externalClientModelName(providerID string, model ExternalModelConfig) string {
	if alias := strings.TrimSpace(model.Alias); alias != "" {
		return alias
	}
	return providerID + "/" + strings.TrimSpace(model.Name)
}

func nativeModelEntry(providerID string, model ExternalModelConfig) map[string]any {
	return map[string]any{
		"name":               model.Name,
		"alias":              externalClientModelName(providerID, model),
		"display-name":       providerID + " / " + model.Name,
		"max-context-length": model.EffectiveContextWindow(),
		"force-mapping":      true,
	}
}

func openAICompatibilityModelEntry(providerID string, model ExternalModelConfig) map[string]any {
	entry := nativeModelEntry(providerID, model)
	entry["input-modalities"] = []string{"text"}
	if model.SupportsVision() {
		entry["input-modalities"] = []string{"text", "image"}
	}
	return entry
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
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
