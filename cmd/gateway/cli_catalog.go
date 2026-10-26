package main

import (
	"fmt"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/codex"
	"wopal-codex-gateway/internal/config"
)

var (
	refreshCatalogCommand       = catalog.RefreshCatalog
	recordCatalogExternalModels = codex.RecordExposedExternalModels
)

func runCatalogCommand(subcommand string, args []string) error {
	switch subcommand {
	case "refresh":
		cfg, err := config.LoadGatewayConfig(config.ConfigPath())
		if err != nil {
			return err
		}
		result, err := refreshCatalogCommand(cfg)
		if err != nil {
			return err
		}
		if recordCatalogExternalModels != nil {
			if err := recordCatalogExternalModels(result.ExternalNames); err != nil {
				return fmt.Errorf("record exposed external models: %w", err)
			}
		}
		if err := signalGatewayReloadAfterCommit(); err != nil {
			return fmt.Errorf("catalog refreshed but gateway reload failed: %w", err)
		}
		fmt.Printf(
			"Model catalog: official-visible=%d, official-internal=%d, wopal-ai=%d\n",
			result.NativeVisible,
			result.NativeHidden,
			result.WopalAI,
		)
		return nil
	default:
		return fmt.Errorf("unknown catalog subcommand %q; usage: %s catalog refresh", subcommand, binaryName)
	}
}
