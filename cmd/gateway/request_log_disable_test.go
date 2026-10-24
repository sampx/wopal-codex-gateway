package main

import (
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v8/sdk/logging"
)

// TestDisableSDKRequestLoggingReturnsNilLogger pins the one contract this
// design depends on: the SDK only installs its request-logging middleware when
// the factory returns a non-nil logger.
func TestDisableSDKRequestLoggingReturnsNilLogger(t *testing.T) {
	var factory func(*sdkconfig.Config, string) sdklogging.RequestLogger = disableSDKRequestLogging
	if logger := factory(nil, "/tmp/does-not-matter.yaml"); logger != nil {
		t.Fatalf("factory returned %T, want nil so the SDK skips request logging", logger)
	}
}

// TestDisabledRequestLoggingStateIsReported keeps /_status honest: the value it
// exposes must describe the mechanism that is actually installed.
func TestDisabledRequestLoggingStateIsReported(t *testing.T) {
	if requestBodyLoggingState != "disabled" {
		t.Fatalf("requestBodyLoggingState = %q, want disabled", requestBodyLoggingState)
	}
}
