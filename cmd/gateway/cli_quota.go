package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
)

func runQuotaCommand(subcommand string, args []string) error {
	switch subcommand {
	case "protection":
		return runQuotaProtection(args)
	default:
		return fmt.Errorf("unknown quota subcommand: %s", subcommand)
	}
}

// runQuotaProtection reads or writes the operator switch that suspends the
// fixed official stop points. The daemon picks the change up on its next quota
// decision, so no restart or reload is involved.
func runQuotaProtection(args []string) error {
	path := config.QuotaProtectionPath()
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return fmt.Errorf("usage: %s quota protection [on|off|status]", binaryName)
		}
		state, exists, err := quota.ReadProtectionState(path)
		if err != nil {
			return err
		}
		payload := protectionPayload(state, exists)
		payload["source"] = "default"
		payload["action"] = "status"
		if exists {
			payload["source"] = "override"
		}
		return printQuotaPayload(payload)
	}

	switch args[0] {
	case "on", "off":
		if len(args) > 1 {
			return fmt.Errorf("usage: %s quota protection [on|off|status]", binaryName)
		}
		enabled := args[0] == "on"
		state, err := quota.WriteProtectionState(path, enabled, time.Now())
		if err != nil {
			return err
		}
		payload := protectionPayload(state, true)
		payload["source"] = "override"
		payload["action"] = args[0]
		return printQuotaPayload(payload)
	default:
		return fmt.Errorf("unknown quota protection action: %s (expected on, off or status)", strings.TrimSpace(args[0]))
	}
}

func protectionPayload(state quota.ProtectionState, exists bool) map[string]any {
	enabled := true
	payload := map[string]any{"protection": "enabled"}
	if exists {
		enabled = state.Enabled
	}
	if !enabled {
		payload["protection"] = "disabled"
	}
	if !state.UpdatedAt.IsZero() {
		payload["updated_at"] = state.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return payload
}

func printQuotaPayload(payload map[string]any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quota output: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
