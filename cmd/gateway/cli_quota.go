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
// change on its next persist.
func runQuotaCommand(subcommand string, args []string) error {
	switch subcommand {
	case "reset-learning":
		path := filepath.Join(config.StateDir(), "official-quota-state.json")
		cleared, err := quota.ResetLearning(path)
		if err != nil {
			return err
		}
		logConfig, configErr := quotaLogsConfig()
		if configErr != nil {
			fmt.Fprintf(os.Stderr, "warning: using default log rotation: %v\n", configErr)
		}
		if logger, logErr := activity.New(config.LogsDir(), logConfig); logErr == nil {
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

	return fmt.Errorf("usage: %s quota reset-learning", binaryName)
}

// quotaLogsConfig keeps the short-lived reset command on the same rotation
// policy as the daemon it reports against.
func quotaLogsConfig() (config.Logs, error) {
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return config.Logs{}, err
	}
	return cfg.Logs, nil
}
