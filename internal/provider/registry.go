// Package provider owns the normalized provider and model identity used by
// CLI, catalog and daemon code. It deliberately does not perform network I/O.
package provider

import (
	"fmt"
	"sort"
	"strings"

	"wopal-codex-gateway/internal/config"
)

const (
	OfficialID = "official"
	WopalAIID  = "wopal-ai"
)

type Kind string

const (
	KindOfficial Kind = "official"
	KindWopalAI  Kind = "wopal-ai"
	KindExternal Kind = "external"
)

type Entry struct {
	ID     string
	Kind   Kind
	Config config.ProviderConfig
}

type ModelRef struct {
	ProviderID    string
	UpstreamModel string
	ClientModel   string
	Protocol      string
}

type Registry struct {
	providers map[string]Entry
	models    map[string]ModelRef
}

// Build normalizes the three provider sources into one lookup surface. The
// official provider has no configured model list yet; its bare slugs are
// supplied by the official baseline in the catalog/routing tasks.
func Build(cfg *config.GatewayConfig) (*Registry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gateway config is nil")
	}
	if err := cfg.ValidateProviders(); err != nil {
		return nil, err
	}

	registry := &Registry{
		providers: map[string]Entry{
			OfficialID: {ID: OfficialID, Kind: KindOfficial},
			WopalAIID:  {ID: WopalAIID, Kind: KindWopalAI},
		},
		models: make(map[string]ModelRef),
	}

	for _, model := range cfg.WopalAI.Models {
		upstream := strings.TrimSpace(model.Name)
		if upstream == "" {
			return nil, fmt.Errorf("wopal-ai has a model with an empty name")
		}
		ref := ModelRef{
			ProviderID:    WopalAIID,
			UpstreamModel: upstream,
			ClientModel:   WopalAIID + "/" + upstream,
			Protocol:      config.ProtocolOpenAIChat,
		}
		if err := registry.addModel(ref); err != nil {
			return nil, err
		}
	}

	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		providerConfig := cfg.Providers[id].Clone()
		registry.providers[id] = Entry{ID: id, Kind: KindExternal, Config: providerConfig}
		for _, model := range providerConfig.Models {
			protocol := strings.TrimSpace(model.Protocol)
			if protocol == "" {
				protocol = strings.TrimSpace(providerConfig.Protocol)
			}
			ref := ModelRef{
				ProviderID:    id,
				UpstreamModel: strings.TrimSpace(model.Name),
				ClientModel:   ClientVisibleName(id, model),
				Protocol:      protocol,
			}
			if err := registry.addModel(ref); err != nil {
				return nil, err
			}
		}
	}
	return registry, nil
}

func (r *Registry) addModel(ref ModelRef) error {
	if ref.ClientModel == "" {
		return fmt.Errorf("provider %q model %q resolves to an empty client-visible name", ref.ProviderID, ref.UpstreamModel)
	}
	if previous, exists := r.models[ref.ClientModel]; exists {
		return fmt.Errorf("duplicate client-visible model %q: %s/%s and %s/%s", ref.ClientModel, previous.ProviderID, previous.UpstreamModel, ref.ProviderID, ref.UpstreamModel)
	}
	r.models[ref.ClientModel] = ref
	return nil
}

func ClientVisibleName(providerID string, model config.ExternalModelConfig) string {
	if alias := strings.TrimSpace(model.Alias); alias != "" {
		return alias
	}
	upstream := strings.TrimSpace(model.Name)
	if upstream == "" {
		return ""
	}
	return providerID + "/" + upstream
}

func (r *Registry) Provider(id string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	entry, ok := r.providers[id]
	if !ok {
		return Entry{}, false
	}
	entry.Config = entry.Config.Clone()
	return entry, true
}

func (r *Registry) Model(clientVisibleName string) (ModelRef, bool) {
	if r == nil {
		return ModelRef{}, false
	}
	ref, ok := r.models[clientVisibleName]
	return ref, ok
}

func (r *Registry) Providers() []Entry {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		entry, _ := r.Provider(id)
		entries = append(entries, entry)
	}
	return entries
}

func (r *Registry) Models() []ModelRef {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	sort.Strings(names)
	models := make([]ModelRef, 0, len(names))
	for _, name := range names {
		models = append(models, r.models[name])
	}
	return models
}
