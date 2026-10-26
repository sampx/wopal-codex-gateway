package main

import (
	"fmt"
	"strings"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/provider"
)

func runProvidersCommand(subcommand string, args []string) error {
	switch subcommand {
	case "set":
		return runProvidersSet(args)
	case "remove":
		return runProvidersRemove(args)
	default:
		return fmt.Errorf("unknown providers subcommand: %s", subcommand)
	}
}

func runProvidersSet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s providers set <provider> --base-url <url>", binaryName)
	}
	id := strings.TrimSpace(args[0])
	if err := config.ValidateProviderID(id); err != nil {
		return err
	}

	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}
	current, exists := cfg.Providers[id]
	next := current.Clone()
	if err := applyProviderSetOptions(&next, args[1:]); err != nil {
		return err
	}
	if !exists && strings.TrimSpace(next.BaseURL) == "" {
		return fmt.Errorf("creating provider %q requires --base-url", id)
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]config.ProviderConfig)
	}
	cfg.Providers[id] = next
	if _, err := provider.Build(cfg); err != nil {
		return err
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		return err
	}
	if err := signalGatewayReloadAfterCommit(); err != nil {
		return fmt.Errorf("provider configuration saved but gateway reload failed: %w", err)
	}
	if exists {
		fmt.Println("updated")
	} else {
		fmt.Println("created")
	}
	return nil
}

func runProvidersRemove(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "--") {
		return fmt.Errorf("usage: %s providers remove <provider>", binaryName)
	}
	id := strings.TrimSpace(args[0])
	if err := config.ValidateProviderID(id); err != nil {
		return err
	}
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}
	if _, exists := cfg.Providers[id]; !exists {
		return fmt.Errorf("provider %q is not configured", id)
	}
	delete(cfg.Providers, id)
	if _, err := provider.Build(cfg); err != nil {
		return err
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		return err
	}
	if err := signalGatewayReloadAfterCommit(); err != nil {
		return fmt.Errorf("provider configuration saved but gateway reload failed: %w", err)
	}
	fmt.Println("removed")
	return nil
}

func applyProviderSetOptions(dst *config.ProviderConfig, args []string) error {
	for index := 0; index < len(args); index++ {
		flag := args[index]
		if !strings.HasPrefix(flag, "--") {
			return fmt.Errorf("unexpected providers set argument %q", flag)
		}
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
			return fmt.Errorf("%s requires a value", flag)
		}
		value := args[index+1]
		index++

		switch flag {
		case "--base-url":
			normalized, err := normalizedProviderURL(flag, value)
			if err != nil {
				return err
			}
			dst.BaseURL = normalized
		case "--base-url-for":
			protocolName, rawURL, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(protocolName) == "" || strings.TrimSpace(rawURL) == "" {
				return fmt.Errorf("--base-url-for requires <protocol>=<url>")
			}
			protocolName = strings.TrimSpace(protocolName)
			if err := config.ValidateProtocol(protocolName); err != nil {
				return err
			}
			normalized, err := normalizedProviderURL("--base-url-for", rawURL)
			if err != nil {
				return err
			}
			if dst.BaseURLs == nil {
				dst.BaseURLs = make(map[string]string)
			}
			dst.BaseURLs[protocolName] = normalized
		case "--models-url":
			normalized, err := normalizedProviderURL(flag, value)
			if err != nil {
				return err
			}
			dst.ModelsURL = normalized
		case "--models-auth":
			if value != "none" && value != "provider-key" {
				return fmt.Errorf("--models-auth must be none or provider-key")
			}
			dst.ModelsAuth = value
		case "--header":
			name, headerValue, ok := strings.Cut(value, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return fmt.Errorf("--header requires <name>=<value>")
			}
			if dst.Headers == nil {
				dst.Headers = make(map[string]string)
			}
			dst.Headers[name] = headerValue
		case "--protocol":
			if err := config.ValidateProtocol(value); err != nil || value == "" {
				if err != nil {
					return err
				}
				return fmt.Errorf("--protocol requires a protocol")
			}
			dst.Protocol = value
		default:
			return fmt.Errorf("unknown providers set option: %s", flag)
		}
	}
	return nil
}

func normalizedProviderURL(flag, value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := config.ValidateHTTPURL(value); err != nil {
		return "", fmt.Errorf("%s %w", flag, err)
	}
	return strings.TrimRight(value, "/"), nil
}
