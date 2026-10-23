package main

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
	wopalKeychainService = "com.wopal.codex-gateway.wopal-ai"
	wopalKeychainAccount = "wopal-ai"
)

// keychainExec is replaceable so tests can exercise argument handling without
// reading from or writing to the user's real Keychain.
var keychainExec = func(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// getWopalAPIKey reads the Wopal AI API key from the macOS Keychain.
func getWopalAPIKey() (string, error) {
	var output bytes.Buffer
	err := keychainExec([]string{
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

// hasWopalAPIKey reports whether a usable key is stored.
func hasWopalAPIKey() bool {
	_, err := getWopalAPIKey()
	return err == nil
}

// setWopalAPIKeyInteractive stores a key using security's interactive prompt.
func setWopalAPIKeyInteractive() error {
	err := keychainExec([]string{
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
