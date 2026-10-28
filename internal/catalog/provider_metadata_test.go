package catalog

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
)

func useCatalogTempRoot(t *testing.T) {
	t.Helper()
	t.Cleanup(config.SetProjectRootForTest(t.TempDir()))
}

func TestResolveProviderModelProtocolPrecedence(t *testing.T) {
	metadata := &ProviderModelMetadataSnapshot{
		Provider: "opencode-go",
		Models: []ProviderModelMetadata{{
			Name:           "model-a",
			Protocol:       config.ProtocolOpenAIResponses,
			ProtocolSource: "metadata",
		}},
	}
	tests := []struct {
		name       string
		model      config.ExternalModelConfig
		provider   config.ProviderConfig
		snapshot   *ProviderModelMetadataSnapshot
		want       string
		wantSource string
	}{
		{"explicit", config.ExternalModelConfig{Name: "model-a", Protocol: config.ProtocolGemini}, config.ProviderConfig{Protocol: config.ProtocolOpenAIChat}, metadata, config.ProtocolGemini, "explicit"},
		{"metadata", config.ExternalModelConfig{Name: "model-a"}, config.ProviderConfig{Protocol: config.ProtocolOpenAIChat}, metadata, config.ProtocolOpenAIResponses, "metadata"},
		{"provider default", config.ExternalModelConfig{Name: "model-a"}, config.ProviderConfig{Protocol: config.ProtocolAnthropicMessages}, nil, config.ProtocolAnthropicMessages, "provider-default"},
		{"needs protocol", config.ExternalModelConfig{Name: "model-a"}, config.ProviderConfig{}, nil, "", "needs_protocol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, sources := ResolveProviderModel(tc.model, tc.provider, tc.snapshot)
			if got.Protocol != tc.want || sources.Protocol != tc.wantSource {
				t.Fatalf("protocol=%q source=%q, want %q/%q", got.Protocol, sources.Protocol, tc.want, tc.wantSource)
			}
		})
	}
}

func TestResolveProviderModelCapabilityPrecedence(t *testing.T) {
	snapshot := &ProviderModelMetadataSnapshot{Provider: "p", Models: []ProviderModelMetadata{{
		Name: "m", ContextWindow: providerIntPtr(131072), Vision: providerBoolPtr(false), Reasoning: providerBoolPtr(false),
	}}}
	model := config.ExternalModelConfig{Name: "m", Vision: providerBoolPtr(true)}
	got, sources := ResolveProviderModel(model, config.ProviderConfig{}, snapshot)
	if got.EffectiveContextWindow() != 131072 || !got.SupportsVision() || got.SupportsReasoning() {
		t.Fatalf("resolved=%+v", got)
	}
	if sources.ContextWindow != "metadata" || sources.Vision != "explicit" || sources.Reasoning != "metadata" {
		t.Fatalf("sources=%+v", sources)
	}

	defaults, defaultSources := ResolveProviderModel(config.ExternalModelConfig{Name: "unknown"}, config.ProviderConfig{}, snapshot)
	if defaults.EffectiveContextWindow() != config.DefaultContextWindow || !defaults.SupportsVision() || !defaults.SupportsReasoning() {
		t.Fatalf("defaults=%+v", defaults)
	}
	if defaultSources.ContextWindow != "default" || defaultSources.Vision != "default" || defaultSources.Reasoning != "default" {
		t.Fatalf("default sources=%+v", defaultSources)
	}
}

func TestModelsDevProtocolResolutionShapeThenModelNPMThenProviderNPM(t *testing.T) {
	source := &ModelsDevProvider{
		Provider: "p",
		NPM:      "@ai-sdk/anthropic",
		Models: map[string]ModelsDevModel{
			"shape":        {Name: "shape", Shape: "responses", NPM: "@ai-sdk/openai-compatible"},
			"model-npm":    {Name: "model-npm", NPM: "@ai-sdk/google"},
			"provider-npm": {Name: "provider-npm"},
			"unknown":      {Name: "unknown", NPM: "@ai-sdk/mistral"},
		},
	}
	snapshot := JoinProviderMetadata(source, []string{"shape", "model-npm", "provider-npm", "unknown", "missing"})
	want := map[string]string{
		"shape":        config.ProtocolOpenAIResponses,
		"model-npm":    config.ProtocolGemini,
		"provider-npm": config.ProtocolAnthropicMessages,
		"unknown":      "",
		"missing":      "",
	}
	for _, model := range snapshot.Models {
		if model.Protocol != want[model.Name] {
			t.Errorf("%s protocol=%q want=%q", model.Name, model.Protocol, want[model.Name])
		}
	}
}

func TestFetchModelsDevProviderUsesETagAndParsesCapabilities(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write([]byte(`{"providers":{"opencode-go":{"id":"opencode-go","npm":"@ai-sdk/openai-compatible","models":{"gpt-x":{"id":"gpt-x","reasoning":true,"modalities":{"input":["text","image"],"output":["text"]},"limit":{"context":262144,"output":8192},"provider":{"shape":"responses"}}}}},"models":{}}`))
	}))
	defer server.Close()

	provider, notModified, err := FetchModelsDevProvider(server.URL, "opencode-go", `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if notModified || gotIfNoneMatch != `"v1"` || provider.ETag != `"v2"` {
		t.Fatalf("notModified=%v if-none-match=%q etag=%q", notModified, gotIfNoneMatch, provider.ETag)
	}
	model := provider.Models["gpt-x"]
	if model.ContextWindow == nil || *model.ContextWindow != 262144 || model.Vision == nil || !*model.Vision || model.Reasoning == nil || !*model.Reasoning {
		t.Fatalf("model=%+v", model)
	}
}

func TestFetchModelsDevProviderReturnsNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	provider, notModified, err := FetchModelsDevProvider(server.URL, "p", `"v1"`)
	if err != nil || !notModified || provider != nil {
		t.Fatalf("provider=%v notModified=%v err=%v", provider, notModified, err)
	}
}

func TestProviderMetadataStoreKeepsProvidersSeparate(t *testing.T) {
	useCatalogTempRoot(t)
	now := time.Now().UTC()
	a := &ProviderModelMetadataSnapshot{Provider: "a", ETag: "a1", FetchedAt: now, Models: []ProviderModelMetadata{{Name: "ma", Protocol: config.ProtocolOpenAIChat}}}
	b := &ProviderModelMetadataSnapshot{Provider: "b", ETag: "b1", FetchedAt: now, Models: []ProviderModelMetadata{{Name: "mb", Protocol: config.ProtocolGemini}}}
	if err := SaveProviderModelMetadata(a); err != nil {
		t.Fatal(err)
	}
	if err := SaveProviderModelMetadata(b); err != nil {
		t.Fatal(err)
	}
	gotA, err := LoadProviderModelMetadata("a")
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := LoadProviderModelMetadata("b")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotA.Models, a.Models) || !reflect.DeepEqual(gotB.Models, b.Models) {
		t.Fatalf("a=%+v b=%+v", gotA, gotB)
	}
}

func providerIntPtr(v int) *int    { return &v }
func providerBoolPtr(v bool) *bool { return &v }
