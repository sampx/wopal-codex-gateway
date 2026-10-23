package main

import (
	"fmt"
	"strings"
)

func runAuthCommand(subcommand string, args []string) error {
	switch subcommand {
	case "set":
		return runAuthSet()
	case "test":
		return runAuthTest()
	case "status":
		fmt.Println(authStatusText(hasWopalAPIKey()))
		return nil
	default:
		return fmt.Errorf("unknown auth subcommand: %s", subcommand)
	}
}

func runAuthSet() error {
	if err := setWopalAPIKeyInteractive(); err != nil {
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
	cfg, err := loadGatewayConfig(configPath())
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return nil, nil, fmt.Errorf("Wopal AI base URL is not configured")
	}
	key, err := getWopalAPIKey()
	if err != nil {
		return nil, nil, err
	}
	models, err := fetchWopalModels(cfg.WopalAI.BaseURL, key)
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
