package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Only the owning provider can read a native capsule. Convert it once into a
// text continuation before sending the conversation to a third-party model.
// The bounded cache contains no credentials; its keys include the auth scope.
const nativeCompactionRestoreTimeout = 3 * time.Minute

func nativeCompactionRestoreContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, nativeCompactionRestoreTimeout)
}

type nativeCompactionBridge struct {
	chatgptTarget, openaiTarget *url.URL
	mu                          sync.Mutex
	summaries                   map[[32]byte]string
	logger                      *activityLogger
	quotaGuard                  *officialQuotaGuard
}

func nativeCompactionModel() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	// Codex refreshes models_cache.json through the gateway's /models, which
	// contains third-party models only. Prefer the installed merged catalog.
	for _, name := range []string{"wopal-codex-gateway-models.json", "models_cache.json"} {
		raw, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			continue
		}
		var catalog struct {
			Models []struct {
				Slug, Visibility string
				Priority         int
			} `json:"models"`
		}
		if json.Unmarshal(raw, &catalog) != nil {
			continue
		}
		model, priority := "", 0
		for _, entry := range catalog.Models {
			if classifyModel(entry.Slug) == routeOfficial && entry.Visibility == "list" && (model == "" || entry.Priority < priority) {
				model, priority = entry.Slug, entry.Priority
			}
		}
		if model != "" {
			return model, nil
		}
	}
	return "", fmt.Errorf("native model catalog contains no available official model")
}

func (b *nativeCompactionBridge) expand(ctx context.Context, req *http.Request, message map[string]any) error {
	input, _ := message["input"].([]any)
	for index, value := range input {
		item, ok := value.(map[string]any)
		if !ok || itemTypeOf(item) != "compaction" || strings.HasPrefix(wopalEncryptedContent(item), wopalCompactionCapsulePrefix) {
			continue
		}
		if b == nil {
			return fmt.Errorf("native compaction needs its official provider to restore conversation context")
		}
		encoded, _ := json.Marshal(item)
		scope := req.Header.Get("Authorization") + "\x00" + req.Header.Get("ChatGPT-Account-ID") + "\x00" + req.Header.Get("OpenAI-Organization") + "\x00" + req.Header.Get("OpenAI-Project") + "\x00" + req.Header.Get("X-OpenAI-Account-Routing-Override") + "\x00"
		key := sha256.Sum256(append([]byte(scope), encoded...))
		b.mu.Lock()
		summary := b.summaries[key]
		b.mu.Unlock()
		if summary == "" {
			b.logger.write(map[string]any{"event": "native_compaction_restore_started"})
			var err error
			summary, err = b.summarize(ctx, req, input[:index+1])
			if err != nil {
				b.logger.write(map[string]any{"event": "native_compaction_restore_failed", "error": err.Error()})
				return err
			}
			b.logger.write(map[string]any{"event": "native_compaction_restore_completed", "summary_bytes": len(summary)})
			b.mu.Lock()
			if len(b.summaries) >= 32 {
				b.summaries = nil
			}
			if b.summaries == nil {
				b.summaries = make(map[[32]byte]string)
			}
			b.summaries[key] = summary
			b.mu.Unlock()
		}
		// Copy the slice: the original frame remains unchanged if conversion fails.
		input = append([]any(nil), input...)
		input[index] = map[string]any{"type": "message", "role": "developer", "id": "native_compaction_summary_" + reasoningItemID(item), "content": "The earlier conversation was compacted by the official provider. Continue from this preserved context:\n\n" + summary}
	}
	if input != nil {
		message["input"] = input
	}
	return nil
}

func (b *nativeCompactionBridge) summarize(ctx context.Context, original *http.Request, input []any) (string, error) {
	if blocked, _ := b.quotaGuard.blocked(chatGPTAccountID(original), time.Now()); blocked {
		return "", fmt.Errorf("official quota reserve is protected; native compaction restore is paused until the quota window resets")
	}
	if strings.TrimSpace(original.Header.Get("Authorization")) == "" || original.Header.Get("Authorization") == "Bearer "+localAccessKey {
		return "", fmt.Errorf("official credentials are required to restore native compacted history; switch to an official model first")
	}
	model, err := nativeCompactionModel()
	if err != nil {
		return "", err
	}
	input, _ = sanitizeOfficialRequestInput(input)
	filtered := make([]any, 0, len(input)+1)
	for _, item := range input {
		if !isWopalCompactionRetainedItem(item) {
			filtered = append(filtered, item)
		}
	}
	filtered = append(filtered, map[string]any{"type": "message", "role": "user", "content": wopalCompactionPrompt})
	body, err := json.Marshal(map[string]any{"model": model, "input": filtered, "instructions": wopalCompactionPrompt, "stream": true, "store": false})
	if err != nil {
		return "", err
	}
	ctx, cancel := nativeCompactionRestoreContext(ctx)
	defer cancel()
	if original.Header.Get("ChatGPT-Account-ID") != "" {
		return b.summarizeChatGPTWebsocket(ctx, original, model, body)
	}
	target := b.openaiTarget
	endpoint := *target
	endpoint.Path = joinURLPath(target.Path, "responses")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	for _, name := range []string{"Authorization", "ChatGPT-Account-ID", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "X-OpenAI-Account-Routing-Override", "User-Agent"} {
		if value := original.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Codex-Routing-Hint", "model="+model)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("restore native compacted history: %w", err)
	}
	defer resp.Body.Close()
	b.quotaGuard.observeHeaders(chatGPTAccountID(original), resp.Header, time.Now())
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("official history restoration returned HTTP %d; no context was discarded", resp.StatusCode)
	}
	var output []byte
	var text strings.Builder
	completed := false
	stopAfterCompleted := errors.New("stop reading after response.completed")
	err = readSSE(io.LimitReader(resp.Body, 16<<20), func(raw []byte) error {
		b.quotaGuard.observeRateLimitPayload(chatGPTAccountID(original), raw, time.Now())
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			return nil
		}
		switch event["type"] {
		case "response.output_text.delta":
			text.WriteString(stringOrEmpty(event["delta"]))
		case "response.completed":
			completed = true
			output, _ = responseOutputFromRawEvent(raw, nil)
			return stopAfterCompleted
		case "error", "response.failed", "response.incomplete":
			return fmt.Errorf("official history restoration did not complete; no context was discarded")
		}
		return nil
	})
	if err != nil && !errors.Is(err, stopAfterCompleted) {
		return "", err
	}
	summary := extractWopalCompactionSummary(output, text.String())
	if !completed || summary == "" {
		return "", fmt.Errorf("official history restoration returned no continuation summary; no context was discarded")
	}
	return summary, nil
}

func (b *nativeCompactionBridge) summarizeChatGPTWebsocket(ctx context.Context, original *http.Request, model string, body []byte) (string, error) {
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		return "", err
	}
	message["type"] = "response.create"
	payload, err := json.Marshal(message)
	if err != nil {
		return "", err
	}

	target := b.chatgptTarget
	endpoint := url.URL{
		Scheme:   target.Scheme,
		Host:     target.Host,
		Path:     joinURLPath(target.Path, "responses"),
		RawQuery: target.RawQuery,
	}
	switch endpoint.Scheme {
	case "http":
		endpoint.Scheme = "ws"
	case "https":
		endpoint.Scheme = "wss"
	}
	header := original.Header.Clone()
	for _, name := range []string{
		"Connection", "Upgrade", "Sec-WebSocket-Key",
		"Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
	} {
		header.Del(name)
	}
	header.Set("X-Codex-Routing-Hint", "model="+model)
	dialer := websocket.Dialer{
		Subprotocols:     websocket.Subprotocols(original),
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   64 * 1024,
		WriteBufferSize:  1 << 20,
	}
	conn, response, err := dialer.DialContext(ctx, endpoint.String(), header)
	if response != nil {
		b.quotaGuard.observeHeaders(chatGPTAccountID(original), response.Header, time.Now())
	}
	if err != nil {
		if response != nil {
			return "", fmt.Errorf("restore native compacted history websocket handshake returned HTTP %d: %w", response.StatusCode, err)
		}
		return "", fmt.Errorf("restore native compacted history websocket handshake: %w", err)
	}
	defer conn.Close()
	if b.logger != nil {
		b.logger.write(map[string]any{"event": "native_compaction_restore_connected", "transport": "websocket", "model": model})
	}
	deadline, ok := ctx.Deadline()
	if ok {
		_ = conn.SetWriteDeadline(deadline)
		_ = conn.SetReadDeadline(deadline)
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return "", fmt.Errorf("restore native compacted history websocket write: %w", err)
	}

	collected := make([]json.RawMessage, 0, 8)
	var text strings.Builder
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("restore native compacted history websocket read: %w", err)
		}
		b.quotaGuard.observeRateLimitPayload(chatGPTAccountID(original), raw, time.Now())
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		typeName := stringOrEmpty(event["type"])
		if typeName == "response.output_item.done" {
			if item := rawOutputItemFromEvent(raw); len(item) > 0 {
				collected = append(collected, item)
			}
		}
		switch typeName {
		case "response.output_text.delta":
			text.WriteString(stringOrEmpty(event["delta"]))
		case "response.completed":
			output, _ := responseOutputFromRawEvent(raw, collected)
			summary := extractWopalCompactionSummary(output, text.String())
			if summary == "" {
				return "", fmt.Errorf("official history restoration returned no continuation summary; no context was discarded")
			}
			return summary, nil
		case "error", "response.failed", "response.incomplete":
			return "", fmt.Errorf("official history restoration did not complete; no context was discarded")
		}
	}
}
