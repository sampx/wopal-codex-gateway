package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
	return routingMiddlewareWithResolverAndTargetsAndQuotaGuard(
		logger,
		mustURL("https://chatgpt.com/backend-api/codex"),
		mustURL("https://api.openai.com/v1"),
		quotaGuard,
		legacyRouteResolver,
	)
}

func RoutingMiddlewareWithResolver(logger *activity.Logger, quotaGuard *quota.Guard, resolve RouteResolver) gin.HandlerFunc {
	return routingMiddlewareWithResolverAndTargetsAndQuotaGuard(
		logger,
		mustURL("https://chatgpt.com/backend-api/codex"),
		mustURL("https://api.openai.com/v1"),
		quotaGuard,
		resolve,
	)
}

func RoutingMiddlewareWithResolverAndRoles(logger *activity.Logger, quotaGuard *quota.Guard, resolve RouteResolver, roles RuntimeRoles) gin.HandlerFunc {
	return routingMiddlewareWithRuntime(
		logger,
		mustURL("https://chatgpt.com/backend-api/codex"),
		mustURL("https://api.openai.com/v1"),
		quotaGuard,
		resolve,
		roles,
	)
}

func routingMiddlewareWithTargets(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL) gin.HandlerFunc {
	return routingMiddlewareWithTargetsAndQuotaGuard(logger, chatgptTarget, openaiTarget, quota.NewGuard(logger))
}

func routingMiddlewareWithTargetsAndQuotaGuard(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL, quotaGuard *quota.Guard) gin.HandlerFunc {
	return routingMiddlewareWithResolverAndTargetsAndQuotaGuard(logger, chatgptTarget, openaiTarget, quotaGuard, legacyRouteResolver)
}

func routingMiddlewareWithResolverAndTargetsAndQuotaGuard(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL, quotaGuard *quota.Guard, resolve RouteResolver) gin.HandlerFunc {
	return routingMiddlewareWithRuntime(logger, chatgptTarget, openaiTarget, quotaGuard, resolve, defaultRuntimeRoles())
}

func routingMiddlewareWithRuntime(logger *activity.Logger, chatgptTarget, openaiTarget *url.URL, quotaGuard *quota.Guard, resolve RouteResolver, roles RuntimeRoles) gin.HandlerFunc {
	chatgptProxy := newOfficialReverseProxy(chatgptTarget, true, logger, quotaGuard)
	openaiProxy := newOfficialReverseProxy(openaiTarget, false, logger, quotaGuard)
	nativeReplay := compaction.NewNativeBridge(chatgptTarget, openaiTarget, logger, quotaGuard)
	clientWatcher := clientfp.NewWatcher(64)
	selfLoopToken := newSelfLoopToken()

	return func(c *gin.Context) {
		// Every request gets the process-local token once. Clones and contexts
		// derived from this request retain it, so every Wopal websocket entry
		// point can identify its own loopback hop without shared mutable state.
		c.Request = attachSelfLoopToken(c.Request, selfLoopToken)
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

		inspection, err := inspectRequest(c.Request)
		if err != nil {
			logger.Write(map[string]any{"event": "route_detection_failed", "path": path, "error": err.Error()})
			c.Next()
			return
		}
		model, shape := inspection.model, inspection.shape
		// Record which Codex build sent this request. Codex ships several times
		// a week and a release can change the request contract without breaking
		// a build, so the version and the observed shape are written next to the
		// routing decision instead of being reconstructed after a failure.
		fingerprint := clientfp.From(c.Request.Header)
		clientFields := fingerprint.Fields()
		internalHop := isSelfLoopRequest(c.Request, selfLoopToken)
		if !internalHop && shape.ToolSurface != "" && shape.ToolSurface != "none" {
			for key, value := range shape.Fields() {
				clientFields[key] = value
			}
			observeClientContract(logger, clientWatcher, c.Request.Header, model, shape)
		}
		if c.Request.Method == http.MethodPost && isGatewayCompactionRequest(inspection, path) {
			if !roles.Compaction.Ready || strings.TrimSpace(roles.Compaction.ClientModel) == "" {
				abortModelRoleUnavailable(c, "compaction")
				return
			}
			logger.Write(map[string]any{"event": "route_selected", "provider": roles.Compaction.ProviderID, "model": roles.Compaction.ClientModel, "path": path, "purpose": "compaction"})
			_ = prepareExternalHTTP(c, logger, roles.Compaction.ClientModel, roles.Compaction.ProviderID, roles.Compaction.ClientModel, nativeReplay, inspection)
			return
		}

		route, routeErr := resolve(model)
		if routeErr != nil {
			logger.Write(withClient(map[string]any{"event": "route_rejected", "model": model, "path": path, "reason": "not_registered", "error": routeErr.Error()}, clientFields))
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"message": routeErr.Error(), "type": "invalid_request_error", "code": "model_route_unavailable"}})
			return
		}
		switch route.Kind {
		case routing.Wopal:
			logger.Write(withClient(map[string]any{"event": "route_selected", "provider": "wopal-ai", "model": model, "path": path}, clientFields))
			if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
				c.Set("native_compaction_bridge", nativeReplay)
				c.Set(clientContractWatcherContextKey, clientWatcher)
				c.Abort()
				serveExternalWebsocketBridgeWithCompaction(c, logger, model, route.ProviderID, resolve, readyRoleModel(roles.Compaction))
				return
			}
			if c.Request.Method == http.MethodPost && !prepareExternalHTTP(c, logger, model, route.ProviderID, readyRoleModel(roles.Compaction), nativeReplay, inspection) {
				return
			}
			c.Request.Header.Set("Authorization", "Bearer "+config.LocalAccessKey)
			c.Request.Header.Del("ChatGPT-Account-ID")
			c.Next()
			return
		case routing.External:
			logger.Write(withClient(map[string]any{"event": "route_selected", "provider": route.ProviderID, "model": model, "path": path}, clientFields))
			if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
				c.Set("native_compaction_bridge", nativeReplay)
				c.Set(clientContractWatcherContextKey, clientWatcher)
				c.Abort()
				serveExternalWebsocketBridgeWithCompaction(c, logger, model, route.ProviderID, resolve, readyRoleModel(roles.Compaction))
				return
			}
			if c.Request.Method == http.MethodPost && !prepareExternalHTTP(c, logger, model, route.ProviderID, readyRoleModel(roles.Compaction), nativeReplay, inspection) {
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
				effectiveFallback := readyRoleModel(roles.OfficialFallback)
				if blocked, reason := quotaGuard.BlockedModelWithTarget(accountID, model, effectiveFallback, time.Now()); blocked {
					if effectiveFallback == "" {
						abortModelRoleUnavailable(c, "officialFallback")
						return
					}
					logger.Write(map[string]any{"event": "official_quota_failover", "requested_model": model, "effective_model": effectiveFallback, "path": path, "reason": reason})
					if c.Request.Method == http.MethodGet && path == "/v1/responses" && isWebsocketUpgrade(c.Request) {
						c.Set("native_compaction_bridge", nativeReplay)
						c.Set(clientContractWatcherContextKey, clientWatcher)
						c.Set("quota_failover_requested_model", model)
						c.Set("quota_failover_guard", quotaGuard)
						c.Set("quota_failover_account_id", accountID)
						c.Abort()
						serveExternalWebsocketBridgeWithCompaction(c, logger, effectiveFallback, roles.OfficialFallback.ProviderID, resolve, readyRoleModel(roles.Compaction))
						return
					}
					if c.Request.Method == http.MethodPost {
						if err := rewriteRequestModel(c.Request, effectiveFallback, inspection); err != nil {
							c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "quota_failover_rewrite_failed", "type": "invalid_request_error", "message": err.Error()}})
							return
						}
						if !prepareExternalHTTP(c, logger, effectiveFallback, roles.OfficialFallback.ProviderID, readyRoleModel(roles.Compaction), nativeReplay, inspection) {
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
				serveOfficialWebsocketBridgeWithRuntime(c, logger, target, quotaGuard, accountID, model, nativeReplay, resolve, roles)
				return
			}
			sanitizeOfficialInspection(c.Request, inspection, logger)
			c.Abort()
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		default:
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "model route is unavailable", "type": "invalid_request_error", "code": "model_route_unavailable"}})
			return
		}
	}
}

func readyRoleModel(role RoleTarget) string {
	if !role.Ready {
		return ""
	}
	return strings.TrimSpace(role.ClientModel)
}

func abortModelRoleUnavailable(c *gin.Context, role string) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
		"code": "model_role_unavailable", "type": "server_error", "message": fmt.Sprintf("model role %s is unavailable", role),
	}})
}

// selfLoopToken distinguishes a Wopal websocket's loopback HTTP request from
// an external POST. It is deliberately not authentication: the SDK still
// checks the local access key. The token only keeps an implementation detail
// out of the client-contract watcher, whose subject is the original Codex
// client rather than the gateway's own compactor request.
type selfLoopContextKey struct{}

const (
	selfLoopTokenHeader             = "X-Wopal-Internal-Token"
	clientContractWatcherContextKey = "wopal_client_contract_watcher"
)

func newSelfLoopToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("generate self-loop token: %v", err))
	}
	return hex.EncodeToString(raw[:])
}

func attachSelfLoopToken(req *http.Request, token string) *http.Request {
	if req == nil || token == "" {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), selfLoopContextKey{}, token))
}

func selfLoopTokenFor(req *http.Request) string {
	if req == nil {
		return ""
	}
	token, _ := req.Context().Value(selfLoopContextKey{}).(string)
	return token
}

func isSelfLoopRequest(req *http.Request, token string) bool {
	if req == nil || token == "" {
		return false
	}
	return subtleConstantTimeEqual(req.Header.Get(selfLoopTokenHeader), token)
}

func subtleConstantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// copySelfLoopFingerprint carries only non-secret client identity metadata to
// the gateway's own loopback hop. Authentication and account headers are never
// copied.
func copySelfLoopFingerprint(dst *http.Request, src *http.Request) {
	if dst == nil || src == nil {
		return
	}
	token := selfLoopTokenFor(src)
	if token == "" {
		return
	}
	dst.Header.Set(selfLoopTokenHeader, token)
	for _, name := range []string{"version", "originator", "User-Agent", "x-codex-beta-features"} {
		if value := src.Header.Get(name); value != "" {
			dst.Header.Set(name, value)
		}
	}
}

// observeWebsocketClientContract observes only the shapes which define a
// request contract. Incremental frames may omit model; use the model bound to
// this socket so an omitted field cannot collapse distinct sessions into one
// watcher key.
func observeWebsocketClientContract(logger *activity.Logger, watcher *clientfp.Watcher, header http.Header, boundModel, requestType string, shape clientfp.Shape) {
	if requestType != "response.create" && requestType != "response.append" {
		return
	}
	model := strings.TrimSpace(shape.Model)
	if model == "" {
		model = strings.TrimSpace(boundModel)
	}
	observeClientContract(logger, watcher, header, model, shape)
}

// observeClientContract records a transition for the original client request.
// It is shared by HTTP inspection and the Wopal websocket frame loop; generated
// loopback requests must not call it.
func observeClientContract(logger *activity.Logger, watcher *clientfp.Watcher, header http.Header, model string, shape clientfp.Shape) {
	if watcher == nil || shape.ToolSurface == "" || shape.ToolSurface == "none" {
		return
	}
	fingerprint := clientfp.From(header)
	if change := watcher.Observed(fingerprint.Version, model, shape, fingerprint.Encoding, fingerprint.BetaFeatures); change != nil {
		fields := change.Fields()
		for key, value := range fingerprint.Fields() {
			if _, ok := fields[key]; !ok {
				fields[key] = value
			}
		}
		logger.Write(fields)
	}
}

func rewriteRequestModel(req *http.Request, model string, inspection *requestInspection) error {
	if inspection == nil || inspection.payload == nil {
		return fmt.Errorf("request body is required for quota failover")
	}
	inspection.payload["model"] = model
	return writeInspection(req, inspection)
}

func writeInspection(req *http.Request, inspection *requestInspection) error {
	body, err := json.Marshal(inspection.payload)
	if err != nil {
		return err
	}
	inspection.decoded = body
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}

func isGatewayCompactionRequest(inspection *requestInspection, path string) bool {
	if path == "/v1/responses/compact" {
		return true
	}
	if inspection == nil || inspection.payload == nil || path != "/v1/responses" {
		return false
	}
	input, _ := inspection.payload["input"].([]any)
	return routing.InputHasCompactionTrigger(input)
}

// replayedBody reconstructs a partially read request body without erasing its
// terminal error. A reader that saw n > 0 plus a non-EOF error must yield those
// bytes first, then the same error, then EOF. ContentLength is set to -1 so the
// transport does not reinterpret the restored byte count as a complete body.
type replayingReadCloser struct {
	reader io.Reader
}

func (b *replayingReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (*replayingReadCloser) Close() error { return nil }

type terminalErrorReader struct {
	err error
}

func (r *terminalErrorReader) Read([]byte) (int, error) {
	err := r.err
	r.err = io.EOF
	return 0, err
}

func replayedBody(raw []byte, terminal error) io.ReadCloser {
	if terminal == nil {
		return io.NopCloser(bytes.NewReader(raw))
	}
	if len(raw) == 0 {
		return io.NopCloser(&terminalErrorReader{err: terminal})
	}
	return &replayingReadCloser{reader: io.MultiReader(bytes.NewReader(raw), &terminalErrorReader{err: terminal})}
}

// requestInspection is one request body read and one decoded parse, shared by
// routing, shape logging, compaction detection, quota rewrite and Wopal HTTP
// preparation. It is scoped to a single request; never cache it in a context
// that a new self-loop request will inherit.
type requestInspection struct {
	raw     []byte
	decoded []byte
	payload map[string]any
	shape   clientfp.Shape
	model   string
}

// inspectRequest parses a POST body once. It preserves the exact original bytes,
// headers and ContentLength on the request so an untouched official request can
// pass through byte-for-byte. Parsing is strict (exactly one JSON value) and
// uses UseNumber so rewrites retain numeric fidelity.
func inspectRequest(req *http.Request) (*requestInspection, error) {
	return inspectRequestWithLimit(req, routing.MaxBodyBytes)
}

func inspectRequestWithLimit(req *http.Request, limit int64) (*requestInspection, error) {
	hint := routing.ParseRoutingHintModel(req.Header.Get("X-Codex-Routing-Hint"))
	if req.Method == http.MethodGet || req.Body == nil {
		return &requestInspection{model: hint}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	// Restore the bytes already read, then replay the terminal read error before
	// exposing them. The request context may continue into another handler when
	// inspection fails, so silently substituting a complete bytes.Reader would
	// turn a truncated request into successful inference.
	if err != nil {
		req.Body = replayedBody(raw, err)
		req.ContentLength = -1
		return nil, fmt.Errorf("read request body for routing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	// Inspection must not normalize a chunked/unknown-length request.
	if int64(len(raw)) > limit {
		req.ContentLength = int64(len(raw))
		return nil, fmt.Errorf("request body exceeds routing limit")
	}
	decoded, err := compaction.DecodeBody(raw, req.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("decoded request exceeds routing limit")
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse request body for routing: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("parse request body for routing: %w", err)
	}
	shape := clientfp.ShapeFromBody(payload)
	model := shape.Model
	if model == "" {
		model = hint
	}
	return &requestInspection{raw: raw, decoded: decoded, payload: payload, shape: shape, model: model}, nil
}

// sanitizeOfficialInspection reuses the parsed payload and rewrites only when
// the official sanitizer actually changes it. Otherwise the original bytes,
// headers and compression stay exactly as received.
func sanitizeOfficialInspection(req *http.Request, inspection *requestInspection, logger *activity.Logger) {
	if inspection == nil || inspection.payload == nil {
		return
	}
	if !compaction.SanitizeParsedRequest(inspection.payload, logger) {
		return
	}
	if err := writeInspection(req, inspection); err != nil {
		logger.Write(map[string]any{"event": "official_sanitizer_encode_failed", "error": err.Error()})
	}
}

func newOfficialReverseProxy(target *url.URL, chatgpt bool, logger *activity.Logger, quotaGuard *quota.Guard) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = routing.JoinURLPath(target.Path, stripV1(req.URL.Path))
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
