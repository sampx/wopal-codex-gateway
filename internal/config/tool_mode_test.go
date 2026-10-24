package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestGatewayToolModeValidationAndDefault(t *testing.T) {
	for _, mode := range []string{"", "direct", "code_mode", "code_mode_only"} {
		t.Run(mode, func(t *testing.T) {
			cfg, err := LoadGatewayConfig(writeGatewayConfigFile(t, fmt.Sprintf(`{"version":1,"wopalAi":{"toolMode":%q}}`, mode)))
			if err != nil {
				t.Fatal(err)
			}
			want := mode
			if want == "" {
				want = "code_mode_only"
			}
			if cfg.WopalAI.EffectiveToolMode() != want {
				t.Fatalf("effective mode = %q, want %q", cfg.WopalAI.EffectiveToolMode(), want)
			}
		})
	}
	for _, mode := range []string{"bogus", "CODE_MODE", " code_mode "} {
		_, err := LoadGatewayConfig(writeGatewayConfigFile(t, fmt.Sprintf(`{"version":1,"wopalAi":{"toolMode":%q}}`, mode)))
		if err == nil || !strings.Contains(err.Error(), "wopalAi.toolMode") {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
}
