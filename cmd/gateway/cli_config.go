package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

func runConfigCommand(subcommand string, args []string) error {
	switch subcommand {
	case "migrate":
		return runConfigMigrate(args)
	case "show":
		return runConfigShow()
	case "wopal":
		return runConfigWopal(args)
	default:
		return fmt.Errorf("unknown config subcommand: %s", subcommand)
	}
}

func runConfigShow() error {
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode gateway config: %w", err)
	}
	fmt.Println(string(payload))
	return nil
}

func runConfigWopal(args []string) error {
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}

	if mode, ok := option(args, "--tool-mode"); ok {
		if mode == "" {
			return fmt.Errorf("--tool-mode requires direct, code_mode or code_mode_only")
		}
		cfg.WopalAI.ToolMode = mode
		if err := cfg.WopalAI.ValidateToolMode(); err != nil {
			return err
		}
	} else if hasFlag(args, "--tool-mode") {
		return fmt.Errorf("--tool-mode requires direct, code_mode or code_mode_only")
	}

	if baseURL, ok := option(args, "--base-url"); ok {
		parsed, err := url.Parse(baseURL)
		if err != nil {
			return fmt.Errorf("parse Wopal AI base URL: %w", err)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("Wopal AI base URL must use http or https")
		}
		cfg.WopalAI.BaseURL = strings.TrimRight(baseURL, "/")
	}

	names := nonEmptyOptions(args, "--model")
	if len(names) > 0 {
		patch, err := parseModelPatch(args, false)
		if err != nil {
			return err
		}
		cfg.WopalAI.Models = catalog.AddWopalModelConfigs(nil, names, patch).Models

		explicit := make(map[string]bool, len(names))
		for _, name := range names {
			explicit[stripConfigModelPrefix(name)] = true
		}
		excluded := make([]string, 0, len(cfg.WopalAI.ExcludedModels))
		for _, name := range cfg.WopalAI.ExcludedModels {
			if !explicit[name] {
				excluded = append(excluded, name)
			}
		}
		cfg.WopalAI.ExcludedModels = excluded
	}

	if err := config.SaveGatewayConfig(cfg); err != nil {
		return err
	}

	baseURL := cfg.WopalAI.BaseURL
	if baseURL == "" {
		baseURL = "(base URL not set)"
	}
	models := make([]string, 0, len(cfg.WopalAI.Models))
	for _, model := range cfg.WopalAI.Models {
		models = append(models, "wopal-ai/"+model.Name)
	}
	if hasFlag(args, "--tool-mode") {
		fmt.Printf("Wopal AI tool mode: %s. Run catalog refresh, then use a new turn/conversation to load the catalog.\n", cfg.WopalAI.EffectiveToolMode())
		if cfg.WopalAI.ToolMode == "direct" {
			fmt.Println("Direct mode has no exec tool; start a new conversation when switching from a code-mode model.")
		}
	}
	fmt.Printf("Wopal AI configured: %s; models=%s\n", baseURL, strings.Join(models, ", "))
	return nil
}

func nonEmptyOptions(args []string, name string) []string {
	values := options(args, name)
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			kept = append(kept, value)
		}
	}
	return kept
}

func stripConfigModelPrefix(name string) string {
	return strings.TrimPrefix(name, "wopal-ai/")
}
