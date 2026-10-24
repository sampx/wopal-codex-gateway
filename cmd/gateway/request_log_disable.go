package main

import (
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v8/sdk/logging"
)

// requestBodyLoggingState is the operator-visible answer to "are prompts being
// written to disk?".
const requestBodyLoggingState = "disabled"

// disableSDKRequestLogging is passed to sdkapi.WithRequestLoggerFactory and
// returns a nil RequestLogger.
//
// The embedded SDK only installs its request-logging middleware when the
// factory returns a non-nil logger. That middleware is the only thing that
// captures request bodies: with it absent, neither a successful request nor a
// forced error log can spool prompt text to disk. No directory trick, no
// environment pinning, and no startup failure mode is needed.
//
// A no-op logger implementation is not an option: sdk/logging.RequestLogger
// names internal/interfaces.ErrorMessage in its method set, so the interface
// cannot be implemented outside the SDK module. Returning nil is the supported
// way to opt out.
func disableSDKRequestLogging(*sdkconfig.Config, string) sdklogging.RequestLogger {
	return nil
}
