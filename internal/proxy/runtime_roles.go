package proxy

import (
	"strings"

	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/routing"
)

type RoleTarget struct {
	ProviderID    string
	UpstreamModel string
	ClientModel   string
	Source        string
	Status        string
	Ready         bool
}

type RuntimeRoles struct {
	OfficialFallback RoleTarget
	Compaction       RoleTarget
}

// defaultCompactionRole is the compaction target a bridge uses when callers do
// not supply a runtime snapshot. It mirrors the default runtime role so a
// session built through the legacy entry points still names a real provider.
func defaultCompactionRole() RoleTarget {
	return RoleTarget{ProviderID: "wopal-ai", ClientModel: compaction.DedicatedModel, Ready: true}
}

// readyRoleTarget returns the role only when it is servable. A role that is not
// ready carries no client model, so downstream code fails closed with
// "model_role_unavailable" instead of routing a compaction to a model the
// registry never resolved.
func readyRoleTarget(role RoleTarget) RoleTarget {
	if !role.Ready || strings.TrimSpace(role.ClientModel) == "" {
		return RoleTarget{}
	}
	return role
}

func defaultRuntimeRoles() RuntimeRoles {
	return RuntimeRoles{
		OfficialFallback: RoleTarget{ProviderID: "wopal-ai", ClientModel: routing.OfficialQuotaFailoverModel, Ready: true},
		Compaction:       RoleTarget{ProviderID: "wopal-ai", ClientModel: routing.OfficialQuotaFailoverModel, Ready: true},
	}
}
