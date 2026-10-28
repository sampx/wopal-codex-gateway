package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
)

const (
	ModelsDevCatalogURL      = "https://models.dev/catalog.json"
	maxModelsDevCatalogBytes = 16 * 1024 * 1024
	maxProviderMetadataBytes = 4 * 1024 * 1024
)

var modelsDevHTTPClient = &http.Client{Timeout: 30 * time.Second}

// ModelsDevProvider is the provider-scoped slice read from models.dev before
// joining it with the provider's available/configured model set. It is never
// persisted directly because the full upstream catalog is intentionally not a
// runtime state file.
type ModelsDevProvider struct {
	Provider  string
	NPM       string
	ETag      string
	FetchedAt time.Time
	Models    map[string]ModelsDevModel
}

type ModelsDevModel struct {
	Name          string
	NPM           string
	Shape         string
	ContextWindow *int
	Vision        *bool
	Reasoning     *bool
}

type ProviderModelMetadata struct {
	Name           string `json:"name"`
	Protocol       string `json:"protocol,omitempty"`
	ProtocolSource string `json:"protocolSource,omitempty"`
	ContextWindow  *int   `json:"contextWindow,omitempty"`
	Vision         *bool  `json:"vision,omitempty"`
	Reasoning      *bool  `json:"reasoning,omitempty"`
}

type ProviderModelMetadataSnapshot struct {
	Provider  string                  `json:"provider"`
	ETag      string                  `json:"etag,omitempty"`
	FetchedAt time.Time               `json:"fetchedAt"`
	Models    []ProviderModelMetadata `json:"models"`
}

type providerMetadataStore struct {
	Providers map[string]ProviderModelMetadataSnapshot `json:"providers"`
}

type ProviderModelSources struct {
	Protocol      string
	ContextWindow string
	Vision        string
	Reasoning     string
}

// ResolveProviderModel keeps protocol precedence independent from capability
// precedence. Protocol has no default: an unresolved model stays unroutable.
func ResolveProviderModel(model config.ExternalModelConfig, provider config.ProviderConfig, snapshot *ProviderModelMetadataSnapshot) (config.ExternalModelConfig, ProviderModelSources) {
	resolved := model.Clone()
	sources := ProviderModelSources{
		Protocol:      "needs_protocol",
		ContextWindow: "explicit",
		Vision:        "explicit",
		Reasoning:     "explicit",
	}
	var metadata ProviderModelMetadata
	if snapshot != nil && snapshot.Provider != "" {
		for _, candidate := range snapshot.Models {
			if candidate.Name == model.Name {
				metadata = candidate
				break
			}
		}
	}

	if strings.TrimSpace(resolved.Protocol) != "" {
		resolved.Protocol = strings.TrimSpace(resolved.Protocol)
		sources.Protocol = "explicit"
	} else if metadata.Protocol != "" {
		resolved.Protocol = metadata.Protocol
		sources.Protocol = "metadata"
	} else if strings.TrimSpace(provider.Protocol) != "" {
		resolved.Protocol = strings.TrimSpace(provider.Protocol)
		sources.Protocol = "provider-default"
	} else {
		resolved.Protocol = ""
	}

	if resolved.ContextWindow == nil {
		value := config.DefaultContextWindow
		sources.ContextWindow = "default"
		if metadata.ContextWindow != nil && *metadata.ContextWindow > 0 {
			value = *metadata.ContextWindow
			sources.ContextWindow = "metadata"
		}
		resolved.ContextWindow = &value
	}
	if resolved.Vision == nil {
		value := true
		sources.Vision = "default"
		if metadata.Vision != nil {
			value = *metadata.Vision
			sources.Vision = "metadata"
		}
		resolved.Vision = &value
	}
	if resolved.Reasoning == nil {
		value := true
		sources.Reasoning = "default"
		if metadata.Reasoning != nil {
			value = *metadata.Reasoning
			sources.Reasoning = "metadata"
		}
		resolved.Reasoning = &value
	}
	return resolved, sources
}

func protocolFromModelsDev(shape, modelNPM, providerNPM string) string {
	switch strings.TrimSpace(shape) {
	case "responses":
		return config.ProtocolOpenAIResponses
	case "completions":
		return config.ProtocolOpenAIChat
	}
	npm := strings.TrimSpace(modelNPM)
	if npm == "" {
		npm = strings.TrimSpace(providerNPM)
	}
	switch npm {
	case "@ai-sdk/openai":
		return config.ProtocolOpenAIResponses
	case "@ai-sdk/openai-compatible":
		return config.ProtocolOpenAIChat
	case "@ai-sdk/anthropic":
		return config.ProtocolAnthropicMessages
	case "@ai-sdk/google":
		return config.ProtocolGemini
	default:
		return ""
	}
}

func JoinProviderMetadata(source *ModelsDevProvider, modelNames []string) *ProviderModelMetadataSnapshot {
	providerID := ""
	etag := ""
	fetchedAt := time.Now().UTC()
	if source != nil {
		providerID = source.Provider
		etag = source.ETag
		if !source.FetchedAt.IsZero() {
			fetchedAt = source.FetchedAt
		}
	}
	seen := make(map[string]struct{}, len(modelNames))
	models := make([]ProviderModelMetadata, 0, len(modelNames))
	for _, rawName := range modelNames {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		joined := ProviderModelMetadata{Name: name}
		if source != nil {
			if model, ok := source.Models[name]; ok {
				joined.Protocol = protocolFromModelsDev(model.Shape, model.NPM, source.NPM)
				if joined.Protocol != "" {
					joined.ProtocolSource = "metadata"
				}
				joined.ContextWindow = cloneInt(model.ContextWindow)
				joined.Vision = cloneBool(model.Vision)
				joined.Reasoning = cloneBool(model.Reasoning)
			}
		}
		models = append(models, joined)
	}
	return &ProviderModelMetadataSnapshot{Provider: providerID, ETag: etag, FetchedAt: fetchedAt, Models: models}
}

// RejoinPreviousProviderMetadata updates a persisted joined slice when the
// available/configured model set only shrank while models.dev returned 304.
// If a requested model was not in the previous slice, callers must refetch the
// unchanged upstream catalog without ETag because the persisted state
// intentionally does not retain metadata for models that were not joined.
func RejoinPreviousProviderMetadata(previous *ProviderModelMetadataSnapshot, modelNames []string) (*ProviderModelMetadataSnapshot, bool) {
	if previous == nil {
		return nil, false
	}
	byName := make(map[string]ProviderModelMetadata, len(previous.Models))
	for _, model := range previous.Models {
		byName[model.Name] = model
	}
	joined := &ProviderModelMetadataSnapshot{
		Provider:  previous.Provider,
		ETag:      previous.ETag,
		FetchedAt: time.Now().UTC(),
		Models:    make([]ProviderModelMetadata, 0, len(modelNames)),
	}
	seen := make(map[string]struct{}, len(modelNames))
	for _, rawName := range modelNames {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		model, ok := byName[name]
		if !ok {
			return nil, false
		}
		model.ContextWindow = cloneInt(model.ContextWindow)
		model.Vision = cloneBool(model.Vision)
		model.Reasoning = cloneBool(model.Reasoning)
		joined.Models = append(joined.Models, model)
	}
	return joined, true
}

// FetchModelsDevProvider fetches only enough of catalog.json to extract one
// provider. It uses ETag for normal refreshes and never persists the raw body.
func FetchModelsDevProvider(catalogURL, providerID, etag string) (*ModelsDevProvider, bool, error) {
	req, err := http.NewRequest(http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("models.dev: create request: %w", err)
	}
	if strings.TrimSpace(etag) != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := modelsDevHTTPClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("models.dev: request catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("models.dev: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsDevCatalogBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("models.dev: read catalog: %w", err)
	}
	if len(body) > maxModelsDevCatalogBytes {
		return nil, false, fmt.Errorf("models.dev: catalog exceeds %d byte limit", maxModelsDevCatalogBytes)
	}
	var payload struct {
		Providers map[string]struct {
			NPM    string `json:"npm"`
			Models map[string]struct {
				ID         string `json:"id"`
				Reasoning  *bool  `json:"reasoning"`
				Modalities struct {
					Input []string `json:"input"`
				} `json:"modalities"`
				Limit struct {
					Context int `json:"context"`
				} `json:"limit"`
				Provider struct {
					NPM   string `json:"npm"`
					Shape string `json:"shape"`
				} `json:"provider"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("models.dev: parse catalog: %w", err)
	}
	entry, ok := payload.Providers[providerID]
	if !ok {
		return &ModelsDevProvider{Provider: providerID, ETag: resp.Header.Get("ETag"), FetchedAt: time.Now().UTC(), Models: map[string]ModelsDevModel{}}, false, nil
	}
	result := &ModelsDevProvider{
		Provider:  providerID,
		NPM:       entry.NPM,
		ETag:      resp.Header.Get("ETag"),
		FetchedAt: time.Now().UTC(),
		Models:    make(map[string]ModelsDevModel, len(entry.Models)),
	}
	for key, raw := range entry.Models {
		name := strings.TrimSpace(raw.ID)
		if name == "" {
			name = strings.TrimSpace(key)
		}
		if name == "" {
			continue
		}
		model := ModelsDevModel{Name: name, NPM: strings.TrimSpace(raw.Provider.NPM), Shape: strings.TrimSpace(raw.Provider.Shape)}
		if raw.Limit.Context > 0 {
			model.ContextWindow = intPtr(raw.Limit.Context)
		}
		if raw.Reasoning != nil {
			model.Reasoning = boolPtr(*raw.Reasoning)
		}
		if raw.Modalities.Input != nil {
			vision := false
			for _, modality := range raw.Modalities.Input {
				if modality == "image" {
					vision = true
				}
			}
			model.Vision = boolPtr(vision)
		}
		result.Models[name] = model
	}
	return result, false, nil
}

func SaveProviderModelMetadata(snapshot *ProviderModelMetadataSnapshot) error {
	if snapshot == nil || strings.TrimSpace(snapshot.Provider) == "" || snapshot.FetchedAt.IsZero() || snapshot.Models == nil {
		return fmt.Errorf("provider model metadata requires provider, fetchedAt and models[]")
	}
	root, err := config.ProjectRootDir()
	if err != nil {
		return fmt.Errorf("resolve provider metadata path: %w", err)
	}
	path := config.ProviderModelMetadataPathFor(root)
	store, err := loadProviderMetadataStore(path)
	if err != nil {
		return err
	}
	if store.Providers == nil {
		store.Providers = map[string]ProviderModelMetadataSnapshot{}
	}
	copySnapshot := cloneProviderSnapshot(snapshot)
	store.Providers[snapshot.Provider] = *copySnapshot
	payload, err := json.MarshalIndent(&store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provider model metadata: %w", err)
	}
	payload = append(payload, '\n')
	if len(payload) > maxProviderMetadataBytes {
		return fmt.Errorf("provider model metadata exceeds %d byte limit", maxProviderMetadataBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create provider metadata directory: %w", err)
	}
	if err := config.WriteFileAtomic(path, payload, 0o600); err != nil {
		return fmt.Errorf("write provider model metadata: %w", err)
	}
	return nil
}

func LoadProviderModelMetadata(providerID string) (*ProviderModelMetadataSnapshot, error) {
	root, err := config.ProjectRootDir()
	if err != nil {
		return nil, fmt.Errorf("resolve provider metadata path: %w", err)
	}
	store, err := loadProviderMetadataStore(config.ProviderModelMetadataPathFor(root))
	if err != nil {
		return nil, err
	}
	snapshot, ok := store.Providers[providerID]
	if !ok {
		return nil, nil
	}
	return cloneProviderSnapshot(&snapshot), nil
}

func loadProviderMetadataStore(path string) (*providerMetadataStore, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return &providerMetadataStore{Providers: map[string]ProviderModelMetadataSnapshot{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read provider model metadata: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProviderMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read provider model metadata: %w", err)
	}
	if len(data) > maxProviderMetadataBytes {
		return nil, fmt.Errorf("provider model metadata exceeds %d byte limit", maxProviderMetadataBytes)
	}
	var store providerMetadataStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse provider model metadata %s: %w", path, err)
	}
	if store.Providers == nil {
		store.Providers = map[string]ProviderModelMetadataSnapshot{}
	}
	return &store, nil
}

func cloneProviderSnapshot(snapshot *ProviderModelMetadataSnapshot) *ProviderModelMetadataSnapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	clone.Models = make([]ProviderModelMetadata, len(snapshot.Models))
	for i, model := range snapshot.Models {
		clone.Models[i] = model
		clone.Models[i].ContextWindow = cloneInt(model.ContextWindow)
		clone.Models[i].Vision = cloneBool(model.Vision)
		clone.Models[i].Reasoning = cloneBool(model.Reasoning)
	}
	sort.SliceStable(clone.Models, func(i, j int) bool { return clone.Models[i].Name < clone.Models[j].Name })
	return &clone
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func intPtr(value int) *int    { return &value }
func boolPtr(value bool) *bool { return &value }
