package proxy

import "wopal-codex-gateway/internal/routing"

type RoleTarget struct {
	ProviderID  string
	ClientModel string
	Ready       bool
}

type RuntimeRoles struct {
	OfficialFallback RoleTarget
	Compaction       RoleTarget
}

func defaultRuntimeRoles() RuntimeRoles {
	return RuntimeRoles{
		OfficialFallback: RoleTarget{ProviderID: "wopal-ai", ClientModel: routing.OfficialQuotaFailoverModel, Ready: true},
		Compaction:       RoleTarget{ProviderID: "wopal-ai", ClientModel: routing.OfficialQuotaFailoverModel, Ready: true},
	}
}
