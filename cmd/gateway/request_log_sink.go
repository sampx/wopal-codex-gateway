package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// blockSDKRequestLogDir makes the embedded CLIProxyAPI unable to persist any
// request artifact.
//
// The SDK always installs a *FileRequestLogger and, on failures, forces an
// error log even when `logging-to-file` is false. Its request-body capture and
// error-log writer both need to create files under <auth-dir>/logs, so that
// path is replaced by a regular file: every open fails with ENOTDIR and the SDK
// degrades to "no log" instead of writing the body first and deleting it later.
//
// The SDK's own no-op request logger is not an option for an external module:
// sdk/logging.RequestLogger names internal/interfaces.ErrorMessage in its
// method set, so it cannot be implemented outside the SDK module.
func blockSDKRequestLogDir(authDir string) error {
	if authDir == "" {
		return fmt.Errorf("auth directory is required")
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return fmt.Errorf("create CLIProxyAPI auth dir: %w", err)
	}

	sink := filepath.Join(authDir, "logs")
	info, err := os.Lstat(sink)
	switch {
	case err == nil && info.IsDir():
		// Only ever remove the directory this function owns; anything else is
		// left alone so a surprising layout fails loudly instead of being wiped.
		if err := os.RemoveAll(sink); err != nil {
			return fmt.Errorf("clear CLIProxyAPI log directory %s: %w", sink, err)
		}
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("CLIProxyAPI log sink %s is not a regular file", sink)
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("inspect CLIProxyAPI log sink %s: %w", sink, err)
	}

	descriptor, err := os.OpenFile(sink, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create CLIProxyAPI log sink %s: %w", sink, err)
	}
	closeErr := descriptor.Close()
	if closeErr != nil {
		return fmt.Errorf("close CLIProxyAPI log sink %s: %w", sink, closeErr)
	}
	return nil
}

// requestBodyLoggingState is the operator-visible answer to "are prompts being
// written to disk?".
const requestBodyLoggingState = "disabled"
