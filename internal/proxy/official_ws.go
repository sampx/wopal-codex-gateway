package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

// Official websocket passthrough stays transparent for native traffic while
// keeping gateway-issued compaction capsules and Wopal reasoning items out of
// OpenAI-bound frames. OpenAI verifies their encrypted content and rejects
// unknown payloads, which fails closed browser and computer-use auto review
// after a Wopal compaction or a mid-session switch to an official model.

const (
	officialWebsocketFrameLimit = 64 << 20
	officialWebsocketWriteWait  = 30 * time.Second
	officialWebsocketDrainWait  = 2 * time.Second
)

var officialWebsocketUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func serveOfficialWebsocketBridge(c *gin.Context, logger *activity.Logger, target *url.URL, quotaGuard *quota.Guard, accountID, boundModel string, nativeReplay *compaction.NativeBridge) {
	serveOfficialWebsocketBridgeWithResolver(c, logger, target, quotaGuard, accountID, boundModel, nativeReplay, legacyRouteResolver)
}

func serveOfficialWebsocketBridgeWithResolver(c *gin.Context, logger *activity.Logger, target *url.URL, quotaGuard *quota.Guard, accountID, boundModel string, nativeReplay *compaction.NativeBridge, resolve RouteResolver) {
	serveOfficialWebsocketBridgeWithRuntime(c, logger, target, quotaGuard, accountID, boundModel, nativeReplay, resolve, defaultRuntimeRoles())
}

func serveOfficialWebsocketBridgeWithRuntime(c *gin.Context, logger *activity.Logger, target *url.URL, quotaGuard *quota.Guard, accountID, boundModel string, nativeReplay *compaction.NativeBridge, resolve RouteResolver, roles RuntimeRoles) {
	// Dial upstream before upgrading the client so a failed dial still returns
	// a clean HTTP error instead of a half-open websocket.
	upstreamConn, upstreamResponse, err := dialOfficialWebsocketUpstream(c, target)
	if err != nil {
		logger.Write(map[string]any{"event": "official_ws_dial_failed", "upstream": target.Host, "error": err.Error()})
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	// Preserve upstream handshake response headers the native transport relied
	// on, then let gorilla own the websocket-specific ones.
	responseHeader := http.Header{}
	if upstreamResponse != nil {
		quotaGuard.ObserveHeaders(accountID, upstreamResponse.Header, time.Now())
		responseHeader = upstreamResponse.Header.Clone()
	}
	for _, name := range []string{
		"Connection", "Upgrade", "Sec-WebSocket-Accept",
		"Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
	} {
		responseHeader.Del(name)
	}
	if subprotocol := upstreamConn.Subprotocol(); subprotocol != "" {
		responseHeader.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	clientConn, err := officialWebsocketUpgrader.Upgrade(c.Writer, c.Request, responseHeader)
	if err != nil {
		logger.Write(map[string]any{"event": "official_ws_upgrade_failed", "error": err.Error()})
		_ = upstreamConn.Close()
		return
	}
	clientConn.SetReadLimit(officialWebsocketFrameLimit)
	upstreamConn.SetReadLimit(officialWebsocketFrameLimit)
	logger.Write(map[string]any{
		"event":       "official_ws_connected",
		"upstream":    target.Host,
		"subprotocol": upstreamConn.Subprotocol(),
	})

	done := make(chan struct{}, 2)
	clientRequest := c.Request.Clone(c.Request.Context())
	var clientWriteMu sync.Mutex
	var upstreamWriteMu sync.Mutex
	go pumpOfficialWebsocket(clientConn, upstreamConn, done, true, logger, quotaGuard, accountID, boundModel, nativeReplay, clientRequest, &clientWriteMu, &upstreamWriteMu, resolve, roles)
	go pumpOfficialWebsocket(upstreamConn, clientConn, done, false, logger, quotaGuard, accountID, boundModel, nil, nil, &upstreamWriteMu, &clientWriteMu, nil, roles)

	<-done
	_ = clientConn.Close()
	_ = upstreamConn.Close()
	select {
	case <-done:
	case <-time.After(officialWebsocketDrainWait):
	}
}

// pumpOfficialWebsocket copies frames one direction. Each connection has exactly
// one reader and one writer goroutine, which websocket.Conn requires.
func pumpOfficialWebsocket(source, target *websocket.Conn, done chan struct{}, rewriteClientFrames bool, logger *activity.Logger, quotaGuard *quota.Guard, accountID, boundModel string, nativeReplay *compaction.NativeBridge, clientRequest *http.Request, sourceWriteMu, targetWriteMu *sync.Mutex, resolve RouteResolver, roles RuntimeRoles) {
	defer func() { done <- struct{}{} }()
	for {
		messageType, payload, err := source.ReadMessage()
		if err != nil {
			fields := map[string]any{"event": "official_ws_closed", "client_to_upstream": rewriteClientFrames}
			if closeErr, ok := err.(*websocket.CloseError); ok {
				fields["code"] = closeErr.Code
				fields["reason"] = closeErr.Text
			} else {
				fields["error"] = err.Error()
			}
			logger.Write(fields)
			if closeErr, ok := err.(*websocket.CloseError); ok {
				_ = writeOfficialWebsocketMessage(target, targetWriteMu, websocket.CloseMessage, websocket.FormatCloseMessage(closeErr.Code, closeErr.Text))
			}
			return
		}
		isDataFrame := messageType == websocket.TextMessage || messageType == websocket.BinaryMessage
		activeResolver := resolve
		if activeResolver == nil {
			activeResolver = legacyRouteResolver
		}
		if rewriteClientFrames && isDataFrame && reconnectForWebsocketRouteChange(source, "official", payload, logger, activeResolver) {
			return
		}
		if rewriteClientFrames && isDataFrame {
			if message, ok := officialCompactionTriggerMessage(payload); ok {
				// The official upstream owns this socket's incremental history;
				// the dedicated compactor starts without it. A transport restart
				// makes Codex resend the full input, including the trigger, before
				// we generate a summary. Never summarize only an incremental tail.
				if routing.PreviousResponseID(message) != "" {
					logger.Write(map[string]any{"event": "official_compaction_replay_required", "transport": "websocket"})
					sourceWriteMu.Lock()
					_ = source.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
						websocket.CloseServiceRestart, "compaction requires full conversation input; reconnect and resend",
					), time.Now().Add(time.Second))
					sourceWriteMu.Unlock()
					return
				}
				compactionModel := readyRoleModel(roles.Compaction)
				if compactionModel == "" {
					writeModelRoleUnavailableWebsocket(source, sourceWriteMu, "compaction")
					continue
				}
				logger.Write(map[string]any{"event": "official_compaction_intercepted", "compaction_model": compactionModel, "transport": "websocket"})
				runOfficialWebsocketCompaction(source, sourceWriteMu, logger, nativeReplay, clientRequest, message, compactionModel, roles.Compaction.ProviderID)
				continue
			}
			if model, ok := officialInferenceModel(payload, boundModel); ok {
				effectiveFallback := readyRoleModel(roles.OfficialFallback)
				if blocked, reason := quotaGuard.BlockedModelWithTarget(accountID, model, effectiveFallback, time.Now()); blocked {
					if effectiveFallback == "" {
						writeModelRoleUnavailableWebsocket(source, sourceWriteMu, "officialFallback")
						continue
					}
					logger.Write(map[string]any{"event": "official_quota_guard_blocked", "transport": "websocket", "model": model, "reason": reason, "action": "failover", "effective_model": effectiveFallback})
					sourceWriteMu.Lock()
					_ = source.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
						websocket.CloseServiceRestart, "official quota reserve protected; reconnect for automatic external failover",
					), time.Now().Add(time.Second))
					sourceWriteMu.Unlock()
					return
				}
			}
		}
		if !rewriteClientFrames && isDataFrame {
			quotaGuard.ObserveRateLimitPayload(accountID, payload, time.Now())
		}
		if rewriteClientFrames && isDataFrame {
			if rewritten, ok := compaction.SanitizeWebsocketFrame(payload, logger); ok {
				payload = rewritten
			}
		}
		if err := writeOfficialWebsocketMessage(target, targetWriteMu, messageType, payload); err != nil {
			return
		}
	}
}

func writeOfficialWebsocketMessage(conn *websocket.Conn, mu *sync.Mutex, messageType int, payload []byte) error {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(officialWebsocketWriteWait))
	return conn.WriteMessage(messageType, payload)
}

func officialCompactionTriggerMessage(payload []byte) (map[string]any, bool) {
	var message map[string]any
	if json.Unmarshal(payload, &message) != nil {
		return nil, false
	}
	requestType := strings.TrimSpace(routing.StringOrEmpty(message["type"]))
	if requestType != "response.create" && requestType != "response.append" {
		return nil, false
	}
	input, ok := message["input"].([]any)
	if !ok || !routing.InputHasCompactionTrigger(input) {
		return nil, false
	}
	return message, true
}

func runOfficialWebsocketCompaction(client *websocket.Conn, clientWriteMu *sync.Mutex, logger *activity.Logger, nativeReplay *compaction.NativeBridge, original *http.Request, message map[string]any, compactionModel, providerID string) {
	if original == nil {
		return
	}
	session := &wopalWebsocketSession{
		logger:          logger,
		model:           compactionModel,
		providerID:      providerID,
		compactionModel: compactionModel,
		request:         original.Clone(original.Context()),
		nativeReplay:    nativeReplay,
	}
	session.emit = func(payload []byte) error {
		return writeOfficialWebsocketMessage(client, clientWriteMu, websocket.TextMessage, payload)
	}
	ctx, cancel, ok := session.startTurn()
	if !ok {
		session.sendError(http.StatusConflict, "gateway_turn_overlap", "a compaction response is already active")
		return
	}
	session.forwardCompactionTurn(ctx, uuid.NewString(), time.Now(), message)
	session.finishTurn(cancel)
}

func writeModelRoleUnavailableWebsocket(conn *websocket.Conn, mu *sync.Mutex, role string) {
	payload, _ := json.Marshal(map[string]any{"type": "error", "status": http.StatusServiceUnavailable, "error": map[string]any{
		"type": "server_error", "code": "model_role_unavailable", "message": fmt.Sprintf("model role %s is unavailable", role),
	}})
	_ = writeOfficialWebsocketMessage(conn, mu, websocket.TextMessage, payload)
}

func dialOfficialWebsocketUpstream(c *gin.Context, target *url.URL) (*websocket.Conn, *http.Response, error) {
	header := c.Request.Header.Clone()
	for _, name := range []string{
		"Connection", "Upgrade", "Sec-WebSocket-Key",
		"Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
	} {
		header.Del(name)
	}
	dialer := websocket.Dialer{
		Subprotocols:     websocket.Subprotocols(c.Request),
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  1 << 20,
	}
	scheme := target.Scheme
	switch scheme {
	case "http":
		scheme = "ws"
	case "https":
		scheme = "wss"
	}
	upstreamURL := url.URL{
		Scheme:   scheme,
		Host:     target.Host,
		Path:     routing.JoinURLPath(target.Path, stripV1(c.Request.URL.Path)),
		RawQuery: c.Request.URL.RawQuery,
	}
	conn, response, err := dialer.Dial(upstreamURL.String(), header)
	if err != nil {
		return nil, response, err
	}
	return conn, response, nil
}

func officialInferenceModel(payload []byte, fallback string) (string, bool) {
	var request struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &request) != nil || (request.Type != "response.create" && request.Type != "response.append") {
		return "", false
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = strings.TrimSpace(fallback)
	}
	return model, true
}
