package keychain

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	wopalKeychainService    = "com.wopal.codex-gateway.wopal-ai"
	wopalKeychainAccount    = "wopal-ai"
	providerKeychainService = "com.wopal.codex-gateway.providers"
)

// KeychainExec is replaceable so tests can exercise argument handling without
// reading from or writing to the user's real Keychain.
var KeychainExec = func(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// GetWopalAPIKey reads the Wopal AI API key from the macOS Keychain.
func GetWopalAPIKey() (string, error) {
	var output bytes.Buffer
	err := KeychainExec([]string{
		"find-generic-password",
		"-a", wopalKeychainAccount,
		"-s", wopalKeychainService,
		"-w",
	}, nil, &output, io.Discard)
	key := strings.TrimSpace(output.String())
	if err != nil || key == "" {
		return "", errors.New("Wopal AI API key is not configured. Run: wopal-codex-gateway auth set")
	}
	return key, nil
}

// HasAPIKeyHook is a seam so install flows can be proven without a real
// Keychain entry. Production always uses HasWopalAPIKey.
var HasAPIKeyHook = HasWopalAPIKey

// HasWopalAPIKey reports whether a usable key is stored.
func HasWopalAPIKey() bool {
	_, err := GetWopalAPIKey()
	return err == nil
}

// SetWopalAPIKeyInteractive stores a key using security's interactive prompt.
func SetWopalAPIKeyInteractive() error {
	err := KeychainExec([]string{
		"add-generic-password",
		"-a", wopalKeychainAccount,
		"-s", wopalKeychainService,
		"-U",
		"-w",
	}, os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		return nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return fmt.Errorf("security exited with code %d", exitError.ExitCode())
	}
	return fmt.Errorf("run security: %w", err)
}

// GetProviderAPIKey reads a provider credential. wopal-ai intentionally keeps
// its legacy service/account so existing installations continue to work.
func GetProviderAPIKey(providerID string) (string, error) {
	if providerID == wopalKeychainAccount {
		return GetWopalAPIKey()
	}
	var output bytes.Buffer
	err := KeychainExec([]string{
		"find-generic-password",
		"-a", providerID,
		"-s", providerKeychainService,
		"-w",
	}, nil, &output, io.Discard)
	key := strings.TrimSpace(output.String())
	if err != nil || key == "" {
		return "", fmt.Errorf("%s API key is not configured. Run: wopal-codex-gateway auth set --provider %s", providerID, providerID)
	}
	return key, nil
}

func HasProviderAPIKey(providerID string) bool {
	if providerID == wopalKeychainAccount {
		return HasWopalAPIKey()
	}
	err := KeychainExec([]string{
		"find-generic-password",
		"-a", providerID,
		"-s", providerKeychainService,
	}, nil, io.Discard, io.Discard)
	return err == nil
}

func SetProviderAPIKeyInteractive(providerID string) error {
	if providerID == wopalKeychainAccount {
		return SetWopalAPIKeyInteractive()
	}
	err := KeychainExec([]string{
		"add-generic-password",
		"-a", providerID,
		"-s", providerKeychainService,
		"-U",
		"-w",
	}, os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return fmt.Errorf("security exited with code %d", exitError.ExitCode())
	}
	return fmt.Errorf("run security: %w", err)
}
