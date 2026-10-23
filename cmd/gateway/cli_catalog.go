package main

import "fmt"

func runCatalogCommand(subcommand string, args []string) error {
	switch subcommand {
	case "refresh":
		cfg, err := loadGatewayConfig(configPath())
		if err != nil {
			return err
		}
		result, err := refreshCatalog(cfg)
		if err != nil {
			return err
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
