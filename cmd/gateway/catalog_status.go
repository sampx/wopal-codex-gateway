package main

import (
	"strings"
	"sync"
	"time"

	"wopal-codex-gateway/internal/catalog"
)

type runtimeCatalogSnapshot struct {
	Status          string
	Baseline        *catalog.OfficialModelsBaseline
	CurrentSHA256   string
	CheckedAt       time.Time
	LastAutoStatus  string
	LastAutoUpdated time.Time
	LastAutoError   string
}

type runtimeCatalogStatus struct {
	mu       sync.RWMutex
	snapshot runtimeCatalogSnapshot
}

func newRuntimeCatalogStatus(baseline *catalog.OfficialModelsBaseline) *runtimeCatalogStatus {
	return &runtimeCatalogStatus{snapshot: runtimeCatalogSnapshot{Status: "current", Baseline: baseline}}
}

func (s *runtimeCatalogStatus) MarkRefreshing() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot.Status = "refreshing"
	s.snapshot.CheckedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *runtimeCatalogStatus) MarkCurrent(baseline *catalog.OfficialModelsBaseline, currentSHA string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot.Status = "current"
	s.snapshot.Baseline = baseline
	s.snapshot.CurrentSHA256 = strings.TrimSpace(currentSHA)
	s.snapshot.CheckedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *runtimeCatalogStatus) MarkAutoSuccess(baseline *catalog.OfficialModelsBaseline) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.snapshot.Status = "current"
	s.snapshot.Baseline = baseline
	if baseline != nil {
		s.snapshot.CurrentSHA256 = baseline.Artifact.SHA256
	}
	s.snapshot.CheckedAt = now
	s.snapshot.LastAutoStatus = "ok"
	s.snapshot.LastAutoUpdated = now
	s.snapshot.LastAutoError = ""
	s.mu.Unlock()
}

func (s *runtimeCatalogStatus) MarkAutoFailure(err error) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.snapshot.Status = "refresh_failed"
	s.snapshot.CheckedAt = now
	s.snapshot.LastAutoStatus = "failed"
	s.snapshot.LastAutoUpdated = now
	if err != nil {
		s.snapshot.LastAutoError = strings.TrimSpace(err.Error())
	}
	s.mu.Unlock()
}

func (s *runtimeCatalogStatus) Snapshot() runtimeCatalogSnapshot {
	if s == nil {
		return runtimeCatalogSnapshot{Status: "current"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func (s *runtimeReloadStatus) SetCatalog(status *runtimeCatalogStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.catalog = status
	s.mu.Unlock()
}

func (s *runtimeReloadStatus) Catalog() *runtimeCatalogStatus {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catalog
}
