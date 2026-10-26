package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
)

type OfficialModelsBaseline struct {
	Artifact  NativeArtifactIdentity `json:"artifact"`
	Slugs     []string               `json:"slugs"`
	UpdatedAt time.Time              `json:"updatedAt"`
}

func newOfficialModelsBaseline(native []map[string]any, artifact NativeArtifactIdentity) (*OfficialModelsBaseline, error) {
	seen := make(map[string]struct{}, len(native))
	slugs := make([]string, 0, len(native))
	for _, model := range native {
		slug, _ := model["slug"].(string)
		slug = strings.TrimSpace(slug)
		if slug == "" {
			return nil, fmt.Errorf("official bundled catalog contains a model without slug")
		}
		if _, exists := seen[slug]; exists {
			return nil, fmt.Errorf("official bundled catalog contains duplicate slug %q", slug)
		}
		seen[slug] = struct{}{}
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return &OfficialModelsBaseline{Artifact: artifact, Slugs: slugs, UpdatedAt: time.Now().UTC()}, nil
}

func encodeOfficialModelsBaseline(baseline *OfficialModelsBaseline) ([]byte, error) {
	if baseline == nil || strings.TrimSpace(baseline.Artifact.CanonicalPath) == "" || strings.TrimSpace(baseline.Artifact.SHA256) == "" || len(baseline.Slugs) == 0 {
		return nil, fmt.Errorf("official model baseline requires artifact identity and slugs")
	}
	copyBaseline := *baseline
	copyBaseline.Slugs = append([]string(nil), baseline.Slugs...)
	sort.Strings(copyBaseline.Slugs)
	if copyBaseline.UpdatedAt.IsZero() {
		copyBaseline.UpdatedAt = time.Now().UTC()
	}
	payload, err := json.MarshalIndent(&copyBaseline, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode official model baseline: %w", err)
	}
	return append(payload, '\n'), nil
}

func SaveOfficialModelsBaseline(baseline *OfficialModelsBaseline) error {
	payload, err := encodeOfficialModelsBaseline(baseline)
	if err != nil {
		return err
	}
	path := config.OfficialModelsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create official baseline directory: %w", err)
	}
	if err := catalogWriteAtomic(path, payload, 0o600); err != nil {
		return fmt.Errorf("write official model baseline: %w", err)
	}
	return nil
}

func LoadOfficialModelsBaseline() (*OfficialModelsBaseline, error) {
	path := config.OfficialModelsPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read official model baseline: %w", err)
	}
	var baseline OfficialModelsBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return nil, fmt.Errorf("parse official model baseline %s: %w", path, err)
	}
	if _, err := encodeOfficialModelsBaseline(&baseline); err != nil {
		return nil, fmt.Errorf("invalid official model baseline %s: %w", path, err)
	}
	return &baseline, nil
}

func ValidateExternalNamesAgainstOfficial(cfg *config.GatewayConfig) error {
	if cfg == nil {
		return fmt.Errorf("gateway config is nil")
	}
	hasAlias := false
	for _, provider := range cfg.Providers {
		for _, model := range provider.Models {
			if strings.TrimSpace(model.Alias) != "" {
				hasAlias = true
				break
			}
		}
	}
	if !hasAlias {
		return nil
	}
	baseline, err := LoadOfficialModelsBaseline()
	if err != nil {
		return err
	}
	if baseline == nil {
		return fmt.Errorf("official model baseline is unavailable; run catalog refresh before setting a bare alias")
	}
	official := make(map[string]struct{}, len(baseline.Slugs))
	for _, slug := range baseline.Slugs {
		official[slug] = struct{}{}
	}
	for providerID, provider := range cfg.Providers {
		for _, model := range provider.Models {
			alias := strings.TrimSpace(model.Alias)
			if alias == "" {
				continue
			}
			if _, collides := official[alias]; collides {
				return fmt.Errorf("provider %q model %q client-visible model name %q collides with official model", providerID, model.Name, alias)
			}
		}
	}
	return nil
}
