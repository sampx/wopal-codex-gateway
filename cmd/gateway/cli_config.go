package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"wopal-codex-gateway/internal/config"
)

func runConfigCommand(subcommand string, args []string) error {
	switch subcommand {
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
		contextWindow, _, err := numberOption(args, "--context-window")
		if err != nil {
			return err
		}
		if contextWindow == 0 {
			contextWindow = config.DefaultContextWindow
		}
		vision := hasFlag(args, "--vision")
		reasoning := !hasFlag(args, "--no-reasoning")

		cfg.WopalAI.Models = make([]config.ExternalModelConfig, 0, len(names))
		for _, name := range names {
			cfg.WopalAI.Models = append(cfg.WopalAI.Models, config.ExternalModelConfig{
				Name:          name,
				ContextWindow: &contextWindow,
				Vision:        &vision,
				Reasoning:     &reasoning,
			})
		}

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
