package main

import "fmt"

func runCodexCommand(subcommand string, args []string) error {
	switch subcommand {
	case "install":
		cfg, err := loadGatewayConfig(configPath())
		if err != nil {
			return err
		}
		result, err := installIntoCodex(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("Model catalog: official-visible=%d, official-internal=%d, wopal-ai=%d\n",
			result.NativeVisible, result.NativeHidden, result.WopalAI)
		fmt.Println("Codex configured to use wopal-codex-gateway. Restart ChatGPT/Codex Desktop to reload the model catalog.")
		return nil
	case "restore":
		if err := restoreCodex(); err != nil {
			return err
		}
		fmt.Println("Codex gateway configuration restored. Restart ChatGPT/Codex Desktop to reload the original configuration.")
		return nil
	default:
		return fmt.Errorf("unknown codex subcommand %q; usage: %s codex install|restore", subcommand, binaryName)
	}
}
