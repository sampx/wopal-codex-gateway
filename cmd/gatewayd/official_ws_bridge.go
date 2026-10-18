package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
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

func serveOfficialWebsocketBridge(c *gin.Context, logger *activityLogger, target *url.URL, quotaGuard *officialQuotaGuard, accountID, boundModel string, nativeReplay *nativeCompactionBridge) {
	// Dial upstream before upgrading the client so a failed dial still returns
	// a clean HTTP error instead of a half-open websocket.
	upstreamConn, upstreamResponse, err := dialOfficialWebsocketUpstream(c, target)
	if err != nil {
		logger.write(map[string]any{"event": "official_ws_dial_failed", "upstream": target.Host, "error": err.Error()})
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	// Preserve upstream handshake response headers the native transport relied
	// on, then let gorilla own the websocket-specific ones.
	responseHeader := http.Header{}
	if upstreamResponse != nil {
		quotaGuard.observeHeaders(accountID, upstreamResponse.Header, time.Now())
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
		logger.write(map[string]any{"event": "official_ws_upgrade_failed", "error": err.Error()})
		_ = upstreamConn.Close()
		return
	}
	clientConn.SetReadLimit(officialWebsocketFrameLimit)
	upstreamConn.SetReadLimit(officialWebsocketFrameLimit)
	logger.write(map[string]any{
		"event":       "official_ws_connected",
		"upstream":    target.Host,
		"subprotocol": upstreamConn.Subprotocol(),
	})

	done := make(chan struct{}, 2)
	clientRequest := c.Request.Clone(c.Request.Context())
	var clientWriteMu sync.Mutex
	var upstreamWriteMu sync.Mutex
	go pumpOfficialWebsocket(clientConn, upstreamConn, done, true, logger, quotaGuard, accountID, boundModel, nativeReplay, clientRequest, &clientWriteMu, &upstreamWriteMu)
	go pumpOfficialWebsocket(upstreamConn, clientConn, done, false, logger, quotaGuard, accountID, boundModel, nil, nil, &upstreamWriteMu, &clientWriteMu)

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
func pumpOfficialWebsocket(source, target *websocket.Conn, done chan struct{}, rewriteClientFrames bool, logger *activityLogger, quotaGuard *officialQuotaGuard, accountID, boundModel string, nativeReplay *nativeCompactionBridge, clientRequest *http.Request, sourceWriteMu, targetWriteMu *sync.Mutex) {
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
			logger.write(fields)
			if closeErr, ok := err.(*websocket.CloseError); ok {
				_ = writeOfficialWebsocketMessage(target, targetWriteMu, websocket.CloseMessage, websocket.FormatCloseMessage(closeErr.Code, closeErr.Text))
			}
			return
		}
		isDataFrame := messageType == websocket.TextMessage || messageType == websocket.BinaryMessage
		if rewriteClientFrames && isDataFrame && reconnectForWebsocketRouteChange(source, routeOfficial, payload, logger) {
			return
		}
		if rewriteClientFrames && isDataFrame {
			if message, ok := officialCompactionTriggerMessage(payload); ok {
				logger.write(map[string]any{"event": "official_compaction_intercepted", "compaction_model": dedicatedWopalCompactionModel, "transport": "websocket"})
				runOfficialWebsocketCompaction(source, sourceWriteMu, logger, nativeReplay, clientRequest, message)
				continue
			}
			if model, ok := officialInferenceModel(payload, boundModel); ok {
				if blocked, reason := quotaGuard.blockedModel(accountID, model, time.Now()); blocked {
					logger.write(map[string]any{"event": "official_quota_guard_blocked", "transport": "websocket", "model": model, "reason": reason, "action": "failover", "effective_model": officialQuotaFailoverModel})
					sourceWriteMu.Lock()
					_ = source.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
						websocket.CloseServiceRestart, "official quota reserve protected; reconnect for automatic Wopal failover",
					), time.Now().Add(time.Second))
					sourceWriteMu.Unlock()
					return
				}
			}
		}
		if !rewriteClientFrames && isDataFrame {
			quotaGuard.observeRateLimitPayload(accountID, payload, time.Now())
		}
		if rewriteClientFrames && isDataFrame && containsOfficialSanitizeMarker(payload) {
			if rewritten, ok := sanitizeOfficialWebsocketFrame(payload); ok {
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
	requestType := strings.TrimSpace(stringOrEmpty(message["type"]))
	if requestType != "response.create" && requestType != "response.append" {
		return nil, false
	}
	input, ok := message["input"].([]any)
	if !ok || !wopalInputHasCompactionTrigger(input) {
		return nil, false
	}
	return message, true
}

func runOfficialWebsocketCompaction(client *websocket.Conn, clientWriteMu *sync.Mutex, logger *activityLogger, nativeReplay *nativeCompactionBridge, original *http.Request, message map[string]any) {
	if original == nil {
		return
	}
	session := &wopalWebsocketSession{
		logger:       logger,
		model:        dedicatedWopalCompactionModel,
		request:      original.Clone(original.Context()),
		nativeReplay: nativeReplay,
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

func sanitizeOfficialWebsocketFrame(payload []byte) ([]byte, bool) {
	var message map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&message); err != nil {
		return nil, false
	}
	input, ok := message["input"].([]any)
	if !ok {
		return nil, false
	}
	hadWopalCompaction := inputHasWopalCompactionCapsule(input)
	hadLocalPreviousResponse := hasWopalLocalPreviousResponseID(message)
	expanded, neutralized := sanitizeOfficialRequestInput(input)
	if neutralized == 0 && !hadLocalPreviousResponse {
		return nil, false
	}
	message["input"] = expanded
	if hadWopalCompaction || hadLocalPreviousResponse {
		delete(message, "previous_response_id")
	}
	rewritten, err := json.Marshal(message)
	if err != nil {
		return nil, false
	}
	return rewritten, true
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
		Path:     joinURLPath(target.Path, stripV1(c.Request.URL.Path)),
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
