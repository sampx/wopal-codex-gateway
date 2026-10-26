package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBuildCLIProxyConfigCompilesFourExternalProtocolsWithOneProviderKey(t *testing.T) {
	root := t.TempDir()
	gateway := DefaultGatewayConfig()
	gateway.WopalAI.BaseURL = "https://wopal.example/v1"
	gateway.WopalAI.Models = []ExternalModelConfig{{Name: "wopal-model"}}
	gateway.Providers["multi"] = ProviderConfig{
		BaseURL: "https://openai.example/v1",
		BaseURLs: map[string]string{
			ProtocolAnthropicMessages: "https://anthropic.example",
			ProtocolGemini:            "https://gemini.example",
		},
		Headers: map[string]string{
			"x-provider-session": "$CPA-SESSION-ID",
			"x-provider-static":  "literal",
		},
		Models: []ExternalModelConfig{
			{Name: "chat-model", Protocol: ProtocolOpenAIChat},
			{Name: "responses-model", Alias: "short-responses", Protocol: ProtocolOpenAIResponses},
			{Name: "messages-model", Protocol: ProtocolAnthropicMessages},
			{Name: "gemini-model", Protocol: ProtocolGemini},
			{Name: "unresolved-model"},
		},
	}

	cfg, path, err := BuildCLIProxyConfig(root, gateway, "wopal-secret", map[string]string{"multi": "provider-secret"})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.OpenAICompatibility) != 2 {
		t.Fatalf("openai compatibility buckets = %d, want Wopal + external chat", len(cfg.OpenAICompatibility))
	}
	chat := cfg.OpenAICompatibility[1]
	if chat.BaseURL != "https://openai.example/v1" || chat.Headers["x-provider-session"] != "$CPA-SESSION-ID" {
		t.Fatalf("chat bucket = %+v", chat)
	}
	if len(chat.APIKeyEntries) != 1 || chat.APIKeyEntries[0].APIKey != "provider-secret" {
		t.Fatalf("chat key entries = %+v", chat.APIKeyEntries)
	}
	if len(chat.Models) != 1 || chat.Models[0].Name != "chat-model" || chat.Models[0].Alias != "multi/chat-model" {
		t.Fatalf("chat models = %+v", chat.Models)
	}

	if len(cfg.CodexKey) != 1 {
		t.Fatalf("codex buckets = %d, want 1", len(cfg.CodexKey))
	}
	responses := cfg.CodexKey[0]
	if responses.BaseURL != "https://openai.example/v1" || responses.APIKey != "provider-secret" {
		t.Fatalf("responses bucket = %+v", responses)
	}
	if responses.DisableCodexCloaking == nil || !*responses.DisableCodexCloaking {
		t.Fatalf("responses disable-codex-cloaking = %v", responses.DisableCodexCloaking)
	}
	if responses.Headers["x-provider-session"] != "$CPA-SESSION-ID" {
		t.Fatalf("responses headers = %+v", responses.Headers)
	}
	if len(responses.Models) != 1 || !responses.Models[0].IsCompat || responses.Models[0].Alias != "short-responses" {
		t.Fatalf("responses models = %+v", responses.Models)
	}

	if len(cfg.ClaudeKey) != 1 {
		t.Fatalf("claude buckets = %d, want 1", len(cfg.ClaudeKey))
	}
	messages := cfg.ClaudeKey[0]
	if messages.BaseURL != "https://anthropic.example" || messages.APIKey != "provider-secret" || messages.Headers["x-provider-static"] != "literal" {
		t.Fatalf("messages bucket = %+v", messages)
	}
	if len(messages.Models) != 1 || messages.Models[0].Alias != "multi/messages-model" {
		t.Fatalf("messages models = %+v", messages.Models)
	}

	if len(cfg.GeminiKey) != 1 {
		t.Fatalf("gemini buckets = %d, want 1", len(cfg.GeminiKey))
	}
	gemini := cfg.GeminiKey[0]
	if gemini.BaseURL != "https://gemini.example" || gemini.APIKey != "provider-secret" || gemini.Headers["x-provider-session"] != "$CPA-SESSION-ID" {
		t.Fatalf("gemini bucket = %+v", gemini)
	}
	if len(gemini.Models) != 1 || gemini.Models[0].Alias != "multi/gemini-model" {
		t.Fatalf("gemini models = %+v", gemini.Models)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{"wopal-secret", "provider-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("runtime config persisted secret %q: %s", secret, text)
		}
	}
	if count := strings.Count(text, "__KEYCHAIN__"); count != 5 {
		t.Fatalf("placeholder count = %d, want 5 (Wopal + four external buckets): %s", count, text)
	}
	var disk map[string]json.RawMessage
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("runtime config is not JSON: %v", err)
	}
	for _, key := range []string{"openai-compatibility", "codex-api-key", "claude-api-key", "gemini-api-key"} {
		if _, ok := disk[key]; !ok {
			t.Fatalf("runtime config missing %q: %s", key, text)
		}
	}
}

func TestBuildCLIProxyConfigRequiresKeyForProtocolReadyExternalProvider(t *testing.T) {
	gateway := DefaultGatewayConfig()
	gateway.WopalAI.BaseURL = "https://wopal.example/v1"
	gateway.Providers["external"] = ProviderConfig{
		BaseURL: "https://api.example/v1",
		Models:  []ExternalModelConfig{{Name: "m", Protocol: ProtocolOpenAIChat}},
	}
	_, _, err := BuildCLIProxyConfig(t.TempDir(), gateway, "wopal-secret", nil)
	if err == nil || !strings.Contains(err.Error(), `provider "external" API key is not available`) {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildCLIProxyConfigSkipsNeedsProtocolWithoutProviderKey(t *testing.T) {
	gateway := DefaultGatewayConfig()
	gateway.WopalAI.BaseURL = "https://wopal.example/v1"
	gateway.Providers["external"] = ProviderConfig{
		BaseURL: "https://api.example/v1",
		Models:  []ExternalModelConfig{{Name: "needs-protocol"}},
	}
	cfg, _, err := BuildCLIProxyConfig(t.TempDir(), gateway, "wopal-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.CodexKey) != 0 || len(cfg.ClaudeKey) != 0 || len(cfg.GeminiKey) != 0 {
		t.Fatalf("unresolved external model produced executor: %+v", cfg)
	}
}
