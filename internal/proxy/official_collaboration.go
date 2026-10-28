package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"wopal-codex-gateway/internal/compaction"
)

type officialCollaborationContextKey struct{}
type officialCollaborationResponseStream bool

// Each queued create owns its restore mode. response.created binds that mode
// to an upstream response ID; incremental requests inherit from their explicit
// previous_response_id, not from whichever request happened to arrive last.
// Only IDs and booleans are retained, and all metadata dies with the socket.
type officialCollaborationState struct {
	mu        sync.Mutex
	pending   []bool
	current   bool
	active    bool
	activeID  string
	responses map[string]bool
}

func (state *officialCollaborationState) prepare(request map[string]any) error {
	if request["type"] != "response.create" && request["type"] != "response.append" {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pending) >= 16 {
		return fmt.Errorf("too many outstanding official response requests")
	}
	mode := compaction.OfficialCollaborationRewriteRequired(request)
	if !mode && !compaction.OfficialCollaborationNamespaceConflict(request) {
		previous, _ := request["previous_response_id"].(string)
		mode = state.responses[strings.TrimSpace(previous)]
	}
	state.pending = append(state.pending, mode)
	return nil
}

func (state *officialCollaborationState) restore(payload []byte) ([]byte, error) {
	var event struct {
		Type       string `json:"type"`
		ResponseID string `json:"response_id"`
		Response   struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return payload, nil
	}
	state.mu.Lock()
	id := event.ResponseID
	if id == "" {
		id = event.Response.ID
	}
	mode := state.current
	known := false
	if id != "" {
		mode, known = state.responses[id]
	}
	if event.Type == "response.created" {
		if len(state.pending) > 0 {
			state.current, state.pending = state.pending[0], state.pending[1:]
		}
		if id != "" {
			if state.responses == nil {
				state.responses = make(map[string]bool)
			}
			state.responses[id] = state.current
		}
		state.active, state.activeID = true, id
		mode = state.current
	} else if event.Type == "error" || event.Type == "response.failed" {
		currentFailure := state.active && id != "" && id == state.activeID
		if id == "" && state.active && len(state.pending) > 0 {
			state.mu.Unlock()
			return payload, fmt.Errorf("cannot associate official websocket failure with an active response or pending create")
		}
		if !currentFailure && !known && len(state.pending) > 0 {
			// Validation can reject a create before response.created. Consume
			// that create's mode, without touching an already-created response.
			mode, state.pending = state.pending[0], state.pending[1:]
		} else if currentFailure || (id == "" && state.active) {
			state.active = false
		}
	}
	if (event.Type == "response.completed" || event.Type == "response.incomplete") && (id == "" || id == state.activeID) {
		state.active = false
	}
	state.mu.Unlock()
	if mode {
		return compaction.RestoreOfficialCollaborationResponse(payload), nil
	}
	return payload, nil
}

// Transform one event at a time so tool identity restoration never buffers a
// whole SSE turn or delays the first tool call until generation finishes.
type officialCollaborationBody struct {
	io.ReadCloser
	reader  *bufio.Reader
	stream  bool
	pending []byte
	err     error
}

func (body *officialCollaborationBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 && body.err == nil {
		if !body.stream {
			raw, err := io.ReadAll(io.LimitReader(body.reader, officialWebsocketFrameLimit+1))
			if len(raw) > officialWebsocketFrameLimit {
				return 0, fmt.Errorf("official response exceeds compatibility limit")
			}
			body.pending = compaction.RestoreOfficialCollaborationResponse(raw)
			body.err = err
			if body.err == nil {
				body.err = io.EOF
			}
			break
		}
		var event []byte
		for {
			line, err := body.reader.ReadString('\n')
			event = append(event, line...)
			if len(event) > officialWebsocketFrameLimit {
				return 0, fmt.Errorf("official SSE event exceeds compatibility limit")
			}
			if err != nil {
				body.err = err
				break
			}
			if line == "\n" || line == "\r\n" {
				break
			}
		}
		body.pending = restoreOfficialCollaborationEvent(event)
	}
	if len(body.pending) == 0 {
		return 0, body.err
	}
	n := copy(p, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func restoreOfficialCollaborationEvent(event []byte) []byte {
	lines := bytes.SplitAfter(event, []byte("\n"))
	var data [][]byte
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[5:]))
		}
	}
	raw := bytes.Join(data, []byte("\n"))
	restored := compaction.RestoreOfficialCollaborationResponse(raw)
	if bytes.Equal(restored, raw) {
		return event
	}
	var result []byte
	written := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if !written {
				result = append(result, []byte("data: ")...)
				result = append(result, restored...)
				result = append(result, '\n')
				written = true
			}
			continue
		}
		result = append(result, line...)
	}
	return result
}

func restoreOfficialHTTPResponse(resp *http.Response) error {
	requestedStream, enabled := resp.Request.Context().Value(officialCollaborationContextKey{}).(officialCollaborationResponseStream)
	if !enabled || resp.Body == nil {
		return nil
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("unexpected compressed official compatibility response")
	}
	contentType := resp.Header.Get("Content-Type")
	stream := strings.HasPrefix(contentType, "text/event-stream")
	// ChatGPT's native Responses endpoint can label SSE as text/plain.
	if !strings.HasPrefix(contentType, "application/json") && bool(requestedStream) {
		stream = true
	}
	if !stream && !strings.HasPrefix(contentType, "application/json") {
		return nil
	}
	resp.Body = &officialCollaborationBody{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), stream: stream}
	resp.ContentLength = -1
	for _, name := range []string{"Content-Length", "ETag", "Content-MD5"} {
		resp.Header.Del(name)
	}
	return nil
}
