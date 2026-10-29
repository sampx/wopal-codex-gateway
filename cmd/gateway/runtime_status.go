package main

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/proxy"
)

type runtimeReloadSnapshot struct {
	Status                string
	Error                 string
	UsingPreviousSnapshot bool
	UpdatedAt             time.Time
}

type runtimeReloadStatus struct {
	mu       sync.RWMutex
	snapshot runtimeReloadSnapshot
	catalog  *runtimeCatalogStatus
}

type gatewayServiceRuntime struct {
	Roles         proxy.RuntimeRoles
	Credentials   map[string]bool
	Reload        *runtimeReloadStatus
	Catalog       *runtimeCatalogStatus
	StatusGateway *config.GatewayConfig
}

func newRuntimeReloadStatus() *runtimeReloadStatus {
	return &runtimeReloadStatus{snapshot: runtimeReloadSnapshot{Status: "ok", UpdatedAt: time.Now().UTC()}}
}

func (s *runtimeReloadStatus) MarkFailure(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = runtimeReloadSnapshot{
		Status:                "failed",
		UsingPreviousSnapshot: true,
		UpdatedAt:             time.Now().UTC(),
	}
	if err != nil {
		s.snapshot.Error = strings.TrimSpace(err.Error())
	}
}

func (s *runtimeReloadStatus) MarkSuccess() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = runtimeReloadSnapshot{Status: "ok", UpdatedAt: time.Now().UTC()}
}

func (s *runtimeReloadStatus) Snapshot() runtimeReloadSnapshot {
	if s == nil {
		return runtimeReloadSnapshot{Status: "ok"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func sanitizeStatusError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const maxRunes = 400
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "..."
	}
	return value
}

func catalogStatusPayload(snapshot runtimeCatalogSnapshot) map[string]any {
	status := strings.TrimSpace(snapshot.Status)
	if status == "" {
		status = "current"
	}
	payload := map[string]any{"status": status}
	if snapshot.Baseline != nil {
		if !snapshot.Baseline.UpdatedAt.IsZero() {
			payload["baseline_updated_at"] = snapshot.Baseline.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if sha := strings.TrimSpace(snapshot.Baseline.Artifact.SHA256); sha != "" {
			payload["baseline_sha256"] = sha
		}
	}
	if sha := strings.TrimSpace(snapshot.CurrentSHA256); sha != "" {
		payload["current_sha256"] = sha
	}
	if !snapshot.CheckedAt.IsZero() {
		payload["checked_at"] = snapshot.CheckedAt.UTC().Format(time.RFC3339)
	}
	if snapshot.LastAutoStatus != "" {
		last := map[string]any{"status": snapshot.LastAutoStatus}
		if !snapshot.LastAutoUpdated.IsZero() {
			last["updated_at"] = snapshot.LastAutoUpdated.UTC().Format(time.RFC3339)
		}
		if text := sanitizeStatusError(snapshot.LastAutoError); text != "" {
			last["error"] = text
		}
		payload["last_auto_refresh"] = last
	}
	return payload
}

func roleStatusPayload(target proxy.RoleTarget) map[string]any {
	status := strings.TrimSpace(target.Status)
	if status == "" {
		if target.Ready {
			status = "ready"
		} else {
			status = "unavailable"
		}
	}
	payload := map[string]any{
		"provider":     target.ProviderID,
		"client_model": target.ClientModel,
		"status":       status,
	}
	if target.UpstreamModel != "" {
		payload["upstream_model"] = target.UpstreamModel
	}
	if target.Source != "" {
		payload["source"] = target.Source
	}
	return payload
}

func buildRuntimeStatusPayload(
	gateway *config.GatewayConfig,
	roles proxy.RuntimeRoles,
	credentials map[string]bool,
	reload runtimeReloadSnapshot,
	catalogSnapshot runtimeCatalogSnapshot,
	officialQuota map[string]any,
	officialFailover map[string]any,
	notifications map[string]any,
	instance string,
	requestBodyLogging string,
) map[string]any {
	providers := map[string]any{
		"official": map[string]any{
			"kind":              "official",
			"credential_source": "client-request",
		},
		"wopal-ai": map[string]any{
			"kind":                  "external",
			"credential_configured": credentials["wopal-ai"],
		},
	}
	listen := ""
	if gateway != nil {
		listen = gateway.Listen.Host + ":" + formatPort(gateway.Listen.Port)
		for providerID, providerCfg := range gateway.Providers {
			providers[providerID] = map[string]any{
				"kind":                  "external",
				"credential_configured": credentials[providerID],
				"models":                len(providerCfg.Models),
			}
		}
	}

	reloadPayload := map[string]any{
		"status":                  reload.Status,
		"using_previous_snapshot": reload.UsingPreviousSnapshot,
	}
	if reload.Error != "" {
		reloadPayload["error"] = reload.Error
	}
	if !reload.UpdatedAt.IsZero() {
		reloadPayload["updated_at"] = reload.UpdatedAt.UTC().Format(time.RFC3339)
	}

	return map[string]any{
		"ok":     true,
		"listen": listen,
		"service": map[string]any{
			"status":   "running",
			"instance": instance,
		},
		"providers": providers,
		"roles": map[string]any{
			"officialFallback": roleStatusPayload(roles.OfficialFallback),
			"compaction":       roleStatusPayload(roles.Compaction),
		},
		"official":             "native-pass-through",
		"official_quota":       officialQuota,
		"official_failover":    officialFailover,
		"notifications":        notifications,
		"reload":               reloadPayload,
		"catalog":              catalogStatusPayload(catalogSnapshot),
		"wopal":                "cliproxy-responses-to-chat",
		"request_body_logging": requestBodyLogging,
	}
}

func formatPort(port int) string {
	return strconv.Itoa(port)
}
