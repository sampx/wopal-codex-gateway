package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

type wopalModelsRunner struct {
	loadConfig             func() (*config.GatewayConfig, error)
	saveConfig             func(*config.GatewayConfig) error
	fetchModels            func(string, string) ([]string, error)
	fetchMetadata          func(string, string) (*catalog.ModelMetadataSnapshot, error)
	loadMetadata           func(string) (*catalog.ModelMetadataSnapshot, error)
	saveMetadata           func(*catalog.ModelMetadataSnapshot) error
	getAPIKey              func() (string, error)
	fetchProviderModels    func(string, string, bool) ([]string, error)
	fetchModelsDevProvider func(string, string) (*catalog.ModelsDevProvider, bool, error)
	loadProviderMetadata   func(string) (*catalog.ProviderModelMetadataSnapshot, error)
	saveProviderMetadata   func(*catalog.ProviderModelMetadataSnapshot) error
	getProviderAPIKey      func(string) (string, error)
	validateOfficialNames  func(*config.GatewayConfig) error
	refreshCatalog         func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error)
	signalReload           func() error
	out                    io.Writer
}

func newWopalModelsRunner() wopalModelsRunner {
	return wopalModelsRunner{
		loadConfig: func() (*config.GatewayConfig, error) {
			return config.LoadGatewayConfig(config.ConfigPath())
		},
		saveConfig:          config.SaveGatewayConfig,
		fetchModels:         catalog.FetchWopalModels,
		fetchMetadata:       catalog.FetchWopalModelMetadata,
		loadMetadata:        catalog.LoadModelMetadata,
		saveMetadata:        catalog.SaveModelMetadata,
		getAPIKey:           keychain.GetWopalAPIKey,
		fetchProviderModels: catalog.FetchOpenAIModelIDs,
		fetchModelsDevProvider: func(providerID, etag string) (*catalog.ModelsDevProvider, bool, error) {
			return catalog.FetchModelsDevProvider(catalog.ModelsDevCatalogURL, providerID, etag)
		},
		loadProviderMetadata:  catalog.LoadProviderModelMetadata,
		saveProviderMetadata:  catalog.SaveProviderModelMetadata,
		getProviderAPIKey:     keychain.GetProviderAPIKey,
		validateOfficialNames: catalog.ValidateExternalNamesAgainstOfficial,
		refreshCatalog:        catalog.RefreshCatalog,
		signalReload:          signalGatewayReloadAfterCommit,
		out:                   os.Stdout,
	}
}

func runModelsCommand(subcommand string, args []string) error {
	return newWopalModelsRunner().run(subcommand, args)
}

func (runner wopalModelsRunner) run(subcommand string, args []string) error {
	providerID, remaining, err := parseModelsProvider(args)
	if err != nil {
		return err
	}
	if providerID != "" && providerID != "wopal-ai" {
		return runner.runProvider(subcommand, providerID, remaining)
	}
	args = remaining
	switch subcommand {
	case "list":
		return runner.runList()
	case "inspect":
		return runner.runInspect(args)
	case "sync":
		return runner.runSync()
	case "add":
		return runner.runAdd(args)
	case "set":
		return runner.runSet(args)
	case "remove":
		return runner.runRemove(args)
	default:
		return fmt.Errorf("Unknown command: models %s", subcommand)
	}
}

func (runner wopalModelsRunner) runList() error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	apiKey, err := runner.getAPIKey()
	if err != nil {
		return err
	}
	models, err := runner.fetchModels(cfg.WopalAI.BaseURL, apiKey)
	if err != nil {
		return err
	}
	for _, model := range models {
		runner.printf("%s\n", model)
	}
	return nil
}

func (runner wopalModelsRunner) runSync() error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return fmt.Errorf("Wopal AI base URL is not configured")
	}
	apiKey, err := runner.getAPIKey()
	if err != nil {
		return err
	}
	var available []string
	var snapshot *catalog.ModelMetadataSnapshot
	if runner.fetchMetadata != nil {
		snapshot, err = runner.fetchMetadata(cfg.WopalAI.BaseURL, apiKey)
		if err != nil {
			return err
		}
		if snapshot == nil {
			return fmt.Errorf("provider returned no model metadata")
		}
		for _, m := range snapshot.Models {
			available = append(available, m.Name)
		}
		if err := runner.saveMetadata(snapshot); err != nil {
			return err
		}
	} else {
		available, err = runner.fetchModels(cfg.WopalAI.BaseURL, apiKey)
		if err != nil {
			return err
		}
	}

	previous := cfg.WopalAI.Models
	synced := catalog.SyncWopalModelConfigs(previous, available, cfg.WopalAI.ExcludedModels)
	cfg.WopalAI.Models = synced.Models
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}

	runner.printf("Wopal AI models synced: %d Codex-usable model(s).\n", len(synced.Models))
	if len(synced.Skipped) > 0 {
		runner.printf("Skipped non-chat model(s): %s\n", strings.Join(synced.Skipped, ", "))
	}
	if len(synced.Excluded) > 0 {
		runner.printf("Manually excluded model(s): %s\n", strings.Join(synced.Excluded, ", "))
	}
	if snapshot != nil {
		oldNames := make(map[string]bool, len(previous))
		newNames := make(map[string]bool, len(synced.Models))
		for _, m := range previous {
			oldNames[m.Name] = true
		}
		added, removed, overrides, defaults := 0, 0, 0, 0
		for _, m := range synced.Models {
			newNames[m.Name] = true
			if !oldNames[m.Name] {
				added++
			}
			if m.ContextWindow != nil || m.Vision != nil || m.Reasoning != nil {
				overrides++
			}
			_, sources := catalog.ResolveModel(m, snapshot)
			if sources.ContextWindow == "default" || sources.Vision == "default" || sources.Reasoning == "default" {
				defaults++
			}
		}
		for name := range oldNames {
			if !newNames[name] {
				removed++
			}
		}
		runner.printf("Changes: added=%d, removed=%d, preserved-overrides=%d, using-defaults=%d.\n", added, removed, overrides, defaults)
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) runAdd(args []string) error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	names := positionals(args, "--context-window")
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models add <model...> [--context-window N] [--vision] [--no-reasoning]")
	}
	patch, err := parseModelPatch(args, false)
	if err != nil {
		return err
	}
	added := catalog.AddWopalModelConfigs(cfg.WopalAI.Models, names, patch)
	cfg.WopalAI.Models = added.Models

	unexcluded := make(map[string]struct{}, len(added.Added)+len(added.Existing))
	for _, name := range added.Added {
		unexcluded[name] = struct{}{}
	}
	for _, name := range added.Existing {
		unexcluded[name] = struct{}{}
	}
	excluded := make([]string, 0, len(cfg.WopalAI.ExcludedModels))
	for _, name := range cfg.WopalAI.ExcludedModels {
		if _, ok := unexcluded[name]; !ok {
			excluded = append(excluded, name)
		}
	}
	cfg.WopalAI.ExcludedModels = excluded

	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}

	if len(added.Added) > 0 {
		runner.printf("Wopal AI model(s) added: %s\n", strings.Join(added.Added, ", "))
	}
	if len(added.Existing) > 0 {
		runner.printf("Already configured: %s\n", strings.Join(added.Existing, ", "))
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) runSet(args []string) error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	names := positionals(args, "--context-window")
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models set <model...> [--context-window N] [--vision|--no-vision] [--reasoning|--no-reasoning]")
	}
	patch, err := parseModelPatch(args, true)
	if err != nil {
		return err
	}
	if patch.ContextWindow == nil && patch.Vision == nil && patch.Reasoning == nil &&
		!patch.ResetContextWindow && !patch.ResetVision && !patch.ResetReasoning {
		return fmt.Errorf("models set requires at least one metadata option")
	}

	updated := catalog.UpdateWopalModelConfigs(cfg.WopalAI.Models, names, patch)
	if len(updated.Missing) > 0 {
		return fmt.Errorf("Model(s) not configured: %s. Run models sync or models add first.", strings.Join(updated.Missing, ", "))
	}
	cfg.WopalAI.Models = updated.Models
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}

	runner.printf("Wopal AI model metadata updated: %s\n", strings.Join(updated.Updated, ", "))
	for _, name := range updated.Updated {
		model := findExternalModel(updated.Models, name)
		var snapshot *catalog.ModelMetadataSnapshot
		if runner.loadMetadata != nil {
			snapshot, err = runner.loadMetadata(cfg.WopalAI.BaseURL)
			if err != nil {
				return err
			}
		}
		model, _ = catalog.ResolveModel(model, snapshot)
		runner.printf("  wopal-ai/%s: context=%d, vision=%t, reasoning=%t\n",
			name, model.EffectiveContextWindow(), model.SupportsVision(), model.SupportsReasoning())
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) runRemove(args []string) error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			names = append(names, arg)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models remove <model...>")
	}

	removed := catalog.RemoveWopalModelConfigs(cfg.WopalAI.Models, names)
	cfg.WopalAI.Models = removed.Models
	cfg.WopalAI.ExcludedModels = appendUniqueModelExclusions(
		cfg.WopalAI.ExcludedModels,
		removed.Removed,
		removed.Missing,
	)
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}

	if len(removed.Removed) > 0 {
		runner.printf("Wopal AI model(s) removed: %s\n", strings.Join(removed.Removed, ", "))
	}
	if len(removed.Missing) > 0 {
		runner.printf("Not configured, but excluded from future sync: %s\n", strings.Join(removed.Missing, ", "))
	}
	if len(removed.Removed) > 0 {
		runner.printf("Removed model(s) are excluded from future sync until explicitly added again.\n")
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) refreshModelCatalog(cfg *config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
	result, err := runner.refreshCatalog(cfg)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("catalog refresh returned no result")
	}
	if runner.signalReload != nil {
		if err := runner.signalReload(); err != nil {
			return nil, fmt.Errorf("model changes saved but gateway reload failed: %w", err)
		}
	}
	return result, nil
}

func (runner wopalModelsRunner) printModelCatalog(result *catalog.CatalogRefreshResult) {
	runner.printf("Model catalog: official-visible=%d, official-internal=%d, wopal-ai=%d\n",
		result.NativeVisible, result.NativeHidden, result.WopalAI)
}

func (runner wopalModelsRunner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(runner.out, format, args...)
}

func findExternalModel(models []config.ExternalModelConfig, name string) config.ExternalModelConfig {
	for _, model := range models {
		if model.Name == name {
			return model
		}
	}
	return config.ExternalModelConfig{Name: name}
}

func appendUniqueModelExclusions(existing []string, groups ...[]string) []string {
	seen := make(map[string]struct{}, len(existing))
	result := make([]string, 0, len(existing))
	for _, name := range existing {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	for _, group := range groups {
		for _, name := range group {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			result = append(result, name)
		}
	}
	return result
}
