package main

import (
	"fmt"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

func runAuthCommand(subcommand string, args []string) error {
	providerID, err := authProviderID(args)
	if err != nil {
		return err
	}
	if providerID != "wopal-ai" {
		return runExternalAuthCommand(subcommand, providerID)
	}
	switch subcommand {
	case "set":
		return runAuthSet()
	case "test":
		return runAuthTest()
	case "status":
		fmt.Println(authStatusText(keychain.HasWopalAPIKey()))
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand: %s", subcommand)
	}
}

func authProviderID(args []string) (string, error) {
	if len(args) == 0 {
		return "wopal-ai", nil
	}
	if len(args) != 2 || args[0] != "--provider" || strings.TrimSpace(args[1]) == "" {
		return "", fmt.Errorf("auth accepts only optional --provider <id>")
	}
	id := strings.TrimSpace(args[1])
	if id == "wopal-ai" {
		return id, nil
	}
	if id == "official" {
		return "", fmt.Errorf("official credentials are supplied by Codex and are not managed by auth")
	}
	if err := config.ValidateProviderID(id); err != nil {
		return "", err
	}
	return id, nil
}

func runExternalAuthCommand(subcommand, providerID string) error {
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}
	providerCfg, ok := cfg.Providers[providerID]
	if !ok {
		return fmt.Errorf("provider %q is not configured", providerID)
	}

	switch subcommand {
	case "status":
		fmt.Printf("%s API key: %s\n", providerID, configuredText(keychain.HasProviderAPIKey(providerID)))
		return nil
	case "test":
		configured := keychain.HasProviderAPIKey(providerID)
		if !providerCfg.DiscoveryEnabled() {
			fmt.Printf("%s API key: %s; model discovery disabled, network validation skipped.\n", providerID, configuredText(configured))
			return nil
		}
		if !configured {
			return fmt.Errorf("%s API key is not configured. Run: wopal-codex-gateway auth set --provider %s", providerID, providerID)
		}
		models, err := validateStoredProviderKey(providerID, providerCfg)
		if err != nil {
			return err
		}
		fmt.Printf("%s API key validation succeeded: %d models available.\n", providerID, len(models))
		return nil
	case "set":
		if err := keychain.SetProviderAPIKeyInteractive(providerID); err != nil {
			return err
		}
		if !providerCfg.DiscoveryEnabled() {
			fmt.Printf("%s API key saved in macOS Keychain; model discovery disabled, network validation skipped.\n", providerID)
			return nil
		}
		models, err := validateStoredProviderKey(providerID, providerCfg)
		if err != nil {
			return fmt.Errorf("%s API key saved in macOS Keychain, but validation failed: %w", providerID, err)
		}
		fmt.Printf("%s API key saved and validated: %d models available.\n", providerID, len(models))
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand: %s", subcommand)
	}
}

func configuredText(configured bool) string {
	if configured {
		return "configured"
	}
	return "not configured"
}

func validateStoredProviderKey(providerID string, providerCfg config.ProviderConfig) ([]string, error) {
	useProviderKey := providerCfg.EffectiveModelsAuth() == "provider-key"
	key := ""
	var err error
	if useProviderKey {
		key, err = keychain.GetProviderAPIKey(providerID)
		if err != nil {
			return nil, err
		}
	}
	return catalog.FetchOpenAIModelIDs(providerCfg.EffectiveModelsURL(), key, useProviderKey)
}

func runAuthSet() error {
	if err := keychain.SetWopalAPIKeyInteractive(); err != nil {
		return err
	}
	models, configured, err := validateStoredWopalKey()
	if err != nil {
		return fmt.Errorf("Wopal AI API key saved in macOS Keychain, but validation failed: %w", err)
	}
	fmt.Printf("Wopal AI API key saved and validated: %d models available.\n", len(models))
	printConfiguredModelStatus(configured)
	return nil
}

func runAuthTest() error {
	models, configured, err := validateStoredWopalKey()
	if err != nil {
		return err
	}
	fmt.Printf("Wopal AI API key validation succeeded: %d models available.\n", len(models))
	printConfiguredModelStatus(configured)
	return nil
}

func authStatusText(configured bool) string {
	if configured {
		return "Wopal AI API key: configured"
	}
	return "Wopal AI API key: not configured"
}

func validateStoredWopalKey() ([]string, []string, error) {
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return nil, nil, fmt.Errorf("Wopal AI base URL is not configured")
	}
	key, err := keychain.GetWopalAPIKey()
	if err != nil {
		return nil, nil, err
	}
	models, err := catalog.FetchWopalModels(cfg.WopalAI.BaseURL, key)
	if err != nil {
		return nil, nil, err
	}

	available := make(map[string]bool, len(models))
	for _, model := range models {
		available[model] = true
	}
	configured := make([]string, 0, len(cfg.WopalAI.Models))
	for _, model := range cfg.WopalAI.Models {
		if available[model.Name] {
			configured = append(configured, model.Name)
		}
	}
	return models, configured, nil
}

func printConfiguredModelStatus(configured []string) {
	if len(configured) > 0 {
		fmt.Printf("Configured model(s) available: %s\n", strings.Join(configured, ", "))
		return
	}
	fmt.Println("Warning: none of the configured Wopal AI models were returned by /models.")
}
