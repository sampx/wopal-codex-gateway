package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Codex can reuse a socket after changing models. The handshake binds this
// gateway socket to one provider and its credentials, so a cross-provider
// request must be retried on a fresh connection before any upstream sees it.
func reconnectForWebsocketRouteChange(conn *websocket.Conn, boundRoute routeKind, payload []byte, logger *activityLogger) bool {
	var request struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &request) != nil ||
		(request.Type != "response.create" && request.Type != "response.append") {
		return false
	}
	model := strings.TrimSpace(request.Model)
	nextRoute := classifyModel(model)
	if nextRoute == routeUnknown || nextRoute == boundRoute {
		return false
	}
	logger.write(map[string]any{
		"event": "websocket_route_reconnect", "from": boundRoute, "to": nextRoute, "model": model,
	})
	// A transport close makes Codex retire the socket and replay the full input.
	// Do not return an API error: a 400 would terminate the turn without retrying.
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseServiceRestart, "model route changed; reconnect and resend full input",
	), time.Now().Add(time.Second))
	return true
}
