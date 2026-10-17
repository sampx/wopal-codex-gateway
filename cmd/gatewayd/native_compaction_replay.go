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
)

// Only the owning provider can read a native capsule. Convert it once into a
// text continuation before sending the conversation to a third-party model.
// The bounded cache contains no credentials; its keys include the auth scope.
type nativeCompactionBridge struct {
	chatgptTarget, openaiTarget *url.URL
	mu                          sync.Mutex
	summaries                   map[[32]byte]string
	logger                      *activityLogger
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
	target := b.openaiTarget
	if original.Header.Get("ChatGPT-Account-ID") != "" {
		target = b.chatgptTarget
	}
	endpoint := *target
	endpoint.Path = joinURLPath(target.Path, "responses")
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("official history restoration returned HTTP %d; no context was discarded", resp.StatusCode)
	}
	var output []byte
	var text strings.Builder
	completed := false
	stopAfterCompleted := errors.New("stop reading after response.completed")
	err = readSSE(io.LimitReader(resp.Body, 16<<20), func(raw []byte) error {
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
