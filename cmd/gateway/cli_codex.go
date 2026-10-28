package main

import (
	"fmt"

	"wopal-codex-gateway/internal/codex"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/provider"
)

func runCodexCommand(subcommand string, args []string) error {
	switch subcommand {
	case "install":
		cfg, err := config.LoadGatewayConfig(config.ConfigPath())
		if err != nil {
			return err
		}
		resolved, err := resolveRuntimeModels(cfg)
		if err != nil {
			return err
		}
		registry, err := provider.Build(resolved)
		if err != nil {
			return err
		}
		if err := provider.ValidateExplicitRoles(resolved, registry); err != nil {
			return err
		}
		compactionRole := provider.ResolveRole(resolved, registry, provider.RoleCompaction)
		if compactionRole.Status != provider.RoleReady {
			return fmt.Errorf("model_role_unavailable: compaction role is unavailable: %s", compactionRole.Reason)
		}
		result, err := codex.InstallIntoCodex(cfg, compactionRole.ClientModel)
		if err != nil {
			return err
		}
		fmt.Printf("Model catalog: official-visible=%d, official-internal=%d, wopal-ai=%d\n",
			result.NativeVisible, result.NativeHidden, result.WopalAI)
		fmt.Println("Codex configured to use wopal-codex-gateway. Restart ChatGPT/Codex Desktop to reload the model catalog.")
		return nil
	case "restore":
		if err := codex.RestoreCodex(); err != nil {
			return err
		}
		fmt.Println("Codex gateway configuration restored. Restart ChatGPT/Codex Desktop to reload the original configuration.")
		return nil
	default:
		return fmt.Errorf("unknown codex subcommand %q; usage: %s codex install|restore", subcommand, binaryName)
	}
}
