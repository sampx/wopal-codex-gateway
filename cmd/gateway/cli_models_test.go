package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

type modelsCommandFixture struct {
	config        *GatewayConfig
	available     []string
	refreshResult CatalogRefreshResult
	output        bytes.Buffer
	saved         []*GatewayConfig
	fetchedURL    string
	fetchedKey    string
	fetchCalls    int
	refreshCalls  int
}

func newModelsCommandFixture(config *GatewayConfig) *modelsCommandFixture {
	return &modelsCommandFixture{
		config: config,
		refreshResult: CatalogRefreshResult{
			Native:        20,
			NativeVisible: 12,
			NativeHidden:  8,
			WopalAI:       2,
		},
	}
}

func (fixture *modelsCommandFixture) runner() wopalModelsRunner {
	return wopalModelsRunner{
		loadConfig: func() (*GatewayConfig, error) {
			return fixture.config, nil
		},
		saveConfig: func(cfg *GatewayConfig) error {
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
		refreshCatalog: func(*GatewayConfig) (*CatalogRefreshResult, error) {
			fixture.refreshCalls++
			result := fixture.refreshResult
			return &result, nil
		},
		out: &fixture.output,
	}
}

func TestModelsCommandListPrintsOneModelPerLine(t *testing.T) {
	config := defaultGatewayConfig()
	config.WopalAI.BaseURL = "https://wopal.example/v1"
	fixture := newModelsCommandFixture(config)
	fixture.available = []string{"glm-5.3", "kimi-k3"}

	if err := fixture.runner().run("list", nil); err != nil {
		t.Fatal(err)
	}

	if got, want := fixture.output.String(), "glm-5.3\nkimi-k3\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if fixture.fetchedURL != config.WopalAI.BaseURL {
		t.Fatalf("fetched base URL = %q, want %q", fixture.fetchedURL, config.WopalAI.BaseURL)
	}
	if fixture.fetchedKey != "test-key" {
		t.Fatalf("fetched API key = %q, want test-key", fixture.fetchedKey)
	}
}

func TestModelsCommandSyncImportsModelsAndPrintsCatalogLast(t *testing.T) {
	config := defaultGatewayConfig()
	config.WopalAI.BaseURL = "https://wopal.example/v1"
	config.WopalAI.Models = []ExternalModelConfig{{
		Name:          "existing",
		ContextWindow: modelsIntPtr(524288),
		Vision:        modelsBoolPtr(true),
		Reasoning:     modelsBoolPtr(false),
	}}
	config.WopalAI.ExcludedModels = []string{"drop-me"}
	fixture := newModelsCommandFixture(config)
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
	if got, want := namesOfModels(config.WopalAI.Models), []string{"existing", "new-model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("saved model names = %#v, want %#v", got, want)
	}
	if got := config.WopalAI.Models[0]; got.contextWindow() != 524288 || !got.supportsVision() || got.supportsReasoning() {
		t.Fatalf("existing metadata was not preserved: %#v", got)
	}
}

func TestModelsCommandSyncRequiresBaseURL(t *testing.T) {
	fixture := newModelsCommandFixture(defaultGatewayConfig())

	err := fixture.runner().run("sync", nil)
	if err == nil || err.Error() != "Wopal AI base URL is not configured" {
		t.Fatalf("error = %v, want Wopal AI base URL is not configured", err)
	}
	if fixture.fetchCalls != 0 || len(fixture.saved) != 0 || fixture.refreshCalls != 0 {
		t.Fatalf("side effects occurred: fetch=%d save=%d refresh=%d", fixture.fetchCalls, len(fixture.saved), fixture.refreshCalls)
	}
}

func TestModelsCommandAddUsesDefaultsAndUnexcludesNames(t *testing.T) {
	config := defaultGatewayConfig()
	config.WopalAI.Models = []ExternalModelConfig{{
		Name:          "existing",
		ContextWindow: modelsIntPtr(100000),
		Vision:        modelsBoolPtr(false),
		Reasoning:     modelsBoolPtr(false),
	}}
	config.WopalAI.ExcludedModels = []string{"existing", "new-model", "keep-excluded"}
	fixture := newModelsCommandFixture(config)
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
	if got, want := namesOfModels(config.WopalAI.Models), []string{"existing", "new-model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model names = %#v, want %#v", got, want)
	}
	added := config.WopalAI.Models[1]
	if added.contextWindow() != 8192 || !added.supportsVision() || added.supportsReasoning() {
		t.Fatalf("added model metadata = %#v", added)
	}
	if got, want := config.WopalAI.ExcludedModels, []string{"keep-excluded"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded models = %#v, want %#v", got, want)
	}
	if config.WopalAI.Models[0].contextWindow() != 100000 || config.WopalAI.Models[0].supportsReasoning() {
		t.Fatalf("existing model was changed: %#v", config.WopalAI.Models[0])
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
			config := defaultGatewayConfig()
			fixture := newModelsCommandFixture(config)

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
	config := defaultGatewayConfig()
	config.WopalAI.Models = []ExternalModelConfig{
		{Name: "alpha", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)},
		{Name: "beta", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)},
	}
	fixture := newModelsCommandFixture(config)

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
	alpha := config.WopalAI.Models[0]
	if alpha.contextWindow() != 524288 || alpha.supportsVision() || !alpha.supportsReasoning() {
		t.Fatalf("alpha metadata = %#v", alpha)
	}
	beta := config.WopalAI.Models[1]
	if beta.contextWindow() != 131072 || !beta.supportsVision() || !beta.supportsReasoning() {
		t.Fatalf("beta metadata was changed: %#v", beta)
	}
}

func TestModelsCommandRemoveExcludesRemovedAndMissingNames(t *testing.T) {
	config := defaultGatewayConfig()
	config.WopalAI.Models = []ExternalModelConfig{{Name: "alpha"}, {Name: "beta"}}
	config.WopalAI.ExcludedModels = []string{"existing-exclusion"}
	fixture := newModelsCommandFixture(config)

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
	if got, want := namesOfModels(config.WopalAI.Models), []string{"beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model names = %#v, want %#v", got, want)
	}
	if got, want := config.WopalAI.ExcludedModels, []string{"existing-exclusion", "alpha", "missing"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded models = %#v, want %#v", got, want)
	}
}

func TestModelsCommandRemoveMissingOnlyPrintsNoRemovalNotice(t *testing.T) {
	config := defaultGatewayConfig()
	fixture := newModelsCommandFixture(config)

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
	fixture := newModelsCommandFixture(defaultGatewayConfig())
	err := fixture.runner().run("remove", []string{"--unknown"})
	if err == nil || err.Error() != "Usage: wopal-codex-gateway models remove <model...>" {
		t.Fatalf("error = %v, want remove usage", err)
	}
}
