package provider

import (
	"fmt"
	"strings"

	"wopal-codex-gateway/internal/config"
)

type RoleName string

const (
	RoleOfficialFallback RoleName = "officialFallback"
	RoleCompaction       RoleName = "compaction"
)

type RoleSource string

const (
	RoleDefault  RoleSource = "default"
	RoleExplicit RoleSource = "explicit"
)

type RoleStatus string

const (
	RoleReady       RoleStatus = "ready"
	RoleUnavailable RoleStatus = "unavailable"
	RoleInvalid     RoleStatus = "invalid"
)

type ResolvedRole struct {
	Name          RoleName
	ProviderID    string
	UpstreamModel string
	ClientModel   string
	Source        RoleSource
	Status        RoleStatus
	Reason        string
}

func configuredRole(cfg *config.GatewayConfig, name RoleName) (*config.ModelRoleRef, RoleSource) {
	if cfg != nil {
		switch name {
		case RoleOfficialFallback:
			if cfg.ModelRoles.OfficialFallback != nil {
				value := cfg.ModelRoles.OfficialFallback.Clone()
				return &value, RoleExplicit
			}
		case RoleCompaction:
			if cfg.ModelRoles.Compaction != nil {
				value := cfg.ModelRoles.Compaction.Clone()
				return &value, RoleExplicit
			}
		}
	}
	return &config.ModelRoleRef{Provider: config.DefaultModelRoleProvider, Model: config.DefaultModelRoleModel}, RoleDefault
}

func ResolveRole(cfg *config.GatewayConfig, registry *Registry, name RoleName) ResolvedRole {
	ref, source := configuredRole(cfg, name)
	resolved := ResolvedRole{
		Name:          name,
		ProviderID:    strings.TrimSpace(ref.Provider),
		UpstreamModel: strings.TrimSpace(ref.Model),
		Source:        source,
	}
	invalid := func(reason string) ResolvedRole {
		resolved.Reason = reason
		if source == RoleDefault {
			resolved.Status = RoleUnavailable
		} else {
			resolved.Status = RoleInvalid
		}
		return resolved
	}
	if resolved.ProviderID == "" || resolved.UpstreamModel == "" {
		return invalid("provider and model are required")
	}
	if resolved.ProviderID == OfficialID {
		return invalid("role target must be an external provider model")
	}
	if registry == nil {
		return invalid("provider registry is unavailable")
	}
	if _, ok := registry.Provider(resolved.ProviderID); !ok {
		return invalid(fmt.Sprintf("provider %q is not configured", resolved.ProviderID))
	}
	model, ok := registry.ModelByProvider(resolved.ProviderID, resolved.UpstreamModel)
	if !ok {
		return invalid(fmt.Sprintf("model %q is not configured for provider %q", resolved.UpstreamModel, resolved.ProviderID))
	}
	if strings.TrimSpace(model.Protocol) == "" {
		return invalid(fmt.Sprintf("model %q for provider %q needs protocol", resolved.UpstreamModel, resolved.ProviderID))
	}
	resolved.ClientModel = model.ClientModel
	resolved.Status = RoleReady
	return resolved
}

func (r *Registry) ModelByProvider(providerID, upstreamModel string) (ModelRef, bool) {
	if r == nil {
		return ModelRef{}, false
	}
	providerID = strings.TrimSpace(providerID)
	upstreamModel = strings.TrimSpace(upstreamModel)
	for _, model := range r.models {
		if model.ProviderID == providerID && model.UpstreamModel == upstreamModel {
			return model, true
		}
	}
	return ModelRef{}, false
}

func ValidateExplicitRoles(cfg *config.GatewayConfig, registry *Registry) error {
	for _, name := range []RoleName{RoleOfficialFallback, RoleCompaction} {
		resolved := ResolveRole(cfg, registry, name)
		if resolved.Source == RoleExplicit && resolved.Status != RoleReady {
			return fmt.Errorf("model role %s is invalid: %s", name, resolved.Reason)
		}
	}
	return nil
}
