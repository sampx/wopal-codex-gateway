package proxy

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

// Codex can reuse a socket after changing models. The handshake binds this
// gateway socket to one provider and its credentials, so a cross-provider
// request must be retried on a fresh connection before any upstream sees it.
func reconnectForWebsocketRouteChange(conn *websocket.Conn, boundRoute routing.Kind, payload []byte, logger *activity.Logger) bool {
	var request struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &request) != nil ||
		(request.Type != "response.create" && request.Type != "response.append") {
		return false
	}
	model := strings.TrimSpace(request.Model)
	nextRoute := routing.Classify(model)
	if nextRoute == routing.Unknown || nextRoute == boundRoute {
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

func rewriteQuotaFailoverWebsocketFrame(payload []byte, quotaGuard *quota.Guard, accountID, requestedOfficialModel string, now time.Time) ([]byte, bool, bool) {
	var request map[string]any
	if json.Unmarshal(payload, &request) != nil {
		return payload, false, false
	}
	requestType := strings.TrimSpace(routing.StringOrEmpty(request["type"]))
	if requestType != "response.create" && requestType != "response.append" {
		return payload, false, false
	}
	model := strings.TrimSpace(routing.StringOrEmpty(request["model"]))
	if model == "" {
		model = strings.TrimSpace(requestedOfficialModel)
	}
	if routing.Classify(model) != routing.Official {
		return payload, false, false
	}
	if blocked, _ := quotaGuard.BlockedModel(accountID, model, now); !blocked {
		return payload, true, false
	}
	request["model"] = routing.OfficialQuotaFailoverModel
	rewritten, err := json.Marshal(request)
	if err != nil {
		return payload, false, false
	}
	return rewritten, false, true
}
