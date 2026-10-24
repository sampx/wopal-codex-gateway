package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

func (runner wopalModelsRunner) runInspect(args []string) error {
	cfg, err := runner.loadConfig()
	if err != nil {
		return err
	}
	var snapshot *catalog.ModelMetadataSnapshot
	if runner.loadMetadata != nil {
		snapshot, err = runner.loadMetadata(cfg.WopalAI.BaseURL)
		if err != nil {
			return err
		}
	}
	names := positionals(args)
	for _, name := range names {
		found := false
		for _, m := range cfg.WopalAI.Models {
			if m.Name == stripConfigModelPrefix(name) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Model not configured: %s", name)
		}
	}
	for _, m := range cfg.WopalAI.Models {
		if len(names) > 0 {
			selected := false
			for _, name := range names {
				selected = selected || m.Name == stripConfigModelPrefix(name)
			}
			if !selected {
				continue
			}
		}
		resolved, sources := catalog.ResolveModel(m, snapshot)
		runner.printf("wopal-ai/%s  context=%d [%s]  vision=%t [%s]  reasoning=%t [%s]\n",
			m.Name, resolved.EffectiveContextWindow(), sources.ContextWindow,
			resolved.SupportsVision(), sources.Vision, resolved.SupportsReasoning(), sources.Reasoning)
	}
	runner.printf("Tool mode: %s\n", cfg.WopalAI.EffectiveToolMode())
	return runner.warnCodexContextOverrides()
}

// Only report the two root-level keys; never display unrelated Codex config.
func (runner wopalModelsRunner) warnCodexContextOverrides() error {
	file, err := os.Open(config.CodexConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex context settings: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if strings.HasPrefix(line, "[") {
			break
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "model_context_window" || key == "model_auto_compact_token_limit" {
			runner.printf("Notice: Codex global %s is set and can override per-model behavior; preserved unchanged.\n", key)
		}
	}
	return scanner.Err()
}
