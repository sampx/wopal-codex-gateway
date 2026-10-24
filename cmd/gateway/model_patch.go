package main

import (
	"fmt"

	"wopal-codex-gateway/internal/catalog"
)

// parseModelPatch keeps omitted values absent in user configuration.
func parseModelPatch(args []string, allowReset bool) (catalog.ModelPatch, error) {
	var patch catalog.ModelPatch
	for _, capability := range []string{"vision", "reasoning"} {
		if hasFlag(args, "--"+capability) && hasFlag(args, "--no-"+capability) {
			return patch, fmt.Errorf("Choose only one of --%s or --no-%s", capability, capability)
		}
	}
	if hasFlag(args, "--context-window") {
		n, found, err := numberOption(args, "--context-window")
		if err != nil {
			return patch, err
		}
		if !found {
			return patch, fmt.Errorf("--context-window must be a positive integer")
		}
		patch.ContextWindow = &n
	}
	for _, item := range []struct {
		name  string
		field **bool
	}{{"vision", &patch.Vision}, {"reasoning", &patch.Reasoning}} {
		if hasFlag(args, "--"+item.name) || hasFlag(args, "--no-"+item.name) {
			v := hasFlag(args, "--"+item.name)
			*item.field = &v
		}
	}
	patch.ResetContextWindow = hasFlag(args, "--auto") || hasFlag(args, "--reset-context-window")
	patch.ResetVision = hasFlag(args, "--auto") || hasFlag(args, "--reset-vision")
	patch.ResetReasoning = hasFlag(args, "--auto") || hasFlag(args, "--reset-reasoning")
	if !allowReset && (patch.ResetContextWindow || patch.ResetVision || patch.ResetReasoning) {
		return patch, fmt.Errorf("reset options require models set")
	}
	if (patch.ResetContextWindow && patch.ContextWindow != nil) ||
		(patch.ResetVision && patch.Vision != nil) || (patch.ResetReasoning && patch.Reasoning != nil) {
		return patch, fmt.Errorf("cannot set and reset the same capability")
	}
	return patch, nil
}
