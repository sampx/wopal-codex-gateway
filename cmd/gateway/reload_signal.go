package main

import "wopal-codex-gateway/internal/launchd"

var signalGatewayReload = launchd.SignalReloadIfRunning

func signalGatewayReloadAfterCommit() error {
	if signalGatewayReload == nil {
		return nil
	}
	return signalGatewayReload()
}
