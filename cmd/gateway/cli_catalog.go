package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/codex"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/launchd"
)

var (
	refreshCatalogCommand       = catalog.RefreshCatalog
	recordCatalogExternalModels = codex.RecordExposedExternalModels
	catalogGatewayRunning       = launchd.GatewayRunning
	waitCatalogBaseline         = waitForCatalogBaseline
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
		running := catalogGatewayRunning != nil && catalogGatewayRunning()
		if err := signalGatewayReloadAfterCommit(); err != nil {
			return fmt.Errorf("catalog refreshed but gateway reload failed: %w", err)
		}
		if running {
			if result.Baseline == nil || strings.TrimSpace(result.Baseline.Artifact.SHA256) == "" {
				return fmt.Errorf("catalog refreshed but committed baseline identity is unavailable")
			}
			if err := waitCatalogBaseline(cfg, result.Baseline.Artifact.SHA256); err != nil {
				return fmt.Errorf("catalog refreshed but gateway did not activate the committed baseline: %w", err)
			}
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

func waitForCatalogBaseline(cfg *config.GatewayConfig, expectedSHA string) error {
	if cfg == nil {
		return fmt.Errorf("gateway config is nil")
	}
	expectedSHA = strings.TrimSpace(expectedSHA)
	if expectedSHA == "" {
		return fmt.Errorf("expected baseline sha256 is empty")
	}
	host := strings.TrimSpace(cfg.Listen.Host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	statusURL := "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Listen.Port)) + "/_status"
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		response, err := client.Get(statusURL)
		if err == nil {
			var payload struct {
				Catalog struct {
					Status         string `json:"status"`
					BaselineSHA256 string `json:"baseline_sha256"`
				} `json:"catalog"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&payload)
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 && decodeErr == nil {
				if payload.Catalog.Status == "current" && strings.TrimSpace(payload.Catalog.BaselineSHA256) == expectedSHA {
					return nil
				}
				last = fmt.Sprintf("status=%s baseline_sha256=%s", payload.Catalog.Status, payload.Catalog.BaselineSHA256)
			} else if decodeErr != nil {
				last = decodeErr.Error()
			} else {
				last = response.Status
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			if last == "" {
				last = "status confirmation timed out"
			}
			return fmt.Errorf("wait for active catalog baseline %s: %s", expectedSHA, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
