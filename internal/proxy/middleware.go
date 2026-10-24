package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/clientfp"
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

func RoutingMiddleware(logger *activity.Logger, quotaGuard *quota.Guard) gin.HandlerFunc {
	return routingMiddlewareWithTargetsAndQuotaGuard(
		logger,
		mustURL("https://chatgpt.com/backend-api/codex"),
		mustURL("https://api.openai.com/v1"),
		quotaGuard,
	)
}

func routingMiddlewareWithTargets(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL) gin.HandlerFunc {
	return routingMiddlewareWithTargetsAndQuotaGuard(logger, chatgptTarget, openaiTarget, quota.NewGuard(logger))
}

func routingMiddlewareWithTargetsAndQuotaGuard(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL, quotaGuard *quota.Guard) gin.HandlerFunc {
	chatgptProxy := newOfficialReverseProxy(chatgptTarget, true, logger, quotaGuard)
	openaiProxy := newOfficialReverseProxy(openaiTarget, false, logger, quotaGuard)
	nativeReplay := compaction.NewNativeBridge(chatgptTarget, openaiTarget, logger, quotaGuard)
	clientWatcher := clientfp.NewWatcher(64)

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

		model, shape, err := detectRequestShape(c.Request)
		if err != nil {
			logger.Write(map[string]any{"event": "route_detection_failed", "path": path, "error": err.Error()})
			c.Next()
			return
		}
		// Record which Codex build sent this request. Codex ships several times
		// a week and a release can change the request contract without breaking
		// a build, so the version and the observed shape are written next to the
		// routing decision instead of being reconstructed after a failure.
		fingerprint := clientfp.From(c.Request.Header)
		clientFields := fingerprint.Fields()
		if shape.ToolSurface != "" && shape.ToolSurface != "none" {
			for key, value := range shape.Fields() {
				clientFields[key] = value
			}
			if change := clientWatcher.Observed(fingerprint.Version, model, shape, fingerprint.Encoding, fingerprint.BetaFeatures); change != nil {
				fields := change.Fields()
				for key, value := range fingerprint.Fields() {
					if _, ok := fields[key]; !ok {
						fields[key] = value
					}
				}
				logger.Write(fields)
			}
		}
		if c.Request.Method == http.MethodPost && isGatewayCompactionRequest(c.Request, path) {
			logger.Write(map[string]any{"event": "route_selected", "provider": "wopal-ai", "model": compaction.DedicatedModel, "path": path, "purpose": "compaction"})
			_ = prepareWopalHTTP(c, logger, compaction.DedicatedModel, nativeReplay)
			return
		}

		switch routing.Classify(model) {
		case routing.Wopal:
			logger.Write(withClient(map[string]any{"event": "route_selected", "provider": "wopal-ai", "model": model, "path": path}, clientFields))
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
		case routing.Official:
			provider := "openai"
			proxy := openaiProxy
			target := openaiTarget
			accountID := routing.AccountID(c.Request)
			if accountID != "" {
				provider = "chatgpt"
				proxy = chatgptProxy
				target = chatgptTarget
				if blocked, reason := quotaGuard.BlockedModel(accountID, model, time.Now()); blocked {
					logger.Write(map[string]any{"event": "official_quota_failover", "requested_model": model, "effective_model": routing.OfficialQuotaFailoverModel, "path": path, "reason": reason})
					if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
						c.Set("native_compaction_bridge", nativeReplay)
						c.Set("quota_failover_requested_model", model)
						c.Set("quota_failover_guard", quotaGuard)
						c.Set("quota_failover_account_id", accountID)
						c.Abort()
						serveWopalWebsocketBridge(c, logger, routing.OfficialQuotaFailoverModel)
						return
					}
					if c.Request.Method == http.MethodPost {
						if err := rewriteRequestModel(c.Request, routing.OfficialQuotaFailoverModel); err != nil {
							c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "quota_failover_rewrite_failed", "type": "invalid_request_error", "message": err.Error()}})
							return
						}
						if !prepareWopalHTTP(c, logger, routing.OfficialQuotaFailoverModel, nativeReplay) {
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
			logger.Write(withClient(map[string]any{"event": "route_selected", "provider": provider, "model": model, "path": path}, clientFields))
			if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
				c.Abort()
				serveOfficialWebsocketBridge(c, logger, target, quotaGuard, accountID, model, nativeReplay)
				return
			}
			compaction.RewriteOfficialRequestBody(c.Request, logger)
			c.Abort()
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		case routing.Disabled:
			logger.Write(withClient(map[string]any{"event": "route_rejected", "model": model, "path": path, "reason": "unsupported_namespace"}, clientFields))
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
	raw, err := io.ReadAll(io.LimitReader(req.Body, routing.MaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read quota failover request: %w", err)
	}
	if len(raw) > routing.MaxBodyBytes {
		return fmt.Errorf("request body exceeds routing limit")
	}
	decoded, err := compaction.DecodeBody(raw, req.Header.Get("Content-Encoding"))
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
	raw, err := io.ReadAll(io.LimitReader(req.Body, routing.MaxBodyBytes+1))
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if err != nil || len(raw) > routing.MaxBodyBytes {
		return false
	}
	decoded, err := compaction.DecodeBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return false
	}
	var payload struct {
		Input []any `json:"input"`
	}
	return json.Unmarshal(decoded, &payload) == nil && routing.InputHasCompactionTrigger(payload.Input)
}

// detectRequestShape resolves the model and, for requests that carry a body,
// summarizes the part of the request that tells the gateway how this Codex
// build talks to it. The body is read once and restored, and the decoded copy is
// reused for the summary so a request is never decoded twice.
func detectRequestShape(req *http.Request) (string, clientfp.Shape, error) {
	hint := routing.ParseRoutingHintModel(req.Header.Get("X-Codex-Routing-Hint"))
	if req.Method == http.MethodGet || req.Body == nil {
		return hint, clientfp.Shape{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, routing.MaxBodyBytes+1))
	if err != nil {
		return "", clientfp.Shape{}, fmt.Errorf("read request body for routing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	if len(raw) > routing.MaxBodyBytes {
		return "", clientfp.Shape{}, fmt.Errorf("request body exceeds routing limit")
	}
	decoded, err := compaction.DecodeBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return "", clientfp.Shape{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", clientfp.Shape{}, fmt.Errorf("parse request body for routing: %w", err)
	}
	shape := clientfp.ShapeFromBody(payload)
	if model := shape.Model; model != "" {
		return model, shape, nil
	}
	return hint, shape, nil
}

func newOfficialReverseProxy(target *url.URL, chatgpt bool, logger *activity.Logger, quotaGuard *quota.Guard) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			incomingPath := req.URL.Path
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if chatgpt {
				req.URL.Path = routing.JoinURLPath(target.Path, stripV1(incomingPath))
			} else {
				req.URL.Path = routing.JoinURLPath(target.Path, stripV1(incomingPath))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Write(map[string]any{"event": "official_upstream_error", "upstream": target.Host, "error": err.Error()})
			http.Error(w, "official upstream unavailable", http.StatusBadGateway)
		},
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if chatgpt {
			quotaGuard.ObserveHeaders(routing.AccountID(resp.Request), resp.Header, time.Now())
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
func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

// withClient copies base and adds the client fingerprint fields. It keeps the
// existing event payloads readable while making every routing decision
// attributable to a specific Codex build.
func withClient(base map[string]any, clientFields map[string]any) map[string]any {
	if len(clientFields) == 0 {
		return base
	}
	for key, value := range clientFields {
		base[key] = value
	}
	return base
}
