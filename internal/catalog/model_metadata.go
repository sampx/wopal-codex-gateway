package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
)

// ModelMetadataSnapshot contains the provider identity, collection time and
// explicit capabilities only. Credentials and raw responses are never retained.
type ModelMetadataSnapshot struct {
	BaseURL   string                       `json:"baseUrl"`
	FetchedAt time.Time                    `json:"fetchedAt"`
	Models    []config.ExternalModelConfig `json:"models"`
}

// ModelCapabilitySources reports where each effective capability came from.
type ModelCapabilitySources struct {
	ContextWindow string
	Vision        string
	Reasoning     string
}

// ResolveModel applies user > provider > default independently per field. The
// snapshot must belong to the current provider (LoadModelMetadata checks this).
// Every returned capability has its own pointer, independent of both inputs.
func ResolveModel(model config.ExternalModelConfig, snapshot *ModelMetadataSnapshot) (config.ExternalModelConfig, ModelCapabilitySources) {
	resolved := cloneExternalModel(model)
	sources := ModelCapabilitySources{ContextWindow: "user", Vision: "user", Reasoning: "user"}
	var provider config.ExternalModelConfig
	if snapshot != nil {
		for _, candidate := range snapshot.Models {
			if candidate.Name == model.Name {
				provider = candidate
				break
			}
		}
	}
	if resolved.ContextWindow == nil {
		value := config.DefaultContextWindow
		sources.ContextWindow = "default"
		if provider.ContextWindow != nil && *provider.ContextWindow > 0 {
			value = *provider.ContextWindow
			sources.ContextWindow = "provider"
		}
		resolved.ContextWindow = &value
	}
	if resolved.Vision == nil {
		value := (config.ExternalModelConfig{}).SupportsVision()
		sources.Vision = "default"
		if provider.Vision != nil {
			value = *provider.Vision
			sources.Vision = "provider"
		}
		resolved.Vision = &value
	}
	if resolved.Reasoning == nil {
		value := (config.ExternalModelConfig{}).SupportsReasoning()
		sources.Reasoning = "default"
		if provider.Reasoning != nil {
			value = *provider.Reasoning
			sources.Reasoning = "provider"
		}
		resolved.Reasoning = &value
	}
	return resolved, sources
}

func normalizeMetadataBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func validateMetadataBaseURL(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("model metadata base URL must be HTTP(S) without credentials, query or fragment")
	}
	return nil
}

func validateModelMetadata(snapshot *ModelMetadataSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("model metadata snapshot is nil")
	}
	if err := validateMetadataBaseURL(snapshot.BaseURL); err != nil {
		return err
	}
	if snapshot.FetchedAt.IsZero() || snapshot.Models == nil {
		return fmt.Errorf("model metadata requires fetchedAt and models[]")
	}
	for _, model := range snapshot.Models {
		if strings.TrimSpace(model.Name) == "" || (model.ContextWindow != nil && *model.ContextWindow <= 0) {
			return fmt.Errorf("model metadata contains invalid model capabilities")
		}
	}
	return nil
}

// SaveModelMetadata atomically replaces runtime/model-metadata.json with a
// private, typed snapshot. A failed validation or write keeps the previous file.
func SaveModelMetadata(snapshot *ModelMetadataSnapshot) error {
	if err := validateModelMetadata(snapshot); err != nil {
		return err
	}
	root, err := config.ProjectRootDir()
	if err != nil {
		return fmt.Errorf("resolve model metadata path: %w", err)
	}
	path := config.ModelMetadataPathFor(root)
	normalized := *snapshot
	normalized.BaseURL = normalizeMetadataBaseURL(snapshot.BaseURL)
	payload, err := json.MarshalIndent(&normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model metadata: %w", err)
	}
	// Include the trailing newline in the exact bytes persisted and bounded.
	payload = append(payload, '\n')
	if len(payload) > maxModelMetadataBytes {
		return fmt.Errorf("model metadata exceeds %d byte limit", maxModelMetadataBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create model metadata directory: %w", err)
	}
	if err := config.WriteFileAtomic(path, payload, 0o600); err != nil {
		return fmt.Errorf("write model metadata: %w", err)
	}
	return nil
}

// LoadModelMetadata returns nil for an absent snapshot or a different provider.
// A malformed cache is an explicit error, never silently treated as no data.
func LoadModelMetadata(baseURL string) (*ModelMetadataSnapshot, error) {
	root, err := config.ProjectRootDir()
	if err != nil {
		return nil, fmt.Errorf("resolve model metadata path: %w", err)
	}
	path := config.ModelMetadataPathFor(root)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model metadata: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxModelMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read model metadata: %w", err)
	}
	if len(data) > maxModelMetadataBytes {
		return nil, fmt.Errorf("model metadata exceeds %d byte limit", maxModelMetadataBytes)
	}
	var snapshot *ModelMetadataSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("parse model metadata %s: %w", path, err)
	}
	if err := validateModelMetadata(snapshot); err != nil {
		return nil, fmt.Errorf("invalid model metadata %s: %w", path, err)
	}
	snapshot.BaseURL = normalizeMetadataBaseURL(snapshot.BaseURL)
	if snapshot.BaseURL != normalizeMetadataBaseURL(baseURL) {
		return nil, nil
	}
	return snapshot, nil
}
