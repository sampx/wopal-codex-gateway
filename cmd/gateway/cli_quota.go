package main

import (
	"fmt"
	"path/filepath"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
)

// runQuotaCommand owns the quota maintenance commands. These run as short-lived
// CLI processes and only touch the persisted state file; the daemon adopts the
// change on its next persist.
func runQuotaCommand(subcommand string, args []string) error {
	switch subcommand {
	case "reset-learning":
		path := filepath.Join(config.StateDir(), "official-quota-state.json")
		cleared, err := resetQuotaLearning(path)
		if err != nil {
			return err
		}
		if logger, logErr := activity.New(config.LogsDir(), config.Logs{}); logErr == nil {
			logger.Write(map[string]any{
				"event": "official_quota_learning_reset", "accounts_cleared": cleared,
			})
			logger.Close()
		}
		fmt.Printf("Quota learning reset: %d account(s) cleared; five_hour=%.0f, weekly=%.0f.\n",
			cleared, officialFiveHourStopPercent, officialWeeklyStopPercent)
		return nil
	}

	return fmt.Errorf("usage: %s quota reset-learning", binaryName)
}
