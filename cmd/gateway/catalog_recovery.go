package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/routing"
)

const (
	defaultCatalogRecoveryBackoff = 2 * time.Second
	defaultCatalogRecoveryTimeout = 10 * time.Second
)

type catalogRecoveryController struct {
	switcher *backendSwitcher
	status   *runtimeCatalogStatus
	recover  func() (*runtimeBackend, error)
	logger   *activity.Logger

	mu           sync.Mutex
	inFlight     chan struct{}
	lastErr      error
	backoffUntil time.Time
	backoff      time.Duration
}

func newCatalogRecoveryController(switcher *backendSwitcher, status *runtimeCatalogStatus, recover func() (*runtimeBackend, error)) *catalogRecoveryController {
	return &catalogRecoveryController{
		switcher: switcher,
		status:   status,
		recover:  recover,
		backoff:  defaultCatalogRecoveryBackoff,
	}
}

func (c *catalogRecoveryController) Resolve(model string) (routing.Route, error) {
	if c == nil || c.switcher == nil {
		return routing.Route{}, fmt.Errorf("gateway runtime is unavailable")
	}
	backend := c.switcher.Current()
	if backend == nil || backend.routes == nil {
		return routing.Route{}, fmt.Errorf("gateway runtime is unavailable")
	}
	route, err := backend.routes.Resolve(model)
	if err == nil || !catalog.IsArtifactVerificationError(err) {
		return route, err
	}
	if recoveryErr := c.ensureRecovered(backend); recoveryErr != nil {
		return routing.Route{}, &routing.CatalogRefreshError{Err: recoveryErr}
	}
	backend = c.switcher.Current()
	if backend == nil || backend.routes == nil {
		return routing.Route{}, &routing.CatalogRefreshError{Err: fmt.Errorf("catalog refreshed but runtime snapshot is unavailable")}
	}
	route, err = backend.routes.Resolve(model)
	if catalog.IsArtifactVerificationError(err) {
		return routing.Route{}, &routing.CatalogRefreshError{Err: fmt.Errorf("catalog refreshed but native artifact is still not current: %w", err)}
	}
	return route, err
}

func (c *catalogRecoveryController) ensureRecovered(observed *runtimeBackend) error {
	for {
		c.mu.Lock()
		if c.switcher.Current() != observed {
			c.mu.Unlock()
			return nil
		}
		if c.lastErr != nil && time.Now().Before(c.backoffUntil) {
			err := c.lastErr
			c.mu.Unlock()
			return err
		}
		if c.inFlight != nil {
			wait := c.inFlight
			c.mu.Unlock()
			<-wait
			continue
		}
		wait := make(chan struct{})
		c.inFlight = wait
		c.mu.Unlock()

		if c.status != nil {
			c.status.MarkRefreshing()
		}
		if c.logger != nil {
			c.logger.Write(map[string]any{"event": "catalog_recovery_started"})
		}
		var next *runtimeBackend
		var err error
		if c.recover == nil {
			err = fmt.Errorf("catalog recovery is unavailable")
		} else {
			next, err = c.recover()
		}
		if err == nil && next == nil {
			err = fmt.Errorf("catalog recovery returned no runtime snapshot")
		}
		if err == nil {
			c.switcher.Swap(next)
		}

		c.mu.Lock()
		if err != nil {
			c.lastErr = err
			c.backoffUntil = time.Now().Add(c.backoff)
		} else {
			c.lastErr = nil
			c.backoffUntil = time.Time{}
		}
		close(wait)
		c.inFlight = nil
		c.mu.Unlock()

		if err != nil {
			if c.status != nil {
				c.status.MarkAutoFailure(err)
			}
			if c.logger != nil {
				c.logger.Write(map[string]any{"event": "catalog_recovery_failed", "error": err.Error()})
			}
			return err
		}
		if c.status != nil {
			c.status.MarkAutoSuccess(next.baseline)
		}
		if c.logger != nil {
			c.logger.Write(map[string]any{"event": "catalog_recovery_succeeded", "baseline_sha256": baselineSHA(next.baseline)})
		}
		return nil
	}
}

func baselineSHA(baseline *catalog.OfficialModelsBaseline) string {
	if baseline == nil {
		return ""
	}
	return baseline.Artifact.SHA256
}

func recoverCatalogRuntime(
	root, gatewayConfigPath, instance string,
	listen config.ListenConfig,
	logger *activity.Logger,
	reloadStatus *runtimeReloadStatus,
	resolve func(string) (routing.Route, error),
) (*runtimeBackend, error) {
	cfg, err := config.LoadGatewayConfig(gatewayConfigPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultCatalogRecoveryTimeout)
	defer cancel()
	result, err := catalog.RefreshCatalogContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	candidate, err := reloadGatewayBackendWithResolver(root, gatewayConfigPath, instance, listen, logger, reloadStatus, resolve)
	if err != nil {
		return nil, err
	}
	if candidate.baseline == nil || result.Baseline == nil || candidate.baseline.Artifact.SHA256 != result.Baseline.Artifact.SHA256 {
		candidate.stopNow()
		return nil, fmt.Errorf("catalog refresh committed but candidate runtime did not load the committed baseline")
	}
	if candidate.verifier == nil {
		candidate.stopNow()
		return nil, fmt.Errorf("catalog refresh candidate has no artifact verifier")
	}
	if _, err := candidate.verifier.Check(); err != nil {
		candidate.stopNow()
		return nil, fmt.Errorf("catalog refresh candidate verification failed: %w", err)
	}
	return candidate, nil
}

// prepareInitialCatalog performs the startup recovery attempt before the front
// door is opened. Failure is deliberately non-fatal when the persisted
// baseline itself is valid: runServe can still start the previous snapshot and
// serve namespaced external routes in degraded mode.
func prepareInitialCatalog(root, gatewayConfigPath string, snapshot *gatewaySnapshot, status *runtimeCatalogStatus, logger *activity.Logger) {
	if snapshot == nil || snapshot.artifactVerifier == nil {
		return
	}
	identity, err := snapshot.artifactVerifier.Check()
	if err == nil {
		status.MarkCurrent(snapshot.baseline, identity.SHA256)
		return
	}
	status.MarkRefreshing()
	logger.Write(map[string]any{"event": "catalog_recovery_started", "phase": "startup"})
	ctx, cancel := context.WithTimeout(context.Background(), defaultCatalogRecoveryTimeout)
	defer cancel()
	result, refreshErr := catalog.RefreshCatalogContext(ctx, snapshot.gateway)
	if refreshErr != nil {
		status.MarkAutoFailure(refreshErr)
		logger.Write(map[string]any{"event": "catalog_recovery_failed", "phase": "startup", "error": refreshErr.Error()})
		return
	}
	fresh, loadErr := loadGatewaySnapshot(root, gatewayConfigPath)
	if loadErr != nil {
		status.MarkAutoFailure(loadErr)
		logger.Write(map[string]any{"event": "catalog_recovery_failed", "phase": "startup", "error": loadErr.Error()})
		return
	}
	current, verifyErr := fresh.artifactVerifier.Check()
	if verifyErr != nil || result.Baseline == nil || fresh.baseline.Artifact.SHA256 != result.Baseline.Artifact.SHA256 {
		if verifyErr == nil {
			verifyErr = fmt.Errorf("startup recovery baseline did not match committed refresh")
		}
		status.MarkAutoFailure(verifyErr)
		logger.Write(map[string]any{"event": "catalog_recovery_failed", "phase": "startup", "error": verifyErr.Error()})
		return
	}
	status.MarkAutoSuccess(fresh.baseline)
	status.MarkCurrent(fresh.baseline, current.SHA256)
	logger.Write(map[string]any{"event": "catalog_recovery_succeeded", "phase": "startup", "baseline_sha256": fresh.baseline.Artifact.SHA256})
}
