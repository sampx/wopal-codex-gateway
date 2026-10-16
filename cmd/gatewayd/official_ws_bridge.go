package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
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

func serveOfficialWebsocketBridge(c *gin.Context, logger *activityLogger, target *url.URL) {
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
	go pumpOfficialWebsocket(clientConn, upstreamConn, done, true, logger)
	go pumpOfficialWebsocket(upstreamConn, clientConn, done, false, logger)

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
func pumpOfficialWebsocket(source, target *websocket.Conn, done chan struct{}, rewriteClientFrames bool, logger *activityLogger) {
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
				_ = target.SetWriteDeadline(time.Now().Add(officialWebsocketWriteWait))
				_ = target.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(closeErr.Code, closeErr.Text),
				)
			}
			return
		}
		if rewriteClientFrames && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) &&
			reconnectForWebsocketRouteChange(source, routeOfficial, payload, logger) {
			return
		}
		if rewriteClientFrames && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) &&
			containsOfficialSanitizeMarker(payload) {
			if rewritten, ok := sanitizeOfficialWebsocketFrame(payload); ok {
				payload = rewritten
			}
		}
		_ = target.SetWriteDeadline(time.Now().Add(officialWebsocketWriteWait))
		if err := target.WriteMessage(messageType, payload); err != nil {
			return
		}
	}
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
	expanded, neutralized := sanitizeOfficialRequestInput(input)
	if neutralized == 0 {
		return nil, false
	}
	message["input"] = expanded
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
