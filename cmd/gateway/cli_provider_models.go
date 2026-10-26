package main

import (
	"fmt"
	"sort"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	providerreg "wopal-codex-gateway/internal/provider"
)

func parseModelsProvider(args []string) (string, []string, error) {
	providerID := ""
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] != "--provider" {
			remaining = append(remaining, args[i])
			continue
		}
		if providerID != "" {
			return "", nil, fmt.Errorf("--provider may be specified only once")
		}
		if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
			return "", nil, fmt.Errorf("--provider requires an id")
		}
		providerID = strings.TrimSpace(args[i+1])
		i++
	}
	if providerID == "official" {
		return "", nil, fmt.Errorf("official models are managed by the native Codex catalog")
	}
	if providerID != "" && providerID != "wopal-ai" {
		if err := config.ValidateProviderID(providerID); err != nil {
			return "", nil, err
		}
	}
	return providerID, remaining, nil
}

func (runner wopalModelsRunner) runProvider(subcommand, providerID string, args []string) error {
	switch subcommand {
	case "list":
		return runner.runProviderList(providerID)
	case "inspect":
		return runner.runProviderInspect(providerID, args)
	case "sync":
		return runner.runProviderSync(providerID)
	case "add":
		return runner.runProviderAdd(providerID, args)
	case "set":
		return runner.runProviderSet(providerID, args)
	case "remove":
		return runner.runProviderRemove(providerID, args)
	default:
		return fmt.Errorf("Unknown command: models %s", subcommand)
	}
}

func (runner wopalModelsRunner) providerConfig(providerID string) (*config.GatewayConfig, config.ProviderConfig, error) {
	cfg, err := runner.loadConfig()
	if err != nil {
		return nil, config.ProviderConfig{}, err
	}
	providerCfg, ok := cfg.Providers[providerID]
	if !ok {
		return nil, config.ProviderConfig{}, fmt.Errorf("provider %q is not configured", providerID)
	}
	return cfg, providerCfg, nil
}

func (runner wopalModelsRunner) providerAvailableModels(providerID string, providerCfg config.ProviderConfig) ([]string, error) {
	if !providerCfg.DiscoveryEnabled() {
		models := make([]string, 0, len(providerCfg.Models))
		for _, model := range providerCfg.Models {
			if name := strings.TrimSpace(model.Name); name != "" {
				models = append(models, name)
			}
		}
		return models, nil
	}
	if runner.fetchProviderModels == nil {
		return nil, fmt.Errorf("provider model discovery is unavailable")
	}
	useProviderKey := providerCfg.EffectiveModelsAuth() == "provider-key"
	key := ""
	if useProviderKey {
		if runner.getProviderAPIKey == nil {
			return nil, fmt.Errorf("provider credential lookup is unavailable")
		}
		var err error
		key, err = runner.getProviderAPIKey(providerID)
		if err != nil {
			return nil, err
		}
	}
	return runner.fetchProviderModels(providerCfg.EffectiveModelsURL(), key, useProviderKey)
}

func (runner wopalModelsRunner) runProviderList(providerID string) error {
	_, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	available, err := runner.providerAvailableModels(providerID, providerCfg)
	if err != nil {
		return err
	}
	var snapshot *catalog.ProviderModelMetadataSnapshot
	if runner.loadProviderMetadata != nil {
		snapshot, err = runner.loadProviderMetadata(providerID)
		if err != nil {
			return err
		}
	}
	configured := make(map[string]config.ExternalModelConfig, len(providerCfg.Models))
	for _, model := range providerCfg.Models {
		configured[model.Name] = model
	}
	seen := make(map[string]struct{}, len(available))
	for _, rawName := range available {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		model, isConfigured := configured[name]
		if !isConfigured {
			model = config.ExternalModelConfig{Name: name}
		}
		resolved, sources := catalog.ResolveProviderModel(model, providerCfg, snapshot)
		protocol := resolved.Protocol
		if protocol == "" {
			protocol = "needs_protocol"
		}
		runner.printf("%s  protocol=%s [%s]  configured=%t\n", name, protocol, sources.Protocol, isConfigured)
	}
	return nil
}

func (runner wopalModelsRunner) runProviderSync(providerID string) error {
	cfg, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	targetNames, err := runner.providerAvailableModels(providerID, providerCfg)
	if err != nil {
		return err
	}
	targetNames = filterProviderExcludedModels(providerCfg, targetNames)
	var previous *catalog.ProviderModelMetadataSnapshot
	if runner.loadProviderMetadata != nil {
		previous, err = runner.loadProviderMetadata(providerID)
		if err != nil {
			return err
		}
	}
	etag := ""
	if previous != nil {
		etag = previous.ETag
	}
	if runner.fetchModelsDevProvider == nil {
		return fmt.Errorf("models.dev metadata refresh is unavailable")
	}
	source, notModified, fetchErr := runner.fetchModelsDevProvider(providerID, etag)
	if fetchErr != nil {
		if previous != nil {
			runner.printf("Warning: models.dev refresh failed; keeping previous metadata: %v\n", fetchErr)
		} else {
			runner.printf("Warning: models.dev refresh failed; using explicit settings and defaults: %v\n", fetchErr)
		}
		result, refreshErr := runner.refreshModelCatalog(cfg)
		if refreshErr != nil {
			return refreshErr
		}
		runner.printModelCatalog(result)
		return nil
	}
	if notModified {
		if rejoined, ok := catalog.RejoinPreviousProviderMetadata(previous, targetNames); ok {
			if runner.saveProviderMetadata == nil {
				return fmt.Errorf("provider metadata persistence is unavailable")
			}
			if err := runner.saveProviderMetadata(rejoined); err != nil {
				return err
			}
			runner.printf("%s models.dev metadata unchanged; joined model set refreshed.\n", providerID)
			result, refreshErr := runner.refreshModelCatalog(cfg)
			if refreshErr != nil {
				return refreshErr
			}
			runner.printModelCatalog(result)
			return nil
		}
		// The catalog itself is unchanged but the provider exposed a newly
		// available/configured model that was intentionally absent from the
		// persisted joined slice. Refetch once without ETag so that model can be
		// joined without ever persisting the full models.dev catalog.
		source, notModified, fetchErr = runner.fetchModelsDevProvider(providerID, "")
		if fetchErr != nil {
			runner.printf("Warning: models.dev metadata unchanged but joined set grew; full refresh failed, keeping previous metadata: %v\n", fetchErr)
			result, refreshErr := runner.refreshModelCatalog(cfg)
			if refreshErr != nil {
				return refreshErr
			}
			runner.printModelCatalog(result)
			return nil
		}
		if notModified || source == nil {
			return fmt.Errorf("models.dev returned 304 without a usable previous join for new models")
		}
	}
	if source == nil {
		return fmt.Errorf("models.dev returned no provider metadata")
	}
	joined := catalog.JoinProviderMetadata(source, targetNames)
	joined.Provider = providerID
	if runner.saveProviderMetadata == nil {
		return fmt.Errorf("provider metadata persistence is unavailable")
	}
	if err := runner.saveProviderMetadata(joined); err != nil {
		return err
	}
	runner.reportProviderMetadataChanges(providerCfg, previous, joined)
	runner.printf("%s model metadata synced: %d model(s).\n", providerID, len(joined.Models))
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) reportProviderMetadataChanges(providerCfg config.ProviderConfig, previous, current *catalog.ProviderModelMetadataSnapshot) {
	oldByName := make(map[string]catalog.ProviderModelMetadata)
	if previous != nil {
		for _, model := range previous.Models {
			oldByName[model.Name] = model
		}
	}
	newByName := make(map[string]catalog.ProviderModelMetadata, len(current.Models))
	for _, model := range current.Models {
		newByName[model.Name] = model
		old := oldByName[model.Name]
		if old.Protocol != "" && model.Protocol != "" && old.Protocol != model.Protocol {
			runner.printf("%s protocol changed: %s -> %s\n", model.Name, old.Protocol, model.Protocol)
		}
	}
	for _, explicit := range providerCfg.Models {
		metadata, ok := newByName[explicit.Name]
		if !ok {
			continue
		}
		if explicit.Protocol != "" && metadata.Protocol != "" && explicit.Protocol != metadata.Protocol {
			runner.printf("Warning: %s explicit protocol %s differs from metadata %s; keeping explicit value.\n", explicit.Name, explicit.Protocol, metadata.Protocol)
		}
		if explicit.ContextWindow != nil && metadata.ContextWindow != nil && *explicit.ContextWindow != *metadata.ContextWindow {
			runner.printf("Warning: %s explicit contextWindow %d differs from metadata %d; keeping explicit value.\n", explicit.Name, *explicit.ContextWindow, *metadata.ContextWindow)
		}
		if explicit.Vision != nil && metadata.Vision != nil && *explicit.Vision != *metadata.Vision {
			runner.printf("Warning: %s explicit vision %t differs from metadata %t; keeping explicit value.\n", explicit.Name, *explicit.Vision, *metadata.Vision)
		}
		if explicit.Reasoning != nil && metadata.Reasoning != nil && *explicit.Reasoning != *metadata.Reasoning {
			runner.printf("Warning: %s explicit reasoning %t differs from metadata %t; keeping explicit value.\n", explicit.Name, *explicit.Reasoning, *metadata.Reasoning)
		}
	}
}

func (runner wopalModelsRunner) runProviderAdd(providerID string, args []string) error {
	cfg, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	names := positionals(args, "--context-window", "--alias", "--protocol")
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models add <model...> --provider <id> [--alias <name>] [--protocol <protocol>]")
	}
	patch, err := parseProviderModelPatch(args, false)
	if err != nil {
		return err
	}
	added := catalog.AddProviderModelConfigs(providerCfg.Models, names, patch)
	providerCfg.Models = added.Models
	unexcluded := make(map[string]struct{}, len(added.Added)+len(added.Existing))
	for _, name := range added.Added {
		unexcluded[name] = struct{}{}
	}
	for _, name := range added.Existing {
		unexcluded[name] = struct{}{}
	}
	excluded := make([]string, 0, len(providerCfg.ExcludedModels))
	for _, name := range providerCfg.ExcludedModels {
		if _, ok := unexcluded[strings.TrimSpace(name)]; !ok {
			excluded = append(excluded, name)
		}
	}
	providerCfg.ExcludedModels = excluded
	cfg.Providers[providerID] = providerCfg
	if _, err := providerreg.Build(cfg); err != nil {
		return err
	}
	if runner.validateOfficialNames != nil {
		if err := runner.validateOfficialNames(cfg); err != nil {
			return err
		}
	}
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}
	if len(added.Added) > 0 {
		runner.printf("%s model(s) added: %s\n", providerID, strings.Join(added.Added, ", "))
	}
	if len(added.Existing) > 0 {
		runner.printf("Already configured: %s\n", strings.Join(added.Existing, ", "))
	}
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) runProviderSet(providerID string, args []string) error {
	cfg, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	names := positionals(args, "--context-window", "--alias", "--protocol")
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models set <model...> --provider <id> [metadata options]")
	}
	patch, err := parseProviderModelPatch(args, true)
	if err != nil {
		return err
	}
	if !providerPatchHasChanges(patch) {
		return fmt.Errorf("models set requires at least one metadata option")
	}
	updated := catalog.UpdateProviderModelConfigs(providerCfg.Models, names, patch)
	if len(updated.Missing) > 0 {
		return fmt.Errorf("Model(s) not configured: %s. Run models add first.", strings.Join(updated.Missing, ", "))
	}
	providerCfg.Models = updated.Models
	cfg.Providers[providerID] = providerCfg
	if _, err := providerreg.Build(cfg); err != nil {
		return err
	}
	if runner.validateOfficialNames != nil {
		if err := runner.validateOfficialNames(cfg); err != nil {
			return err
		}
	}
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}
	runner.printf("%s model metadata updated: %s\n", providerID, strings.Join(updated.Updated, ", "))
	runner.printModelCatalog(result)
	return nil
}

func (runner wopalModelsRunner) runProviderRemove(providerID string, args []string) error {
	cfg, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	names := positionals(args)
	if len(names) == 0 {
		return fmt.Errorf("Usage: wopal-codex-gateway models remove <model...> --provider <id>")
	}
	removed := catalog.RemoveProviderModelConfigs(providerCfg.Models, names)
	providerCfg.Models = removed.Models
	providerCfg.ExcludedModels = appendUniqueModelExclusions(providerCfg.ExcludedModels, removed.Removed, removed.Missing)
	cfg.Providers[providerID] = providerCfg
	if err := runner.saveConfig(cfg); err != nil {
		return err
	}
	result, err := runner.refreshModelCatalog(cfg)
	if err != nil {
		return err
	}
	if len(removed.Removed) > 0 {
		runner.printf("%s model(s) removed: %s\n", providerID, strings.Join(removed.Removed, ", "))
	}
	if len(removed.Missing) > 0 {
		runner.printf("Not configured: %s\n", strings.Join(removed.Missing, ", "))
	}
	runner.printModelCatalog(result)
	return nil
}

func filterProviderExcludedModels(providerCfg config.ProviderConfig, names []string) []string {
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if providerCfg.IsModelExcluded(name) {
			continue
		}
		filtered = append(filtered, name)
	}
	return filtered
}

func (runner wopalModelsRunner) runProviderInspect(providerID string, args []string) error {
	_, providerCfg, err := runner.providerConfig(providerID)
	if err != nil {
		return err
	}
	var snapshot *catalog.ProviderModelMetadataSnapshot
	if runner.loadProviderMetadata != nil {
		snapshot, err = runner.loadProviderMetadata(providerID)
		if err != nil {
			return err
		}
	}
	names := positionals(args)
	selected := make(map[string]struct{}, len(names))
	for _, name := range names {
		selected[strings.TrimSpace(name)] = struct{}{}
	}
	for name := range selected {
		found := false
		for _, model := range providerCfg.Models {
			if model.Name == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Model not configured: %s", name)
		}
	}
	models := append([]config.ExternalModelConfig(nil), providerCfg.Models...)
	sort.SliceStable(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	for _, model := range models {
		if len(selected) > 0 {
			if _, ok := selected[model.Name]; !ok {
				continue
			}
		}
		resolved, sources := catalog.ResolveProviderModel(model, providerCfg, snapshot)
		protocol := resolved.Protocol
		if protocol == "" {
			protocol = "needs_protocol"
		}
		clientName := providerID + "/" + model.Name
		if strings.TrimSpace(model.Alias) != "" {
			clientName = model.Alias
		}
		runner.printf("%s  upstream=%s  protocol=%s [%s]  context=%d [%s]  vision=%t [%s]  reasoning=%t [%s]\n",
			clientName, model.Name, protocol, sources.Protocol,
			resolved.EffectiveContextWindow(), sources.ContextWindow,
			resolved.SupportsVision(), sources.Vision,
			resolved.SupportsReasoning(), sources.Reasoning)
	}
	return runner.warnCodexContextOverrides()
}

func parseProviderModelPatch(args []string, allowReset bool) (catalog.ModelPatch, error) {
	patch, err := parseModelPatch(args, allowReset)
	if err != nil {
		return patch, err
	}
	if hasFlag(args, "--alias") {
		alias, ok := option(args, "--alias")
		if !ok {
			return patch, fmt.Errorf("--alias requires a value; use --alias '' to clear it")
		}
		alias = strings.TrimSpace(alias)
		if alias == "" {
			if !allowReset {
				return patch, fmt.Errorf("empty --alias is only valid with models set")
			}
			patch.ResetAlias = true
		} else {
			patch.Alias = &alias
		}
	}
	if hasFlag(args, "--protocol") {
		protocol, ok := option(args, "--protocol")
		if !ok || strings.TrimSpace(protocol) == "" {
			return patch, fmt.Errorf("--protocol requires a protocol")
		}
		protocol = strings.TrimSpace(protocol)
		if err := config.ValidateProtocol(protocol); err != nil {
			return patch, err
		}
		patch.Protocol = &protocol
	}
	return patch, nil
}

func providerPatchHasChanges(patch catalog.ModelPatch) bool {
	return patch.Alias != nil || patch.Protocol != nil || patch.ContextWindow != nil || patch.Vision != nil || patch.Reasoning != nil ||
		patch.ResetAlias || patch.ResetContextWindow || patch.ResetVision || patch.ResetReasoning
}
