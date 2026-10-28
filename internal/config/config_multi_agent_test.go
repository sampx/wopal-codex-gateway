package config

import (
	"encoding/json"
	"os"
	"testing"
)

// A Codex parent agent delegates to a sub-agent by putting the task in a tool
// argument the client may encrypt. Two things must be true for that task to
// reach a third-party sub-agent: the SDK must strip the encryption flag from
// the collaboration tools, and it must rewrite the resulting agent_message into
// a plain user message. Both are gated on this switch, which is off by default,
// so a gateway that leaves it unset silently delivers an empty task.
func TestBuildCLIProxyConfigEnablesCodexMultiAgentV2(t *testing.T) {
	root := t.TempDir()
	gateway := DefaultGatewayConfig()
	gateway.Version = 1
	gateway.WopalAI.BaseURL = "https://wopal.example/v1"
	gateway.WopalAI.Models = []ExternalModelConfig{{Name: "m"}}

	cfg, runtimePath, err := BuildCLIProxyConfig(root, gateway, "wopal-secret", nil)
	if err != nil {
		t.Fatalf("BuildCLIProxyConfig: %v", err)
	}
	if !cfg.Client.Codex.OptimizeMultiAgentV2 {
		t.Fatal("client.codex.optimize-multi-agent-v2 is disabled; delegated sub-agent tasks would be dropped")
	}

	// The switch has to survive the round trip through the file the SDK reads,
	// not only the in-memory struct.
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode runtime config: %v", err)
	}
	client, _ := raw["client"].(map[string]any)
	codex, _ := client["codex"].(map[string]any)
	if codex["optimize-multi-agent-v2"] != true {
		t.Fatalf("runtime config client.codex = %v, want optimize-multi-agent-v2: true", client)
	}
}
