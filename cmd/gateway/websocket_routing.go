package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
)

// Codex can reuse a socket after changing models. The handshake binds this
// gateway socket to one provider and its credentials, so a cross-provider
// request must be retried on a fresh connection before any upstream sees it.
func reconnectForWebsocketRouteChange(conn *websocket.Conn, boundRoute routeKind, payload []byte, logger *activity.Logger) bool {
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
	logger.Write(map[string]any{
		"event": "websocket_route_reconnect", "from": boundRoute, "to": nextRoute, "model": model,
	})
	// A transport close makes Codex retire the socket and replay the full input.
	// Do not return an API error: a 400 would terminate the turn without retrying.
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseServiceRestart, "model route changed; reconnect and resend full input",
	), time.Now().Add(time.Second))
	return true
}

func rewriteQuotaFailoverWebsocketFrame(payload []byte, quotaGuard *officialQuotaGuard, accountID, requestedOfficialModel string, now time.Time) ([]byte, bool, bool) {
	var request map[string]any
	if json.Unmarshal(payload, &request) != nil {
		return payload, false, false
	}
	requestType := strings.TrimSpace(stringOrEmpty(request["type"]))
	if requestType != "response.create" && requestType != "response.append" {
		return payload, false, false
	}
	model := strings.TrimSpace(stringOrEmpty(request["model"]))
	if model == "" {
		model = strings.TrimSpace(requestedOfficialModel)
	}
	if classifyModel(model) != routeOfficial {
		return payload, false, false
	}
	if blocked, _ := quotaGuard.blockedModel(accountID, model, now); !blocked {
		return payload, true, false
	}
	request["model"] = officialQuotaFailoverModel
	rewritten, err := json.Marshal(request)
	if err != nil {
		return payload, false, false
	}
	return rewritten, false, true
}
