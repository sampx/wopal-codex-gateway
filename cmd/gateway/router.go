package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
)

const (
	maxRoutingBodyBytes        = 64 * 1024 * 1024
	officialQuotaFailoverModel = "wopal-ai/deepseek-v4.1-flash"
)

type routeKind string

const (
	routeUnknown  routeKind = "unknown"
	routeOfficial routeKind = "official"
	routeWopal    routeKind = "wopal-ai"
	routeDisabled routeKind = "disabled"
)

func classifyModel(model string) routeKind {
	model = strings.TrimSpace(model)
	if model == "" {
		return routeUnknown
	}
	if strings.HasPrefix(model, "wopal-ai/") {
		return routeWopal
	}
	if strings.Contains(model, "/") {
		return routeDisabled
	}
	return routeOfficial
}

func routingMiddleware(logger *activity.Logger) gin.HandlerFunc {
	return routingMiddlewareWithQuotaGuard(logger, newOfficialQuotaGuard(logger))
}

func routingMiddlewareWithQuotaGuard(logger *activity.Logger, quotaGuard *officialQuotaGuard) gin.HandlerFunc {
	return routingMiddlewareWithTargetsAndQuotaGuard(
		logger,
		mustURL("https://chatgpt.com/backend-api/codex"),
		mustURL("https://api.openai.com/v1"),
		quotaGuard,
	)
}

func routingMiddlewareWithTargets(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL) gin.HandlerFunc {
	return routingMiddlewareWithTargetsAndQuotaGuard(logger, chatgptTarget, openaiTarget, newOfficialQuotaGuard(logger))
}

func routingMiddlewareWithTargetsAndQuotaGuard(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL, quotaGuard *officialQuotaGuard) gin.HandlerFunc {
	chatgptProxy := newOfficialReverseProxy(chatgptTarget, true, logger, quotaGuard)
	openaiProxy := newOfficialReverseProxy(openaiTarget, false, logger, quotaGuard)
	nativeReplay := &nativeCompactionBridge{chatgptTarget: chatgptTarget, openaiTarget: openaiTarget, logger: logger, quotaGuard: quotaGuard}

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/_health" || path == "/_status" {
			c.Next()
			return
		}
		if path == "/v1/models" || strings.HasPrefix(path, "/v1/models/") {
			c.Request.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
			c.Next()
			return
		}
		if path != "/v1/responses" && path != "/v1/responses/compact" {
			c.Next()
			return
		}

		model, err := detectRequestModel(c.Request)
		if err != nil {
			logger.Write(map[string]any{"event": "route_detection_failed", "path": path, "error": err.Error()})
			c.Next()
			return
		}
		if c.Request.Method == http.MethodPost && isGatewayCompactionRequest(c.Request, path) {
			logger.Write(map[string]any{"event": "route_selected", "provider": "wopal-ai", "model": dedicatedWopalCompactionModel, "path": path, "purpose": "compaction"})
			_ = prepareWopalHTTP(c, logger, dedicatedWopalCompactionModel, nativeReplay)
			return
		}

		switch classifyModel(model) {
		case routeWopal:
			logger.Write(map[string]any{"event": "route_selected", "provider": "wopal-ai", "model": model, "path": path})
			if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
				c.Set("native_compaction_bridge", nativeReplay)
				c.Abort()
				serveWopalWebsocketBridge(c, logger, model)
				return
			}
			if c.Request.Method == http.MethodPost && !prepareWopalHTTP(c, logger, model, nativeReplay) {
				return
			}
			c.Request.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
			c.Request.Header.Del("ChatGPT-Account-ID")
			c.Next()
			return
		case routeOfficial:
			provider := "openai"
			proxy := openaiProxy
			target := openaiTarget
			accountID := chatGPTAccountID(c.Request)
			if accountID != "" {
				provider = "chatgpt"
				proxy = chatgptProxy
				target = chatgptTarget
				if blocked, reason := quotaGuard.blockedModel(accountID, model, time.Now()); blocked {
					logger.Write(map[string]any{"event": "official_quota_failover", "requested_model": model, "effective_model": officialQuotaFailoverModel, "path": path, "reason": reason})
					if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
						c.Set("native_compaction_bridge", nativeReplay)
						c.Set("quota_failover_requested_model", model)
						c.Set("quota_failover_guard", quotaGuard)
						c.Set("quota_failover_account_id", accountID)
						c.Abort()
						serveWopalWebsocketBridge(c, logger, officialQuotaFailoverModel)
						return
					}
					if c.Request.Method == http.MethodPost {
						if err := rewriteRequestModel(c.Request, officialQuotaFailoverModel); err != nil {
							c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "quota_failover_rewrite_failed", "type": "invalid_request_error", "message": err.Error()}})
							return
						}
						if !prepareWopalHTTP(c, logger, officialQuotaFailoverModel, nativeReplay) {
							return
						}
						c.Request.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
						c.Request.Header.Del("ChatGPT-Account-ID")
						c.Next()
						return
					}
					c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": gin.H{"code": "official_quota_guard", "type": "invalid_request_error", "message": "official quota reserve is protected"}})
					return
				}
			}
			logger.Write(map[string]any{"event": "route_selected", "provider": provider, "model": model, "path": path})
			if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
				c.Abort()
				serveOfficialWebsocketBridge(c, logger, target, quotaGuard, accountID, model, nativeReplay)
				return
			}
			rewriteOfficialRequestBody(c.Request, logger)
			c.Abort()
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		case routeDisabled:
			logger.Write(map[string]any{"event": "route_rejected", "model": model, "path": path, "reason": "unsupported_namespace"})
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "model namespace is disabled", "type": "invalid_request_error"}})
			return
		default:
			c.Next()
		}
	}
}

func rewriteRequestModel(req *http.Request, model string) error {
	if req == nil || req.Body == nil {
		return fmt.Errorf("request body is required for quota failover")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRoutingBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read quota failover request: %w", err)
	}
	if len(raw) > maxRoutingBodyBytes {
		return fmt.Errorf("request body exceeds routing limit")
	}
	decoded, err := decodeRoutingBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return fmt.Errorf("parse quota failover request: %w", err)
	}
	payload["model"] = model
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode quota failover request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))
	return nil
}

func isGatewayCompactionRequest(req *http.Request, path string) bool {
	if path == "/v1/responses/compact" {
		return true
	}
	if req == nil || req.Body == nil || path != "/v1/responses" {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRoutingBodyBytes+1))
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if err != nil || len(raw) > maxRoutingBodyBytes {
		return false
	}
	decoded, err := decodeRoutingBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return false
	}
	var payload struct {
		Input []any `json:"input"`
	}
	return json.Unmarshal(decoded, &payload) == nil && wopalInputHasCompactionTrigger(payload.Input)
}

func detectRequestModel(req *http.Request) (string, error) {
	hint := parseRoutingHintModel(req.Header.Get("X-Codex-Routing-Hint"))
	if req.Method == http.MethodGet || req.Body == nil {
		return hint, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRoutingBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read request body for routing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if len(raw) > maxRoutingBodyBytes {
		return "", fmt.Errorf("request body exceeds routing limit")
	}
	decoded, err := decodeRoutingBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return "", err
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", fmt.Errorf("parse request body for routing: %w", err)
	}
	if model := strings.TrimSpace(payload.Model); model != "" {
		return model, nil
	}
	return hint, nil
}

func decodeRoutingBody(raw []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode gzip routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, maxRoutingBodyBytes+1))
	case "deflate":
		reader, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode deflate routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, maxRoutingBodyBytes+1))
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode zstd routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, maxRoutingBodyBytes+1))
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding for routing: %s", encoding)
	}
}

func parseRoutingHintModel(value string) string {
	for _, part := range strings.Split(value, ";") {
		key, raw, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "model") {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

func newOfficialReverseProxy(target *url.URL, chatgpt bool, logger *activity.Logger, quotaGuard *officialQuotaGuard) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			incomingPath := req.URL.Path
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if chatgpt {
				req.URL.Path = joinURLPath(target.Path, stripV1(incomingPath))
			} else {
				req.URL.Path = joinURLPath(target.Path, stripV1(incomingPath))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Write(map[string]any{"event": "official_upstream_error", "upstream": target.Host, "error": err.Error()})
			http.Error(w, "official upstream unavailable", http.StatusBadGateway)
		},
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if chatgpt {
			quotaGuard.observeHeaders(chatGPTAccountID(resp.Request), resp.Header, time.Now())
		}
		logger.Write(map[string]any{
			"event": "official_upstream_connected", "upstream": target.Host,
			"status": resp.StatusCode, "protocol": resp.Proto, "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})
		return nil
	}
	return proxy
}

func stripV1(path string) string {
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return path[3:]
	}
	return path
}

func joinURLPath(base, suffix string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}
