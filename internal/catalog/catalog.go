package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"wopal-codex-gateway/internal/config"
)

// CatalogRefreshResult summarises one catalog rebuild.
type CatalogRefreshResult struct {
	Native        int
	NativeVisible int
	NativeHidden  int
	WopalAI       int
	External      int
}

// CatalogCommandOutput is the captured result of `codex debug models`.
type CatalogCommandOutput struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

// catalogCommandOutput keeps the internal call sites terse.
type catalogCommandOutput = CatalogCommandOutput

var (
	catalogResolveCodex          = resolveCodexCommand
	catalogResolveNativeArtifact = ResolveNativeCodexArtifact
	catalogComputeNativeIdentity = ComputeNativeArtifactIdentity
	catalogExecuteCodex          = runCodexDebugModels
	catalogWriteAtomic           = config.WriteFileAtomic
)

// ResolveCodexHook, ExecuteCodexHook and RefreshCatalogHook are seams so the
// install flow can run end to end without the real codex binary.
var (
	ResolveCodexHook   = func() (string, error) { return catalogResolveCodex() }
	ExecuteCodexHook   = func(command string) CatalogCommandOutput { return catalogExecuteCodex(command) }
	RefreshCatalogHook = RefreshCatalog
)

var catalogReasoningLevels = []any{
	map[string]any{"effort": "none", "description": "Turn reasoning off"},
	map[string]any{"effort": "low", "description": "Fast responses with lighter reasoning"},
	map[string]any{"effort": "medium", "description": "Balanced reasoning depth"},
	map[string]any{"effort": "high", "description": "Greater reasoning depth for complex work"},
}

type externalCatalogModel struct {
	ProviderID string
	Model      config.ExternalModelConfig
	ToolMode   string
}

func buildCatalog(native []map[string]any, wopalModels []config.ExternalModelConfig, toolMode string) []map[string]any {
	return buildCatalogWithExternal(native, wopalModels, toolMode, nil)
}

func buildCatalogWithExternal(native []map[string]any, wopalModels []config.ExternalModelConfig, wopalToolMode string, external []externalCatalogModel) []map[string]any {
	catalog := make([]map[string]any, 0, len(wopalModels)+len(external)+len(native))
	for index, model := range wopalModels {
		catalog = append(catalog, wopalCatalogEntry(model, -200+index, wopalToolMode))
	}
	for index, item := range external {
		toolMode := item.ToolMode
		if strings.TrimSpace(toolMode) == "" {
			toolMode = config.DefaultToolMode
		}
		catalog = append(catalog, externalCatalogEntry(item.ProviderID, item.Model, -100+index, toolMode))
	}
	for _, model := range native {
		copied := make(map[string]any, len(model))
		for key, value := range model {
			copied[key] = value
		}
		// Current Codex promotes base_instructions into model_messages only
		// when instructions_template is absent. Drop exact duplicates only;
		// retain every distinct instruction and all other official metadata.
		if messages, ok := copied["model_messages"].(map[string]any); ok {
			if template, ok := messages["instructions_template"].(string); ok && copied["base_instructions"] == template {
				delete(copied, "base_instructions")
			}
		}
		copied["supported_in_api"] = false
		catalog = append(catalog, copied)
	}
	return catalog
}

func externalCatalogEntry(providerID string, model config.ExternalModelConfig, priority int, toolMode string) map[string]any {
	entry := wopalCatalogEntry(model, priority, toolMode)
	clientName := providerID + "/" + model.Name
	if alias := strings.TrimSpace(model.Alias); alias != "" {
		clientName = alias
	}
	entry["slug"] = clientName
	entry["display_name"] = providerID + " · " + model.Name
	entry["description"] = providerID + " external model"
	return entry
}

func wopalCatalogEntry(model config.ExternalModelConfig, priority int, toolMode string) map[string]any {
	contextWindow := model.EffectiveContextWindow()
	reasoningLevels := catalogReasoningLevels
	defaultReasoningLevel := "high"
	if !model.SupportsReasoning() {
		reasoningLevels = catalogReasoningLevels[:1]
		defaultReasoningLevel = "none"
	}
	inputModalities := []any{"text"}
	if model.SupportsVision() {
		inputModalities = []any{"text", "image"}
	}

	return map[string]any{
		"base_instructions":                 "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals.",
		"context_window":                    contextWindow,
		"default_reasoning_level":           defaultReasoningLevel,
		"default_reasoning_summary":         "auto",
		"description":                       "Wopal AI (New API) model",
		"display_name":                      "Wopal AI · " + model.Name,
		"effective_context_window_percent":  95,
		"experimental_supported_tools":      []any{},
		"include_apps_usage_instructions":   true,
		"include_plugin_usage_instructions": true,
		"include_skills_usage_instructions": true,
		"input_modalities":                  inputModalities,
		"max_context_window":                contextWindow,
		"priority":                          priority,
		"shell_type":                        "unified_exec",
		// The provider default matches Codex official code-mode models so
		// their exec calls remain available across a model switch.
		"tool_mode":                            toolMode,
		"slug":                                 "wopal-ai/" + model.Name,
		"support_verbosity":                    false,
		"supported_in_api":                     true,
		"supported_reasoning_levels":           reasoningLevels,
		"supports_reasoning_summary_parameter": false,
		"truncation_policy":                    map[string]any{"limit": 10000, "mode": "tokens"},
		"visibility":                           "list",
	}
}

func resolveCodexCommand() (string, error) {
	local := filepath.Join(config.HomeDir(), ".local", "bin", "codex")
	if info, err := os.Stat(local); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
		return local, nil
	}

	command, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("codex command not found: tried %s and codex on PATH: %w", local, err)
	}
	return command, nil
}

func runCodexDebugModels(command string) catalogCommandOutput {
	cmd := exec.Command(command, "debug", "models", "--bundled")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := catalogCommandOutput{
		stdout: stdout.Bytes(),
		stderr: stderr.Bytes(),
		err:    err,
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			result.exitCode = exitError.ExitCode()
		}
	}
	return result
}

func loadBundledNativeCatalog() ([]map[string]any, error) {
	command, err := catalogResolveCodex()
	if err != nil {
		return nil, err
	}
	return loadBundledNativeCatalogFromCommand(command)
}

func loadBundledNativeCatalogFromCommand(command string) ([]map[string]any, error) {
	output := catalogExecuteCodex(command)
	if output.err != nil {
		if output.exitCode > 0 {
			return nil, fmt.Errorf(
				"codex debug models --bundled failed with exit code %d: %s",
				output.exitCode,
				summarizeCatalogStderr(output.stderr),
			)
		}
		if isCodexCommandUnavailable(output.err) {
			return nil, fmt.Errorf("codex command not found: %s: %w", command, output.err)
		}
		return nil, fmt.Errorf("run %s debug models --bundled: %w", command, output.err)
	}

	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(output.stdout, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON from %s debug models --bundled: %w", command, err)
	}
	if len(payload.Models) == 0 {
		return nil, fmt.Errorf("Codex bundled model catalog is empty: %s", command)
	}
	return payload.Models, nil
}

func isCodexCommandUnavailable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var execError *exec.Error
	return errors.As(err, &execError)
}

func summarizeCatalogStderr(stderr []byte) string {
	summary := strings.Join(strings.Fields(string(stderr)), " ")
	if summary == "" {
		return "(no stderr)"
	}
	const maxRunes = 400
	runes := []rune(summary)
	if len(runes) > maxRunes {
		summary = string(runes[:maxRunes]) + "..."
	}
	return summary
}

func writeCatalog(path string, catalog []map[string]any) error {
	payload, err := encodeCatalog(catalog)
	if err != nil {
		return err
	}
	if err := catalogWriteAtomic(path, payload, 0o600); err != nil {
		return fmt.Errorf("write model catalog: %w", err)
	}
	return nil
}

func encodeCatalog(catalog []map[string]any) ([]byte, error) {
	if catalog == nil {
		catalog = []map[string]any{}
	}
	payload, err := json.MarshalIndent(struct {
		Models []map[string]any `json:"models"`
	}{Models: catalog}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode model catalog: %w", err)
	}
	payload = append(payload, '\n')
	return payload, nil
}

func writeCatalogAndBaseline(catalogPath string, catalogPayload []byte, baselinePath string, baselinePayload []byte) error {
	previousCatalog, catalogExisted, err := readOptionalFile(catalogPath)
	if err != nil {
		return fmt.Errorf("snapshot previous model catalog: %w", err)
	}
	if err := catalogWriteAtomic(catalogPath, catalogPayload, 0o600); err != nil {
		return fmt.Errorf("write model catalog: %w", err)
	}
	if err := catalogWriteAtomic(baselinePath, baselinePayload, 0o600); err != nil {
		restoreErr := restoreOptionalFile(catalogPath, previousCatalog, catalogExisted)
		if restoreErr != nil {
			return fmt.Errorf("write official model baseline: %v; additionally failed to restore previous model catalog: %w", err, restoreErr)
		}
		return fmt.Errorf("write official model baseline: %w", err)
	}
	return nil
}

func readOptionalFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func restoreOptionalFile(path string, data []byte, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return catalogWriteAtomic(path, data, 0o600)
}

// RefreshCatalog rebuilds the Codex catalog and official baseline from one
// verified native Codex artifact. H1/H2 protects the catalog read from a
// concurrent Codex upgrade: neither output is committed unless the artifact's
// canonical path and SHA-256 remain identical around `debug models --bundled`.
func RefreshCatalog(cfg *config.GatewayConfig) (*CatalogRefreshResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("refresh model catalog: gateway config is nil")
	}

	if err := cfg.WopalAI.ValidateToolMode(); err != nil {
		return nil, err
	}
	if err := cfg.ValidateProviders(); err != nil {
		return nil, err
	}
	command, err := catalogResolveCodex()
	if err != nil {
		return nil, err
	}
	artifactPath, err := catalogResolveNativeArtifact(command)
	if err != nil {
		return nil, fmt.Errorf("resolve authoritative native Codex artifact: %w", err)
	}
	h1, err := catalogComputeNativeIdentity(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("hash native Codex artifact before bundled catalog: %w", err)
	}
	native, err := loadBundledNativeCatalogFromCommand(artifactPath)
	if err != nil {
		return nil, err
	}
	h2, err := catalogComputeNativeIdentity(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("hash native Codex artifact after bundled catalog: %w", err)
	}
	if !sameNativeArtifactIdentity(h1, h2) {
		return nil, fmt.Errorf("native Codex artifact changed while reading bundled catalog; retry catalog refresh")
	}
	baseline, err := newOfficialModelsBaseline(native, h2)
	if err != nil {
		return nil, err
	}
	officialSlugs := make(map[string]struct{}, len(baseline.Slugs))
	for _, slug := range baseline.Slugs {
		officialSlugs[slug] = struct{}{}
	}

	snapshot, err := LoadModelMetadata(cfg.WopalAI.BaseURL)
	if err != nil {
		return nil, err
	}
	wopalModels := make([]config.ExternalModelConfig, 0, len(cfg.WopalAI.Models))
	for _, model := range cfg.WopalAI.Models {
		resolved, _ := ResolveModel(model, snapshot)
		clientName := "wopal-ai/" + resolved.Name
		if _, collides := officialSlugs[clientName]; collides {
			return nil, fmt.Errorf("client-visible model name %q collides with official model", clientName)
		}
		wopalModels = append(wopalModels, resolved)
	}

	providerIDs := make([]string, 0, len(cfg.Providers))
	for providerID := range cfg.Providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	external := make([]externalCatalogModel, 0)
	for _, providerID := range providerIDs {
		providerCfg := cfg.Providers[providerID]
		providerSnapshot, err := LoadProviderModelMetadata(providerID)
		if err != nil {
			return nil, err
		}
		for _, model := range providerCfg.Models {
			resolved, _ := ResolveProviderModel(model, providerCfg, providerSnapshot)
			if strings.TrimSpace(resolved.Protocol) == "" {
				continue
			}
			clientName := providerID + "/" + resolved.Name
			if alias := strings.TrimSpace(resolved.Alias); alias != "" {
				clientName = alias
			}
			if _, collides := officialSlugs[clientName]; collides {
				return nil, fmt.Errorf("client-visible model name %q collides with official model", clientName)
			}
			external = append(external, externalCatalogModel{ProviderID: providerID, Model: resolved, ToolMode: config.DefaultToolMode})
		}
	}
	catalog := buildCatalogWithExternal(native, wopalModels, cfg.WopalAI.EffectiveToolMode(), external)

	catalogPath := config.CodexGatewayCatalogPath()
	baselinePath := config.OfficialModelsPath()
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o700); err != nil {
		return nil, fmt.Errorf("create Codex directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o700); err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	catalogPayload, err := encodeCatalog(catalog)
	if err != nil {
		return nil, err
	}
	baselinePayload, err := encodeOfficialModelsBaseline(baseline)
	if err != nil {
		return nil, err
	}
	if err := writeCatalogAndBaseline(catalogPath, catalogPayload, baselinePath, baselinePayload); err != nil {
		return nil, err
	}

	nativeHidden := 0
	for _, model := range native {
		if visibility, ok := model["visibility"].(string); ok && visibility == "hide" {
			nativeHidden++
		}
	}
	return &CatalogRefreshResult{
		Native:        len(native),
		NativeVisible: len(native) - nativeHidden,
		NativeHidden:  nativeHidden,
		WopalAI:       len(cfg.WopalAI.Models),
		External:      len(external),
	}, nil
}
