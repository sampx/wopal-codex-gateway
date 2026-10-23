package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// blockSDKLogSink replaces the SDK's request-log directory with a regular file.
//
// The SDK always installs a *FileRequestLogger and, on failures, forces an
// error log even when `logging-to-file` is false. Its request-body capture and
// error-log writer both need to create files under the resolved log directory,
// so that directory is replaced by a regular file: every open fails with
// ENOTDIR and the SDK degrades to "no log" instead of writing the body first
// and deleting it later.
//
// The SDK's own no-op request logger is not an option for an external module:
// sdk/logging.RequestLogger names internal/interfaces.ErrorMessage in its
// method set, so it cannot be implemented outside the SDK module.
func blockSDKLogSink(sink string) error {
	if sink == "" {
		return fmt.Errorf("SDK log sink path is required")
	}
	if err := os.MkdirAll(filepath.Dir(sink), 0o700); err != nil {
		return fmt.Errorf("create SDK log parent for %s: %w", sink, err)
	}

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

// installSDKLogProtection pins every log directory the embedded SDK can choose
// to a path whose logs entry is a blocked regular file.
//
// CLIProxyAPI v8.0.29 resolves its request-log directory in this order:
// WRITABLE_PATH (or writable_path) -> <base>/logs, then a writable ./logs,
// then <auth-dir>/logs. Pinning WRITABLE_PATH to our runtime directory makes
// the first branch the only reachable one, independent of the process working
// directory. The auth-dir sink stays in place as defence in depth.
func installSDKLogProtection(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("project root is required")
	}
	writable := filepath.Join(root, "runtime", "cliproxy")
	if err := os.MkdirAll(writable, 0o700); err != nil {
		return "", fmt.Errorf("create CLIProxyAPI writable directory %s: %w", writable, err)
	}
	if err := blockSDKLogSink(filepath.Join(writable, "logs")); err != nil {
		return "", fmt.Errorf("block CLIProxyAPI writable log sink: %w", err)
	}
	if err := blockSDKLogSink(filepath.Join(root, "runtime", "cliproxy-auth", "logs")); err != nil {
		return "", fmt.Errorf("block CLIProxyAPI auth log sink: %w", err)
	}
	if err := os.Setenv("WRITABLE_PATH", writable); err != nil {
		return "", fmt.Errorf("pin CLIProxyAPI writable path to %s: %w", writable, err)
	}
	return requestBodyLoggingState, nil
}

// requestBodyLoggingState is the operator-visible answer to "are prompts being
// written to disk?".
const requestBodyLoggingState = "disabled"
