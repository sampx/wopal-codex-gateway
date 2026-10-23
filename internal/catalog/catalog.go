package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"wopal-codex-gateway/internal/config"
)

// CatalogRefreshResult summarises one catalog rebuild.
type CatalogRefreshResult struct {
	Native        int
	NativeVisible int
	NativeHidden  int
	WopalAI       int
}

type catalogCommandOutput struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

var (
	catalogResolveCodex = resolveCodexCommand
	catalogExecuteCodex = runCodexDebugModels
)

var catalogReasoningLevels = []any{
	map[string]any{"effort": "none", "description": "Turn reasoning off"},
	map[string]any{"effort": "low", "description": "Fast responses with lighter reasoning"},
	map[string]any{"effort": "medium", "description": "Balanced reasoning depth"},
	map[string]any{"effort": "high", "description": "Greater reasoning depth for complex work"},
}

func buildCatalog(native []map[string]any, wopalModels []config.ExternalModelConfig) []map[string]any {
	catalog := make([]map[string]any, 0, len(wopalModels)+len(native))
	for index, model := range wopalModels {
		catalog = append(catalog, wopalCatalogEntry(model, -200+index))
	}
	for _, model := range native {
		copied := make(map[string]any, len(model))
		for key, value := range model {
			copied[key] = value
		}
		copied["supported_in_api"] = false
		catalog = append(catalog, copied)
	}
	return catalog
}

func wopalCatalogEntry(model config.ExternalModelConfig, priority int) map[string]any {
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
		"additional_speed_tiers":               []any{},
		"apply_patch_tool_type":                nil,
		"auto_compact_token_limit":             nil,
		"availability_nux":                     nil,
		"base_instructions":                    "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals.",
		"context_window":                       contextWindow,
		"default_reasoning_level":              defaultReasoningLevel,
		"default_reasoning_summary":            "auto",
		"default_service_tier":                 nil,
		"default_verbosity":                    nil,
		"description":                          "Wopal AI (New API) model",
		"display_name":                         "Wopal AI · " + model.Name,
		"effective_context_window_percent":     95,
		"experimental_supported_tools":         []any{},
		"include_apps_usage_instructions":      true,
		"include_plugin_usage_instructions":    true,
		"include_skills_usage_instructions":    true,
		"input_modalities":                     inputModalities,
		"max_context_window":                   contextWindow,
		"model_messages":                       nil,
		"priority":                             priority,
		"service_tiers":                        []any{},
		"shell_type":                           "unified_exec",
		"slug":                                 "wopal-ai/" + model.Name,
		"support_verbosity":                    false,
		"supported_in_api":                     true,
		"supported_reasoning_levels":           reasoningLevels,
		"supports_image_detail_original":       false,
		"supports_parallel_tool_calls":         true,
		"supports_reasoning_summaries":         false,
		"supports_reasoning_summary_parameter": false,
		"supports_search_tool":                 false,
		"truncation_policy":                    map[string]any{"limit": 10000, "mode": "tokens"},
		"upgrade":                              nil,
		"visibility":                           "list",
		"web_search_tool_type":                 "text",
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
	if catalog == nil {
		catalog = []map[string]any{}
	}
	payload, err := json.MarshalIndent(struct {
		Models []map[string]any `json:"models"`
	}{Models: catalog}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model catalog: %w", err)
	}
	payload = append(payload, '\n')

	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create model catalog temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set model catalog permissions: %w", err)
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write model catalog: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync model catalog: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close model catalog: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace model catalog: %w", err)
	}
	return nil
}

// RefreshCatalog rebuilds ~/.codex/wopal-codex-gateway-models.json from the
// bundled Codex baseline plus the configured Wopal AI models.
func RefreshCatalog(cfg *config.GatewayConfig) (*CatalogRefreshResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("refresh model catalog: gateway config is nil")
	}

	native, err := loadBundledNativeCatalog()
	if err != nil {
		return nil, err
	}
	catalog := buildCatalog(native, cfg.WopalAI.Models)

	path := config.CodexGatewayCatalogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create Codex directory: %w", err)
	}
	if err := writeCatalog(path, catalog); err != nil {
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
	}, nil
}
