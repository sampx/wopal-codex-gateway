package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/provider"
	"wopal-codex-gateway/internal/routing"
)

type gatewaySnapshot struct {
	gateway  *config.GatewayConfig
	registry *provider.Registry
	routes   *routing.Table
	baseline *catalog.OfficialModelsBaseline
}

func loadGatewaySnapshot(root, gatewayConfigPath string) (*gatewaySnapshot, error) {
	if err := config.SetProjectRoot(root); err != nil {
		return nil, err
	}
	gateway, err := config.LoadGatewayConfig(gatewayConfigPath)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateServeConfig(gateway); err != nil {
		return nil, fmt.Errorf("%s: %w", gatewayConfigPath, err)
	}
	resolved, err := resolveRuntimeModels(gateway)
	if err != nil {
		return nil, err
	}
	registry, err := provider.Build(resolved)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gatewayConfigPath, err)
	}
	baseline, err := catalog.LoadOfficialModelsBaseline()
	if err != nil {
		return nil, err
	}
	verifier, err := catalog.NewOfficialArtifactVerifier(baseline)
	if err != nil {
		return nil, err
	}

	official := make(map[string]struct{}, len(baseline.Slugs))
	for _, slug := range baseline.Slugs {
		official[strings.TrimSpace(slug)] = struct{}{}
	}
	external := make(map[string]routing.Route)
	for _, model := range registry.Models() {
		if model.ProviderID != provider.WopalAIID && strings.TrimSpace(model.Protocol) == "" {
			continue
		}
		if _, collides := official[model.ClientModel]; collides {
			return nil, fmt.Errorf("client-visible model name %q collides with official model", model.ClientModel)
		}
		kind := routing.External
		if model.ProviderID == provider.WopalAIID {
			kind = routing.Wopal
		}
		external[model.ClientModel] = routing.Route{Kind: kind, ProviderID: model.ProviderID}
	}
	routes := routing.NewTable(external, baseline.Slugs, verifier.Verify)
	return &gatewaySnapshot{gateway: resolved, registry: registry, routes: routes, baseline: baseline}, nil
}

type runtimeBackend struct {
	handler  http.Handler
	stop     func()
	logger   *activity.Logger
	target   *url.URL
	active   atomic.Int64
	draining atomic.Bool
	stopOnce sync.Once
}

func newRuntimeBackend(handler http.Handler, stop func()) *runtimeBackend {
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	return &runtimeBackend{handler: handler, stop: stop}
}

func (b *runtimeBackend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	b.active.Add(1)
	defer func() {
		if b.active.Add(-1) == 0 && b.draining.Load() {
			b.stopNow()
		}
	}()
	b.handler.ServeHTTP(w, r)
}

func (b *runtimeBackend) drain() {
	if b == nil {
		return
	}
	b.draining.Store(true)
	if b.active.Load() == 0 {
		b.stopNow()
	}
}

func (b *runtimeBackend) stopNow() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		if b.stop != nil {
			b.stop()
		}
	})
}

type backendSwitcher struct {
	active atomic.Pointer[runtimeBackend]
}

func newBackendSwitcher(initial *runtimeBackend) *backendSwitcher {
	s := &backendSwitcher{}
	if initial != nil {
		s.active.Store(initial)
	}
	return s
}

func (s *backendSwitcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := s.active.Load()
	if backend == nil {
		http.Error(w, "gateway runtime is unavailable", http.StatusServiceUnavailable)
		return
	}
	backend.serveHTTP(w, r)
}

func (s *backendSwitcher) Swap(next *runtimeBackend) {
	if next == nil {
		return
	}
	previous := s.active.Swap(next)
	if previous != nil && previous != next {
		previous.drain()
	}
}

func (s *backendSwitcher) Current() *runtimeBackend {
	if s == nil {
		return nil
	}
	return s.active.Load()
}

func (s *backendSwitcher) Close() {
	previous := s.active.Swap(nil)
	if previous != nil {
		previous.drain()
	}
}
