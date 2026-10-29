package main

import (
	"errors"
	"flag"
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

	// Every command group accepts --project-root so a CLI invocation always
	// operates on the runtime the operator named, not on whatever the binary
	// happens to sit next to. `serve` parses it through its own FlagSet.
	if command != "serve" {
		var err error
		if rest, err = applyProjectRootFlag(rest); err != nil {
			return err
		}
	}

	switch command {
	case "help", "--help", "-h":
		printHelp()
		return nil
	case "serve":
		// serve parses its own flags, so "serve --help" reaches it as
		// flag.ErrHelp. That is a help request, not a failure.
		if err := runServe(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		return nil
	case "config", "providers", "auth", "models", "catalog", "codex", "service":
		if requestsHelp(rest) {
			printHelp()
			return nil
		}
		return runCommandGroup(command, rest)
	}

	return fmt.Errorf("unknown command: %s", strings.Join(args, " "))
}

// valueOptions are the flags that consume their next argument. Help detection
// has to skip that argument: "--model -h" names a model, it does not ask for
// help.
var valueOptions = map[string]bool{
	"--provider":       true,
	"--model":          true,
	"--alias":          true,
	"--protocol":       true,
	"--context-window": true,
	"--base-url":       true,
	"--base-url-for":   true,
	"--models-url":     true,
	"--models-auth":    true,
	"--header":         true,
	"--tool-mode":      true,
	"--from":           true,
	"--to":             true,
}

// requestsHelp reports whether the operator asked for help inside a command
// group. Without it "providers --help" fails as an unknown subcommand and
// "models add --help" fails as bad usage, which reads like the binary has no
// help flag at all. Two positions are deliberately not help requests: a
// "help" that is not the group's own subcommand, and any token bound as a
// value to a preceding option. Both name data, not a request.
func requestsHelp(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if valueOptions[arg] {
			index++
			continue
		}
		switch arg {
		case "-h", "--help":
			return true
		case "help":
			if index == 0 {
				return true
			}
		}
	}
	return false
}

func runCommandGroup(group string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s %s <subcommand>; run '%s help' for the command list", binaryName, group, binaryName)
	}

	subcommand, rest := args[0], args[1:]
	switch group {
	case "config":
		return runConfigCommand(subcommand, rest)
	case "providers":
		return runProvidersCommand(subcommand, rest)
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
	}

	return fmt.Errorf("unknown command group: %s", group)
}

// applyProjectRootFlag consumes --project-root <dir> from a command group and
// pins it for the rest of the process. The flag is removed from the argument
// list so subcommands keep their existing parsers.
func applyProjectRootFlag(args []string) ([]string, error) {
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] != "--project-root" {
			remaining = append(remaining, args[index])
			continue
		}
		if index+1 >= len(args) {
			return nil, fmt.Errorf("--project-root requires a directory")
		}
		if err := config.SetProjectRoot(args[index+1]); err != nil {
			return nil, err
		}
		index++
	}
	return remaining, nil
}

func printHelp() {
	fmt.Print(helpText)
}

const helpText = `wopal-codex-gateway

Commands:
  help, -h, --help                                                            Print this list (also accepted after any command)
  serve --project-root <dir>                                                  Run the gateway daemon
  config migrate --from <existing.json> --to <new.json>                         Prepare a separate version-1 config
  config show                                                                 Print the gateway configuration
  config wopal [--base-url <url>] [--tool-mode code_mode_only|code_mode|direct] [--model <name>...] [--context-window N] [--vision] [--no-reasoning]
  providers set <provider> --base-url <url> [--base-url-for <protocol>=<url>]... [--models-url <url>] [--models-auth none|provider-key] [--header <name>=<value>]... [--protocol <default>] [--tool-mode <mode>]
  providers remove <provider>                                                   Remove an unreferenced external provider
  auth set [--provider <id>]                                                  Store and validate a provider API key
  auth test [--provider <id>]                                                 Validate a stored provider API key
  auth status [--provider <id>]                                               Show whether a provider key is configured
  models list [--provider <id>]                                                List available models; provider output includes protocol source
  models inspect [model...] [--provider <id>]                                 Show effective capabilities and sources
  models sync [--provider <id>]                                                Refresh Wopal config or external provider metadata
  models add <model...> [--provider <id>] [--alias <name>] [--protocol <protocol>] [--context-window N] [--vision] [--no-reasoning]
  models set <model...> [--provider <id>] [--alias <name>] [--protocol <protocol>] [--context-window N] [--vision|--no-vision] [--reasoning|--no-reasoning] [--auto|--reset-context-window|--reset-vision|--reset-reasoning]
  models remove <model...> [--provider <id>]                                  Remove configured models; Wopal removal also excludes from sync
  catalog refresh                                                             Rebuild the Codex model catalog
  codex install                                                               Install the gateway into Codex
  codex restore                                                               Restore the pre-install Codex configuration
  service install                                                             Install and start the LaunchAgent
  service uninstall                                                           Unload and remove the LaunchAgent
  service start                                                               Start the installed proxy service
  service stop                                                                Stop the proxy service
  service restart                                                             Restart the proxy service
  service status                                                              Show install/load/health status

Routing:
  wopal-ai/<model>   -> Wopal AI / New API
  unnamespaced model -> native ChatGPT/OpenAI
`
