package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/routing"
)

// HTTP fallback must restore the same compacted history as the websocket path.
// Returns false when this function has handled the request (including errors).
func prepareWopalHTTP(c *gin.Context, logger *activity.Logger, model string, nativeReplay *compaction.NativeBridge, inspection *requestInspection) bool {
	var message map[string]any
	var err error
	if inspection == nil || inspection.payload == nil {
		err = fmt.Errorf("request body is required")
	} else {
		if len(inspection.decoded) > routing.MaxBodyBytes {
			err = fmt.Errorf("decoded request exceeds routing limit")
		} else {
			message = inspection.payload
		}
	}
	if err == nil {
		err = nativeReplay.Expand(c.Request.Context(), c.Request, message)
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "compaction_replay_unavailable", "type": "invalid_request_error", "message": err.Error()}})
		return false
	}
	input, isArray := message["input"].([]any)
	compactEndpoint := c.Request.URL.Path == "/v1/responses/compact"
	if compactEndpoint && !isArray {
		if text, ok := message["input"].(string); ok && text != "" {
			input = []any{map[string]any{"type": "message", "role": "user", "content": text}}
		} else {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "compaction requires conversation input"}})
			return false
		}
	}
	if compactEndpoint || routing.InputHasCompactionTrigger(input) {
		message["type"] = "response.create"
		if compactEndpoint {
			message["input"] = append(input, map[string]any{"type": "compaction_trigger"})
		}
		s := &wopalWebsocketSession{request: c.Request, model: model, logger: logger, nativeReplay: nativeReplay}
		s.emit = func(payload []byte) error {
			if !compactEndpoint {
				c.Header("Content-Type", "text/event-stream")
				_, err := fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
				c.Writer.Flush()
				return err
			}
			var event struct {
				Type     string          `json:"type"`
				Status   int             `json:"status"`
				Response json.RawMessage `json:"response"`
				Error    json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				return err
			}
			switch event.Type {
			case "response.completed":
				c.Data(http.StatusOK, "application/json", event.Response)
			case "error":
				c.Data(event.Status, "application/json", []byte(`{"error":`+string(event.Error)+`}`))
			}
			return nil
		}
		c.Abort()
		s.forwardCompactionTurn(c.Request.Context(), uuid.NewString(), time.Now(), message)
		return false
	}
	if isArray {
		replay, err := compaction.ExpandReplayInput(input)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "compaction_replay_unavailable", "type": "invalid_request_error", "message": err.Error()}})
			return false
		}
		if replay.Found {
			message["input"] = replay.Expanded
			delete(message, "previous_response_id")
		}
	}
	body, err := json.Marshal(message)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Del("Content-Encoding")
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return true
}
