package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/routing"
)

func TestCatalogRecoveryControllerSingleFlightsDriftAndReresolvesTrigger(t *testing.T) {
	var recoverCalls atomic.Int32
	drift := &catalog.ArtifactVerificationError{Cause: errors.New("native Codex artifact changed")}
	old := newRuntimeBackend(nil, nil)
	old.routes = routing.NewTable(nil, []string{"gpt-old"}, func() error { return drift })
	switcher := newBackendSwitcher(old)
	status := newRuntimeCatalogStatus(nil)
	controller := newCatalogRecoveryController(switcher, status, func() (*runtimeBackend, error) {
		recoverCalls.Add(1)
		time.Sleep(20 * time.Millisecond)
		next := newRuntimeBackend(nil, nil)
		next.routes = routing.NewTable(nil, []string{"gpt-new"}, nil)
		return next, nil
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			route, err := controller.Resolve("gpt-new")
			if err != nil {
				errCh <- err
				return
			}
			if route.Kind != routing.Official {
				errCh <- errors.New("recovered request did not resolve official model")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := recoverCalls.Load(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
}

func TestCatalogRecoveryControllerBacksOffAfterFailure(t *testing.T) {
	var recoverCalls atomic.Int32
	drift := &catalog.ArtifactVerificationError{Cause: errors.New("native Codex artifact changed")}
	old := newRuntimeBackend(nil, nil)
	old.routes = routing.NewTable(map[string]routing.Route{
		"vendor/model": {Kind: routing.External, ProviderID: "vendor"},
	}, []string{"gpt-old"}, func() error { return drift })
	switcher := newBackendSwitcher(old)
	controller := newCatalogRecoveryController(switcher, newRuntimeCatalogStatus(nil), func() (*runtimeBackend, error) {
		recoverCalls.Add(1)
		return nil, errors.New("refresh failed")
	})
	controller.backoff = time.Minute

	for range 2 {
		_, err := controller.Resolve("gpt-old")
		if !routing.IsCatalogRefreshError(err) {
			t.Fatalf("Resolve error = %v, want catalog refresh error", err)
		}
	}
	if got := recoverCalls.Load(); got != 1 {
		t.Fatalf("recover calls during backoff = %d, want 1", got)
	}
	if switcher.Current() != old {
		t.Fatal("failed recovery replaced the active runtime snapshot")
	}
	route, err := controller.Resolve("vendor/model")
	if err != nil || route.Kind != routing.External || route.ProviderID != "vendor" {
		t.Fatalf("namespaced external route after failed recovery = %+v, %v", route, err)
	}
}
