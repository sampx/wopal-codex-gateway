package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
	"wopal-codex-gateway/internal/launchd"
)

// InstallIntoCodex points Codex at the gateway and records the pre-install state.
func InstallIntoCodex(cfg *config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gateway configuration is missing")
	}
	if strings.TrimSpace(cfg.WopalAI.BaseURL) == "" {
		return nil, fmt.Errorf("Wopal AI base URL is not configured. Run: wopal-codex-gateway config wopal --base-url <url>")
	}
	if !keychain.HasWopalAPIKey() {
		return nil, fmt.Errorf("Wopal AI API key is not configured. Run: wopal-codex-gateway auth set")
	}

	refresh, err := catalog.RefreshCatalog(cfg)
	if err != nil {
		return nil, err
	}
	if err := launchd.RequireGatewayBinary(); err != nil {
		return nil, err
	}
	if err := launchd.InstallLaunchAgent(); err != nil {
		return nil, err
	}
	if err := launchd.WaitForHealth(cfg.Listen.Host, cfg.Listen.Port); err != nil {
		return nil, err
	}

	configFile := config.CodexConfigPath()
	currentText, err := readCodexFile(configFile)
	if err != nil {
		return nil, err
	}
	gatewayBaseURL := fmt.Sprintf("http://%s:%d/v1", cfg.Listen.Host, cfg.Listen.Port)
	currentState := captureRestoreState(currentText)
	alreadyManaged := currentState.OpenAIBaseURL.Value == gatewayBaseURL &&
		currentState.ModelCatalogJSON.Value == config.CodexGatewayCatalogPath()

	if !alreadyManaged {
		if err := writeCodexRestoreState(currentState); err != nil {
			return nil, err
		}
	} else {
		saved, found, err := readCodexRestoreState()
		if err != nil {
			return nil, err
		}
		if found {
			merged := mergeGatewayRestoreState(saved, currentState)
			savedJSON, err := json.Marshal(saved)
			if err != nil {
				return nil, fmt.Errorf("encode saved restore state: %w", err)
			}
			mergedJSON, err := json.Marshal(merged)
			if err != nil {
				return nil, fmt.Errorf("encode merged restore state: %w", err)
			}
			if !bytes.Equal(savedJSON, mergedJSON) {
				if err := writeCodexRestoreState(merged); err != nil {
					return nil, err
				}
			}
		}
	}

	nextText := applyGatewayConfig(currentText, config.CodexGatewayCatalogPath(), gatewayBaseURL)
	if err := writeCodexFileAtomic(configFile, nextText); err != nil {
		return nil, err
	}
	return refresh, nil
}

// RestoreCodex undoes `codex install` and stops the gateway service.
func RestoreCodex() error {
	state, found, err := readCodexRestoreState()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("codex gateway is not installed: restore state not found at %s; run: wopal-codex-gateway codex install", config.RestoreStatePath())
	}

	configFile := config.CodexConfigPath()
	currentText, err := readCodexFile(configFile)
	if err != nil {
		return err
	}
	if err := writeCodexFileAtomic(configFile, restoreGatewayConfig(currentText, state)); err != nil {
		return err
	}
	if err := launchd.UninstallLaunchAgent(); err != nil {
		return err
	}
	if err := removeCodexFileIfExists(config.RestoreStatePath()); err != nil {
		return err
	}
	if err := removeCodexFileIfExists(config.CodexGatewayCatalogPath()); err != nil {
		return err
	}
	return nil
}

func readCodexRestoreState() (CodexRestoreState, bool, error) {
	data, err := os.ReadFile(config.RestoreStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return CodexRestoreState{}, false, nil
	}
	if err != nil {
		return CodexRestoreState{}, false, fmt.Errorf("read codex restore state: %w", err)
	}
	var state CodexRestoreState
	if err := json.Unmarshal(data, &state); err != nil {
		return CodexRestoreState{}, false, fmt.Errorf("parse codex restore state %s: %w", config.RestoreStatePath(), err)
	}
	return state, true, nil
}

func writeCodexRestoreState(state CodexRestoreState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode codex restore state: %w", err)
	}
	return writeCodexFileAtomic(config.RestoreStatePath(), string(append(data, '\n')))
}

func readCodexFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

func removeCodexFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// writeCodexFileAtomic writes through a private temporary file in the target
// directory, then renames it into place so readers never see a partial file.
func writeCodexFileAtomic(path, content string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", directory, err)
	}
	temp, err := os.CreateTemp(directory, filepath.Base(path)+".wopal-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("set permissions on %s: %w", tempPath, err)
	}
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
