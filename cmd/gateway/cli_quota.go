package main

import (
	"fmt"
	"os"
	"path/filepath"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
)

// runQuotaCommand owns the quota maintenance commands. These run as short-lived
// CLI processes and only touch the persisted state file; the daemon adopts the
// change immediately (see quota.Guard.adoptExternalLearningReset).
func runQuotaCommand(subcommand string, args []string) error {
	switch subcommand {
	case "reset-learning":
		return runQuotaResetLearning()
	}

	return fmt.Errorf("usage: %s quota reset-learning", binaryName)
}

func runQuotaResetLearning() error {
	// Validate the configuration before mutating any state. `quota` reads config
	// only to pick the log rotation policy, but an unsupported or unparsable
	// config must fail every config-reading subcommand, and it must fail before
	// the state file is rewritten.
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return err
	}

	path := filepath.Join(config.StateDir(), "official-quota-state.json")
	cleared, err := quota.ResetLearning(path)
	if err != nil {
		return err
	}

	if logger, logErr := activity.New(config.LogsDir(), cfg.Logs); logErr == nil {
		logger.Write(map[string]any{
			"event": "official_quota_learning_reset", "accounts_cleared": cleared,
		})
		logger.Close()
	} else {
		fmt.Fprintf(os.Stderr, "warning: write quota reset metadata: %v\n", logErr)
	}

	fmt.Printf("Quota learning reset: %d account(s) cleared; five_hour=%.0f, weekly=%.0f.\n",
		cleared, quota.FiveHourStopPercent, quota.WeeklyStopPercent)
	return nil
}
