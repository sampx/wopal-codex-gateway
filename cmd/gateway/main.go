package main

import (
	"fmt"
	"os"
	"strings"

	"wopal-codex-gateway/internal/config"
)

const binaryName = config.BinaryName

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binaryName, err)
		os.Exit(1)
	}
}

// run parses the subcommand group and hands the remaining arguments to the
// handler that owns it. Every group is provided by this single binary.
func run(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}

	command, rest := args[0], args[1:]
	switch command {
	case "help", "--help", "-h":
		printHelp()
		return nil
	case "serve":
		return runServe(rest)
	case "config", "auth", "models", "catalog", "codex", "service", "quota":
		return runCommandGroup(command, rest)
	}

	return fmt.Errorf("unknown command: %s", strings.Join(args, " "))
}

func runCommandGroup(group string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s %s <subcommand>; run '%s help' for the command list", binaryName, group, binaryName)
	}

	subcommand, rest := args[0], args[1:]
	switch group {
	case "config":
		return runConfigCommand(subcommand, rest)
	case "auth":
		return runAuthCommand(subcommand, rest)
	case "models":
		return runModelsCommand(subcommand, rest)
	case "catalog":
		return runCatalogCommand(subcommand, rest)
	case "codex":
		return runCodexCommand(subcommand, rest)
	case "service":
		return runServiceCommand(subcommand, rest)
	case "quota":
		return runQuotaCommand(subcommand, rest)
	}

	return fmt.Errorf("unknown command group: %s", group)
}

func printHelp() {
	fmt.Print(helpText)
}

const helpText = `wopal-codex-gateway

Commands:
  serve                                                                       Run the gateway daemon
  config show                                                                 Print the gateway configuration
  config wopal --base-url <url> [--model <name>...] [--context-window 262144] [--vision] [--no-reasoning]
  auth set                                                                    Store and validate the Wopal AI API key
  auth test                                                                   Validate the stored Wopal AI API key
  auth status                                                                 Show whether a key is configured
  models list                                                                 List models visible to the Wopal AI API key
  models sync                                                                 Import all Codex-usable Wopal AI models
  models add <model...> [--context-window 262144] [--vision] [--no-reasoning]
  models set <model...> [--context-window N] [--vision|--no-vision] [--reasoning|--no-reasoning]
  models remove <model...>                                                    Remove Wopal AI models and exclude them from sync
  catalog refresh                                                             Rebuild the Codex model catalog
  codex install                                                               Install the gateway into Codex
  codex restore                                                               Restore the pre-install Codex configuration
  service install                                                             Install and start the LaunchAgent
  service uninstall                                                           Unload and remove the LaunchAgent
  service start                                                               Start the installed proxy service
  service stop                                                                Stop the proxy service
  service restart                                                             Restart the proxy service
  service status                                                              Show install/load/health status
  quota reset-learning                                                        Clear the learned max quota step

Routing:
  wopal-ai/<model>   -> Wopal AI / New API
  unnamespaced model -> native ChatGPT/OpenAI
`
