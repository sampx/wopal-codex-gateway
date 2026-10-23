package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
)

type modelsCommandFixture struct {
	config        *config.GatewayConfig
	available     []string
	refreshResult catalog.CatalogRefreshResult
	output        bytes.Buffer
	saved         []*config.GatewayConfig
	fetchedURL    string
	fetchedKey    string
	fetchCalls    int
	refreshCalls  int
}

func newModelsCommandFixture(config *config.GatewayConfig) *modelsCommandFixture {
	return &modelsCommandFixture{
		config: config,
		refreshResult: catalog.CatalogRefreshResult{
			Native:        20,
			NativeVisible: 12,
			NativeHidden:  8,
			WopalAI:       2,
		},
	}
}

func (fixture *modelsCommandFixture) runner() wopalModelsRunner {
	return wopalModelsRunner{
		loadConfig: func() (*config.GatewayConfig, error) {
			return fixture.config, nil
		},
		saveConfig: func(cfg *config.GatewayConfig) error {
			fixture.saved = append(fixture.saved, cfg)
			return nil
		},
		fetchModels: func(baseURL, apiKey string) ([]string, error) {
			fixture.fetchCalls++
			fixture.fetchedURL = baseURL
			fixture.fetchedKey = apiKey
			return fixture.available, nil
		},
		getAPIKey: func() (string, error) {
			return "test-key", nil
		},
		refreshCatalog: func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
			fixture.refreshCalls++
			result := fixture.refreshResult
			return &result, nil
		},
		out: &fixture.output,
	}
}

func TestModelsCommandListPrintsOneModelPerLine(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	gatewayCfg.WopalAI.BaseURL = "https://wopal.example/v1"
	fixture := newModelsCommandFixture(gatewayCfg)
	fixture.available = []string{"glm-5.3", "kimi-k3"}

	if err := fixture.runner().run("list", nil); err != nil {
		t.Fatal(err)
	}

	if got, want := fixture.output.String(), "glm-5.3\nkimi-k3\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if fixture.fetchedURL != gatewayCfg.WopalAI.BaseURL {
		t.Fatalf("fetched base URL = %q, want %q", fixture.fetchedURL, gatewayCfg.WopalAI.BaseURL)
	}
	if fixture.fetchedKey != "test-key" {
		t.Fatalf("fetched API key = %q, want test-key", fixture.fetchedKey)
	}
}

func TestModelsCommandSyncImportsModelsAndPrintsCatalogLast(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	gatewayCfg.WopalAI.BaseURL = "https://wopal.example/v1"
	gatewayCfg.WopalAI.Models = []config.ExternalModelConfig{{
		Name:          "existing",
		ContextWindow: modelsIntPtr(524288),
		Vision:        modelsBoolPtr(true),
		Reasoning:     modelsBoolPtr(false),
	}}
	gatewayCfg.WopalAI.ExcludedModels = []string{"drop-me"}
	fixture := newModelsCommandFixture(gatewayCfg)
	fixture.available = []string{"existing", "new-model", "embed-demo", "drop-me", "new-model"}
	fixture.refreshResult.WopalAI = 2

	if err := fixture.runner().run("sync", nil); err != nil {
		t.Fatal(err)
	}

	wantOutput := "" +
		"Wopal AI models synced: 2 Codex-usable model(s).\n" +
		"Skipped non-chat model(s): embed-demo\n" +
		"Manually excluded model(s): drop-me\n" +
		"Model catalog: official-visible=12, official-internal=8, wopal-ai=2\n"
	if got := fixture.output.String(); got != wantOutput {
		t.Fatalf("output = %q, want %q", got, wantOutput)
	}
	if len(fixture.saved) != 1 {
		t.Fatalf("save calls = %d, want 1", len(fixture.saved))
	}
	if fixture.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", fixture.refreshCalls)
	}
	if got, want := namesOfModels(gatewayCfg.WopalAI.Models), []string{"existing", "new-model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("saved model names = %#v, want %#v", got, want)
	}
	if got := gatewayCfg.WopalAI.Models[0]; got.EffectiveContextWindow() != 524288 || !got.SupportsVision() || got.SupportsReasoning() {
		t.Fatalf("existing metadata was not preserved: %#v", got)
	}
}

func TestModelsCommandSyncRequiresBaseURL(t *testing.T) {
	fixture := newModelsCommandFixture(config.DefaultGatewayConfig())

	err := fixture.runner().run("sync", nil)
	if err == nil || err.Error() != "Wopal AI base URL is not configured" {
		t.Fatalf("error = %v, want Wopal AI base URL is not configured", err)
	}
	if fixture.fetchCalls != 0 || len(fixture.saved) != 0 || fixture.refreshCalls != 0 {
		t.Fatalf("side effects occurred: fetch=%d save=%d refresh=%d", fixture.fetchCalls, len(fixture.saved), fixture.refreshCalls)
	}
}

func TestModelsCommandAddUsesDefaultsAndUnexcludesNames(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	gatewayCfg.WopalAI.Models = []config.ExternalModelConfig{{
		Name:          "existing",
		ContextWindow: modelsIntPtr(100000),
		Vision:        modelsBoolPtr(false),
		Reasoning:     modelsBoolPtr(false),
	}}
	gatewayCfg.WopalAI.ExcludedModels = []string{"existing", "new-model", "keep-excluded"}
	fixture := newModelsCommandFixture(gatewayCfg)
	fixture.refreshResult.WopalAI = 2

	args := []string{"wopal-ai/new-model", "existing", "--context-window", "8192", "--vision", "--no-reasoning"}
	if err := fixture.runner().run("add", args); err != nil {
		t.Fatal(err)
	}

	wantOutput := "" +
		"Wopal AI model(s) added: new-model\n" +
		"Already configured: existing\n" +
		"Model catalog: official-visible=12, official-internal=8, wopal-ai=2\n"
	if got := fixture.output.String(); got != wantOutput {
		t.Fatalf("output = %q, want %q", got, wantOutput)
	}
	if got, want := namesOfModels(gatewayCfg.WopalAI.Models), []string{"existing", "new-model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model names = %#v, want %#v", got, want)
	}
	added := gatewayCfg.WopalAI.Models[1]
	if added.EffectiveContextWindow() != 8192 || !added.SupportsVision() || added.SupportsReasoning() {
		t.Fatalf("added model metadata = %#v", added)
	}
	if got, want := gatewayCfg.WopalAI.ExcludedModels, []string{"keep-excluded"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded models = %#v, want %#v", got, want)
	}
	if gatewayCfg.WopalAI.Models[0].EffectiveContextWindow() != 100000 || gatewayCfg.WopalAI.Models[0].SupportsReasoning() {
		t.Fatalf("existing model was changed: %#v", gatewayCfg.WopalAI.Models[0])
	}
}

func TestModelsCommandSetErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing names",
			args:    nil,
			wantErr: "Usage: wopal-codex-gateway models set <model...> [--context-window N] [--vision|--no-vision] [--reasoning|--no-reasoning]",
		},
		{
			name:    "conflicting vision flags",
			args:    []string{"alpha", "--vision", "--no-vision"},
			wantErr: "Choose only one of --vision or --no-vision",
		},
		{
			name:    "conflicting reasoning flags",
			args:    []string{"alpha", "--reasoning", "--no-reasoning"},
			wantErr: "Choose only one of --reasoning or --no-reasoning",
		},
		{
			name:    "no metadata",
			args:    []string{"alpha"},
			wantErr: "models set requires at least one metadata option",
		},
		{
			name:    "invalid context window",
			args:    []string{"alpha", "--context-window", "0"},
			wantErr: "--context-window must be a positive integer",
		},
		{
			name:    "missing model",
			args:    []string{"missing", "--vision"},
			wantErr: "Model(s) not configured: missing. Run models sync or models add first.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gatewayCfg := config.DefaultGatewayConfig()
			fixture := newModelsCommandFixture(gatewayCfg)

			err := fixture.runner().run("set", test.args)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if len(fixture.saved) != 0 || fixture.refreshCalls != 0 {
				t.Fatalf("side effects occurred: save=%d refresh=%d", len(fixture.saved), fixture.refreshCalls)
			}
			if fixture.output.Len() != 0 {
				t.Fatalf("unexpected output: %q", fixture.output.String())
			}
		})
	}
}

func TestModelsCommandSetUpdatesRequestedMetadata(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	gatewayCfg.WopalAI.Models = []config.ExternalModelConfig{
		{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)},
		{Name: "beta", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)},
	}
	fixture := newModelsCommandFixture(gatewayCfg)

	args := []string{"wopal-ai/alpha", "--context-window", "524288", "--no-vision", "--reasoning"}
	if err := fixture.runner().run("set", args); err != nil {
		t.Fatal(err)
	}

	wantOutput := "" +
		"Wopal AI model metadata updated: alpha\n" +
		"  wopal-ai/alpha: context=524288, vision=false, reasoning=true\n" +
		"Model catalog: official-visible=12, official-internal=8, wopal-ai=2\n"
	if got := fixture.output.String(); got != wantOutput {
		t.Fatalf("output = %q, want %q", got, wantOutput)
	}
	alpha := gatewayCfg.WopalAI.Models[0]
	if alpha.EffectiveContextWindow() != 524288 || alpha.SupportsVision() || !alpha.SupportsReasoning() {
		t.Fatalf("alpha metadata = %#v", alpha)
	}
	beta := gatewayCfg.WopalAI.Models[1]
	if beta.EffectiveContextWindow() != 131072 || !beta.SupportsVision() || !beta.SupportsReasoning() {
		t.Fatalf("beta metadata was changed: %#v", beta)
	}
}

func TestModelsCommandRemoveExcludesRemovedAndMissingNames(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	gatewayCfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "alpha"}, {Name: "beta"}}
	gatewayCfg.WopalAI.ExcludedModels = []string{"existing-exclusion"}
	fixture := newModelsCommandFixture(gatewayCfg)

	args := []string{"wopal-ai/alpha", "missing", "--ignored"}
	if err := fixture.runner().run("remove", args); err != nil {
		t.Fatal(err)
	}

	wantOutput := "" +
		"Wopal AI model(s) removed: alpha\n" +
		"Not configured, but excluded from future sync: missing\n" +
		"Removed model(s) are excluded from future sync until explicitly added again.\n" +
		"Model catalog: official-visible=12, official-internal=8, wopal-ai=2\n"
	if got := fixture.output.String(); got != wantOutput {
		t.Fatalf("output = %q, want %q", got, wantOutput)
	}
	if got, want := namesOfModels(gatewayCfg.WopalAI.Models), []string{"beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model names = %#v, want %#v", got, want)
	}
	if got, want := gatewayCfg.WopalAI.ExcludedModels, []string{"existing-exclusion", "alpha", "missing"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded models = %#v, want %#v", got, want)
	}
}

func TestModelsCommandRemoveMissingOnlyPrintsNoRemovalNotice(t *testing.T) {
	gatewayCfg := config.DefaultGatewayConfig()
	fixture := newModelsCommandFixture(gatewayCfg)

	if err := fixture.runner().run("remove", []string{"missing"}); err != nil {
		t.Fatal(err)
	}

	wantOutput := "" +
		"Not configured, but excluded from future sync: missing\n" +
		"Model catalog: official-visible=12, official-internal=8, wopal-ai=2\n"
	if got := fixture.output.String(); got != wantOutput {
		t.Fatalf("output = %q, want %q", got, wantOutput)
	}
	if strings.Contains(fixture.output.String(), "Removed model(s) are excluded") {
		t.Fatal("missing-only remove printed the removal notice")
	}
}

func TestModelsCommandRemoveRequiresNames(t *testing.T) {
	fixture := newModelsCommandFixture(config.DefaultGatewayConfig())
	err := fixture.runner().run("remove", []string{"--unknown"})
	if err == nil || err.Error() != "Usage: wopal-codex-gateway models remove <model...>" {
		t.Fatalf("error = %v, want remove usage", err)
	}
}
