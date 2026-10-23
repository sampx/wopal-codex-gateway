package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var wopalWebsocketUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

const maxWopalSessionStateBytes = 256 << 20

type wopalWebsocketSession struct {
	conn         *websocket.Conn
	writeMu      sync.Mutex
	activeMu     sync.Mutex
	stateMu      sync.Mutex
	cancel       context.CancelFunc
	state        wopalConversationState
	logger       *activityLogger
	model        string
	request      *http.Request
	nativeReplay *nativeCompactionBridge
	emit         func([]byte) error
}

func isWebsocketUpgrade(req *http.Request) bool {
	return req != nil && strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func serveWopalWebsocketBridge(c *gin.Context, logger *activityLogger, model string) {
	conn, err := wopalWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.write(map[string]any{"event": "wopal_ws_upgrade_failed", "model": model, "error": err.Error()})
		return
	}
	s := &wopalWebsocketSession{conn: conn, logger: logger, model: model, request: c.Request.Clone(c.Request.Context())}
	if bridge, ok := c.Get("native_compaction_bridge"); ok {
		s.nativeReplay, _ = bridge.(*nativeCompactionBridge)
	}
	defer func() {
		s.cancelActive()
		released := s.releaseState()
		logger.write(map[string]any{"event": "wopal_ws_state_released", "model": model, "state_bytes": released})
		_ = conn.Close()
	}()
	logger.write(map[string]any{"event": "wopal_ws_connected", "model": model})
	failoverRequestedModel, _ := c.Get("quota_failover_requested_model")
	failoverGuardValue, _ := c.Get("quota_failover_guard")
	failoverGuard, _ := failoverGuardValue.(*officialQuotaGuard)
	failoverAccountValue, _ := c.Get("quota_failover_account_id")
	failoverAccountID, _ := failoverAccountValue.(string)
	requestedOfficialModel, _ := failoverRequestedModel.(string)

	for {
		messageType, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			fields := map[string]any{"event": "wopal_ws_closed", "model": model}
			if closeErr, ok := errRead.(*websocket.CloseError); ok {
				fields["code"] = closeErr.Code
				fields["reason"] = closeErr.Text
				fields["normal"] = closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway
			} else {
				fields["error"] = errRead.Error()
			}
			logger.write(fields)
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		if requestedOfficialModel != "" && failoverGuard != nil {
			rewritten, failback, changed := rewriteQuotaFailoverWebsocketFrame(payload, failoverGuard, failoverAccountID, requestedOfficialModel, time.Now())
			if failback {
				logger.write(map[string]any{"event": "official_quota_failback", "requested_model": requestedOfficialModel, "transport": "websocket"})
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, "official quota recovered; reconnect and resend full input"), time.Now().Add(time.Second))
				return
			}
			if changed {
				payload = rewritten
			}
		}
		if reconnectForWebsocketRouteChange(conn, routeWopal, payload, logger) {
			return
		}
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			s.sendError(http.StatusBadRequest, "gateway_invalid_json", "websocket message is not valid JSON")
			continue
		}
		switch message["type"] {
		case "response.create", "response.append":
			if generate, ok := message["generate"].(bool); ok && !generate {
				if err := s.handlePrewarm(message); err != nil {
					s.sendPrepareError(err)
				}
				continue
			}
			ctx, cancel, ok := s.startTurn()
			if !ok {
				s.sendError(http.StatusConflict, "gateway_turn_overlap", "a response is already active on this websocket")
				continue
			}
			go s.forwardTurn(ctx, cancel, message)
		case "response.interrupt":
			s.cancelActive()
			responseID, _ := message["response_id"].(string)
			_ = s.sendJSON(map[string]any{"type": "response.interrupt.accepted", "response_id": responseID})
			_ = s.sendJSON(map[string]any{"type": "response.incomplete", "response": map[string]any{"id": responseID, "status": "incomplete", "incomplete_details": map[string]any{"reason": "interrupted"}}})
		default:
			s.sendError(http.StatusBadRequest, "gateway_unsupported_ws_message", fmt.Sprintf("unsupported websocket message type: %v", message["type"]))
		}
	}
}

func (s *wopalWebsocketSession) prepareTurn(ctx context.Context, message map[string]any) (wopalPreparedTurn, error) {
	if err := s.nativeReplay.expand(ctx, s.request, message); err != nil {
		return wopalPreparedTurn{}, err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return prepareWopalWebsocketTurn(message, s.model, s.state)
}

func (s *wopalWebsocketSession) prepareCompactionTurn(ctx context.Context, message map[string]any) (wopalPreparedTurn, error) {
	if err := s.nativeReplay.expand(ctx, s.request, message); err != nil {
		return wopalPreparedTurn{}, err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return prepareWopalCompactionSummaryTurn(message, dedicatedWopalCompactionModel, s.state)
}

func validateWopalSessionStateSize(requestBytes, outputBytes int) error {
	if requestBytes < 0 || outputBytes < 0 || requestBytes > maxWopalSessionStateBytes-outputBytes {
		return fmt.Errorf("session transcript exceeds %d bytes", maxWopalSessionStateBytes)
	}
	return nil
}

func (s *wopalWebsocketSession) commitState(nextRequest, output []byte, responseID, compactionID string) error {
	if err := validateWopalSessionStateSize(len(nextRequest), len(output)); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.state.lastRequest = bytes.Clone(nextRequest)
	s.state.lastResponseOutput = bytes.Clone(output)
	s.state.lastResponseID = strings.TrimSpace(responseID)
	s.state.activeCompactionID = strings.TrimSpace(compactionID)
	s.stateMu.Unlock()
	return nil
}

func (s *wopalWebsocketSession) releaseState() int {
	s.stateMu.Lock()
	released := len(s.state.lastRequest) + len(s.state.lastResponseOutput) + len(s.state.activeCompactionID)
	s.state.lastRequest = nil
	s.state.lastResponseOutput = nil
	s.state.lastResponseID = ""
	s.state.activeCompactionID = ""
	s.stateMu.Unlock()
	return released
}

func (s *wopalWebsocketSession) handlePrewarm(message map[string]any) error {
	prepared, err := s.prepareTurn(s.request.Context(), message)
	if err != nil {
		return err
	}
	responseID := "resp_prewarm_" + uuid.NewString()
	if err := s.commitState(prepared.nextRequest, []byte("[]"), responseID, prepared.nextCompactionID); err != nil {
		return err
	}
	created := map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "status": "in_progress", "model": s.model, "output": []any{}}}
	completed := map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "model": s.model, "output": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}}
	if err := s.sendJSON(created); err != nil {
		return err
	}
	return s.sendJSON(completed)
}

func (s *wopalWebsocketSession) sendPrepareError(err error) {
	if err != nil && strings.Contains(err.Error(), "previous_response_not_found") {
		s.sendError(http.StatusConflict, "previous_response_not_found", "previous response is not available on this websocket; resend the full conversation input")
		return
	}
	if err != nil && strings.Contains(err.Error(), "unsupported_compaction_capsule") {
		s.sendError(http.StatusBadRequest, "compaction_replay_unavailable", "compaction state cannot be restored by this gateway; no conversation context was discarded")
		return
	}
	if err != nil {
		s.sendError(http.StatusBadRequest, "gateway_invalid_incremental_request", err.Error())
	}
}

func (s *wopalWebsocketSession) startTurn() (context.Context, context.CancelFunc, bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.cancel != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(s.request.Context())
	s.cancel = cancel
	return ctx, cancel, true
}

func (s *wopalWebsocketSession) clearActive() {
	s.activeMu.Lock()
	s.cancel = nil
	s.activeMu.Unlock()
}

func (s *wopalWebsocketSession) finishTurn(cancel context.CancelFunc) {
	s.clearActive()
	cancel()
}

func (s *wopalWebsocketSession) cancelActive() {
	s.activeMu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *wopalWebsocketSession) forwardTurn(ctx context.Context, cancel context.CancelFunc, message map[string]any) {
	defer s.finishTurn(cancel)
	turnID := uuid.NewString()
	started := time.Now()

	if input, ok := message["input"].([]any); ok && wopalInputHasCompactionTrigger(input) {
		s.forwardCompactionTurn(ctx, turnID, started, message)
		return
	}

	prepared, err := s.prepareTurn(ctx, message)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.clearActive()
		s.sendPrepareError(err)
		return
	}

	var normalized map[string]any
	_ = json.Unmarshal(prepared.body, &normalized)
	model := strings.TrimSpace(fmt.Sprint(normalized["model"]))
	inputItems := 0
	if input, ok := normalized["input"].([]any); ok {
		inputItems = len(input)
	}
	s.logger.write(map[string]any{
		"event": "turn_started", "turn_id": turnID, "provider": "wopal-ai", "client_model": model,
		"client_transport": "websocket", "upstream_transport": "http-sse", "input_items": inputItems,
		"previous_response_id_present": previousResponseID(message) != "",
	})

	target := "http://" + s.request.Host + "/v1/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(prepared.body))
	if err != nil {
		s.sendError(http.StatusInternalServerError, "gateway_request_failed", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+localAccessKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.write(map[string]any{"event": "turn_failed", "turn_id": turnID, "provider": "wopal-ai", "error": err.Error(), "duration_ms": time.Since(started).Milliseconds()})
		s.sendError(http.StatusBadGateway, "gateway_upstream_request_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		s.logger.write(map[string]any{"event": "turn_failed", "turn_id": turnID, "provider": "wopal-ai", "upstream_status": resp.StatusCode, "duration_ms": time.Since(started).Milliseconds()})
		s.clearActive()
		s.sendUpstreamError(resp.StatusCode, raw)
		return
	}

	terminal := false
	terminalType := ""
	collectedOutput := make([]json.RawMessage, 0, 8)
	err = readSSE(resp.Body, func(data []byte) error {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			return nil
		}
		typeName, _ := event["type"].(string)
		if typeName == "response.output_item.done" {
			if raw := rawOutputItemFromEvent(data); len(raw) > 0 {
				collectedOutput = append(collectedOutput, raw)
			}
		}
		switch typeName {
		case "response.completed":
			terminal = true
			terminalType = typeName
			output, responseID := responseOutputFromRawEvent(data, collectedOutput)
			if errCommit := s.commitState(prepared.nextRequest, output, responseID, prepared.nextCompactionID); errCommit != nil {
				released := s.releaseState()
				s.logger.write(map[string]any{
					"event": "wopal_ws_state_dropped", "turn_id": turnID, "provider": "wopal-ai",
					"error": errCommit.Error(), "released_bytes": released,
				})
			}
			s.clearActive()
		case "response.failed", "response.incomplete", "error":
			terminal = true
			terminalType = typeName
			s.clearActive()
		}
		if err := s.sendRaw(data); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.sendError(http.StatusBadGateway, "gateway_upstream_stream_error", err.Error())
		return
	}
	if !terminal && ctx.Err() == nil {
		s.clearActive()
		s.sendError(http.StatusBadGateway, "gateway_stream_ended", "upstream stream ended before a terminal response event")
		return
	}
	if terminalType == "response.completed" {
		s.logger.write(map[string]any{"event": "turn_completed", "turn_id": turnID, "provider": "wopal-ai", "duration_ms": time.Since(started).Milliseconds()})
	} else {
		s.logger.write(map[string]any{"event": "turn_failed", "turn_id": turnID, "provider": "wopal-ai", "terminal_event": terminalType, "duration_ms": time.Since(started).Milliseconds()})
	}
}

func (s *wopalWebsocketSession) forwardCompactionTurn(ctx context.Context, turnID string, started time.Time, message map[string]any) {
	compactionModel := dedicatedWopalCompactionModel
	prepared, err := s.prepareCompactionTurn(ctx, message)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.clearActive()
		s.sendPrepareError(err)
		return
	}

	responseID := "resp_wopal_compact_" + uuid.NewString()
	created := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": responseID, "status": "in_progress", "model": compactionModel, "output": []any{},
		},
	}
	if err := s.sendJSON(created); err != nil {
		return
	}
	s.logger.write(map[string]any{
		"event": "compaction_started", "turn_id": turnID, "provider": "wopal-ai", "client_model": compactionModel,
	})

	target := "http://" + s.request.Host + "/v1/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(prepared.body))
	if err != nil {
		s.clearActive()
		s.sendError(http.StatusInternalServerError, "gateway_compaction_request_failed", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+localAccessKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.write(map[string]any{
			"event": "compaction_failed", "turn_id": turnID, "provider": "wopal-ai",
			"error": err.Error(), "duration_ms": time.Since(started).Milliseconds(),
		})
		s.clearActive()
		s.sendError(http.StatusBadGateway, "gateway_compaction_upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		s.logger.write(map[string]any{
			"event": "compaction_failed", "turn_id": turnID, "provider": "wopal-ai",
			"upstream_status": resp.StatusCode, "duration_ms": time.Since(started).Milliseconds(),
		})
		s.clearActive()
		s.sendUpstreamError(resp.StatusCode, raw)
		return
	}

	terminal := false
	terminalFailed := false
	collectedOutput := make([]json.RawMessage, 0, 4)
	var streamedText strings.Builder
	var finalOutput []byte
	var usage any = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	err = readSSE(resp.Body, func(data []byte) error {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return nil
		}
		var event map[string]any
		if errDecode := json.Unmarshal(data, &event); errDecode != nil {
			return nil
		}
		typeName, _ := event["type"].(string)
		switch typeName {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				streamedText.WriteString(delta)
			}
		case "response.output_item.done":
			if raw := rawOutputItemFromEvent(data); len(raw) > 0 {
				collectedOutput = append(collectedOutput, raw)
			}
		case "response.completed":
			terminal = true
			finalOutput, _ = responseOutputFromRawEvent(data, collectedOutput)
			if response, ok := event["response"].(map[string]any); ok {
				if gotUsage, exists := response["usage"]; exists && gotUsage != nil {
					usage = gotUsage
				}
			}
		case "response.failed", "response.incomplete", "error":
			terminal = true
			terminalFailed = true
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.write(map[string]any{
			"event": "compaction_failed", "turn_id": turnID, "provider": "wopal-ai",
			"error": err.Error(), "duration_ms": time.Since(started).Milliseconds(),
		})
		s.clearActive()
		s.sendError(http.StatusBadGateway, "gateway_compaction_stream_error", err.Error())
		return
	}
	if !terminal || terminalFailed {
		s.logger.write(map[string]any{
			"event": "compaction_failed", "turn_id": turnID, "provider": "wopal-ai",
			"reason": "summary_generation_incomplete", "duration_ms": time.Since(started).Milliseconds(),
		})
		s.clearActive()
		s.sendError(http.StatusBadGateway, "gateway_compaction_incomplete", "compaction summary generation did not complete")
		return
	}

	summary := extractWopalCompactionSummary(finalOutput, streamedText.String())
	if err := validateWopalCompactionSummary(summary); err != nil {
		s.logger.write(map[string]any{
			"event": "compaction_failed", "turn_id": turnID, "provider": "wopal-ai",
			"reason": "invalid_summary", "duration_ms": time.Since(started).Milliseconds(),
		})
		s.clearActive()
		s.sendError(http.StatusBadGateway, "gateway_compaction_invalid_summary", err.Error())
		return
	}

	compactionID := "cmp_wopal_" + uuid.NewString()
	retainedPrefix, err := wopalCompactionRetainedPrefix(prepared.nextRequest)
	if err != nil {
		s.clearActive()
		s.sendError(http.StatusInternalServerError, "gateway_compaction_prefix_failed", err.Error())
		return
	}
	capsule := wopalCompactionCapsule{Version: 1, ID: compactionID, Model: compactionModel, Summary: summary, Prefix: retainedPrefix}
	encryptedContent, err := sealWopalCompactionCapsule(capsule)
	if err != nil {
		s.clearActive()
		s.sendError(http.StatusInternalServerError, "gateway_compaction_seal_failed", err.Error())
		return
	}
	compactedRequest, err := buildWopalCompactedStateRequest(prepared.nextRequest, capsule)
	if err != nil {
		s.clearActive()
		s.sendError(http.StatusInternalServerError, "gateway_compaction_state_failed", err.Error())
		return
	}
	if err := s.commitState(compactedRequest, []byte("[]"), responseID, compactionID); err != nil {
		released := s.releaseState()
		s.logger.write(map[string]any{
			"event": "wopal_ws_state_dropped", "turn_id": turnID, "provider": "wopal-ai",
			"error": err.Error(), "released_bytes": released,
		})
		s.clearActive()
		s.sendError(http.StatusInsufficientStorage, "gateway_compaction_state_too_large", err.Error())
		return
	}

	item := map[string]any{
		"type": "compaction", "id": compactionID, "encrypted_content": encryptedContent,
	}
	s.clearActive()
	if err := s.sendJSON(map[string]any{
		"type": "response.output_item.added", "output_index": 0, "item": item,
	}); err != nil {
		return
	}
	if err := s.sendJSON(map[string]any{
		"type": "response.output_item.done", "output_index": 0, "item": item,
	}); err != nil {
		return
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": responseID, "status": "completed", "model": compactionModel,
			"output": []any{item}, "usage": usage,
		},
	}
	if err := s.sendJSON(completed); err != nil {
		return
	}
	s.logger.write(map[string]any{
		"event": "compaction_completed", "turn_id": turnID, "provider": "wopal-ai",
		"summary_bytes": len(summary), "duration_ms": time.Since(started).Milliseconds(),
	})
}

func readSSE(r io.Reader, onData func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines []string
	flush := func() error {
		if len(lines) == 0 {
			return nil
		}
		parts := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				parts = append(parts, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		lines = lines[:0]
		if len(parts) == 0 {
			return nil
		}
		return onData([]byte(strings.Join(parts, "\n")))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func (s *wopalWebsocketSession) sendUpstreamError(status int, raw []byte) {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) == nil {
		if upstreamError, ok := envelope["error"]; ok {
			_ = s.sendJSON(map[string]any{"type": "error", "status": status, "error": upstreamError})
			return
		}
	}
	s.sendError(status, "gateway_upstream_error", strings.TrimSpace(string(raw)))
}

func (s *wopalWebsocketSession) sendError(status int, code, message string) {
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	_ = s.sendJSON(map[string]any{"type": "error", "status": status, "error": map[string]any{"type": "server_error", "code": code, "message": message}})
}

func (s *wopalWebsocketSession) sendJSON(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.sendRaw(payload)
}

func (s *wopalWebsocketSession) sendRaw(payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.emit != nil {
		return s.emit(payload)
	}
	return s.conn.WriteMessage(websocket.TextMessage, payload)
}
