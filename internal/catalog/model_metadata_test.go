package catalog

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wopal-codex-gateway/internal/config"
)

func metadataTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(config.SetProjectRootForTest(root))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	return filepath.Join(root, "runtime", "model-metadata.json")
}

func metadataTestSnapshot() *ModelMetadataSnapshot {
	return &ModelMetadataSnapshot{BaseURL: "https://provider.test/v1", FetchedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), Models: []config.ExternalModelConfig{
		{Name: "model", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)},
		{Name: "model-free", Vision: modelsBoolPtr(true)},
	}}
}

func TestResolveModelCapabilityPriority(t *testing.T) {
	snapshot := metadataTestSnapshot()
	for _, tc := range []struct {
		name     string
		model    config.ExternalModelConfig
		snapshot *ModelMetadataSnapshot
		want     config.ExternalModelConfig
		sources  ModelCapabilitySources
	}{
		{"default", config.ExternalModelConfig{Name: "unknown"}, snapshot, config.ExternalModelConfig{Name: "unknown", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"default", "default", "default"}},
		{"nil snapshot", config.ExternalModelConfig{Name: "model"}, nil, config.ExternalModelConfig{Name: "model", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"default", "default", "default"}},
		{"provider false", config.ExternalModelConfig{Name: "model"}, snapshot, snapshot.Models[0], ModelCapabilitySources{"provider", "provider", "provider"}},
		{"user wins", config.ExternalModelConfig{Name: "model", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, snapshot, config.ExternalModelConfig{Name: "model", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"user", "user", "user"}},
		{"per field", config.ExternalModelConfig{Name: "model-free", Reasoning: modelsBoolPtr(false)}, snapshot, config.ExternalModelConfig{Name: "model-free", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(false)}, ModelCapabilitySources{"default", "provider", "user"}},
		{"user false", config.ExternalModelConfig{Name: "model-free", Vision: modelsBoolPtr(false)}, snapshot, config.ExternalModelConfig{Name: "model-free", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"default", "user", "default"}},
		{"no suffix guessing", config.ExternalModelConfig{Name: "model-flash"}, snapshot, config.ExternalModelConfig{Name: "model-flash", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"default", "default", "default"}},
		{"no prefix guessing", config.ExternalModelConfig{Name: "wopal-ai/model"}, snapshot, config.ExternalModelConfig{Name: "wopal-ai/model", ContextWindow: modelsIntPtr(1000000), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)}, ModelCapabilitySources{"default", "default", "default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeModel, _ := json.Marshal(tc.model)
			beforeSnapshot, _ := json.Marshal(tc.snapshot)
			got, sources := ResolveModel(tc.model, tc.snapshot)
			if !reflect.DeepEqual(got, tc.want) || sources != tc.sources {
				t.Fatalf("resolved=%+v sources=%+v; want=%+v sources=%+v", got, sources, tc.want, tc.sources)
			}
			*got.ContextWindow = 1
			*got.Vision = !*got.Vision
			*got.Reasoning = !*got.Reasoning
			afterModel, _ := json.Marshal(tc.model)
			afterSnapshot, _ := json.Marshal(tc.snapshot)
			if string(beforeModel) != string(afterModel) || string(beforeSnapshot) != string(afterSnapshot) {
				t.Fatal("resolution aliases or mutates inputs")
			}
		})
	}
}

func TestFetchWopalModelMetadataCapabilities(t *testing.T) {
	body := `{"data":[
 {"id":" model ","context_window":131072,"vision":false,"reasoning":false,"api_key":"must-not-persist"},
 {"id":"camel","contextWindow":262144,"input_modalities":["text","image"],"reasoning":true},
 {"id":"text","input_modalities":["text"]},
 {"id":"explicit","vision":false,"input_modalities":["text","image"]},
 {"id":"image","input_modalities":["image"]},
 {"id":"bare-flash-free","owned_by":"vision-reasoning-million"},
 {"id":7},null,{},"garbage",{"id":""}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request = %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	before := time.Now()
	snapshot, err := FetchWopalModelMetadata(server.URL+"/v1///", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseURL != server.URL+"/v1" || snapshot.FetchedAt.Before(before) || snapshot.FetchedAt.After(time.Now()) {
		t.Fatalf("snapshot identity/time = %+v", snapshot)
	}
	want := []config.ExternalModelConfig{
		{Name: "model", ContextWindow: modelsIntPtr(131072), Vision: modelsBoolPtr(false), Reasoning: modelsBoolPtr(false)},
		{Name: "camel", ContextWindow: modelsIntPtr(262144), Vision: modelsBoolPtr(true), Reasoning: modelsBoolPtr(true)},
		{Name: "text", Vision: modelsBoolPtr(false)}, {Name: "explicit", Vision: modelsBoolPtr(false)},
		{Name: "image", Vision: modelsBoolPtr(true)}, {Name: "bare-flash-free"},
	}
	if !reflect.DeepEqual(snapshot.Models, want) {
		t.Fatalf("models = %+v; want %+v", snapshot.Models, want)
	}
	ids, err := FetchWopalModels(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, namesOfModels(want)) {
		t.Fatalf("legacy IDs = %v", ids)
	}
	raw, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw), "test-key") || strings.Contains(string(raw), "must-not-persist") || strings.Contains(string(raw), "owned_by") {
		t.Fatalf("raw/provider secrets retained: %s", raw)
	}
}

func TestFetchMetadataRejectsInvalidCapabilitiesWithoutInventingValues(t *testing.T) {
	for _, fields := range []string{
		`"context_window":0,"vision":"false","reasoning":1`,
		`"context_window":-1,"vision":null,"reasoning":null`,
		`"context_window":1.5,"input_modalities":"image"`,
		`"contextWindow":"1000000","input_modalities":["text",7]`,
		`"context_window":999999999999999999999999,"input_modalities":[]`,
		`"context_window":null,"input_modalities":null`,
		`"context_window":true,"input_modalities":["audio"]`,
		`"context_window":{},"input_modalities":["text",null]`,
		`"input_modalities":["text","unknown"]`,
	} {
		t.Run(fields, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"data":[{"id":"model",`+fields+`}]}`)
			}))
			defer server.Close()
			snapshot, err := FetchWopalModelMetadata(server.URL, "test-key")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(snapshot.Models, []config.ExternalModelConfig{{Name: "model"}}) {
				t.Fatalf("invalid values became overrides: %+v", snapshot.Models)
			}
		})
	}
}

func TestMetadataPersistenceAndProviderIsolation(t *testing.T) {
	path := metadataTestRoot(t)
	if got, err := LoadModelMetadata("https://provider.test/v1"); err != nil || got != nil {
		t.Fatalf("absent = %+v %v", got, err)
	}
	snapshot := metadataTestSnapshot()
	if err := SaveModelMetadata(snapshot); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, err := LoadModelMetadata(snapshot.BaseURL + "///")
	if err != nil || !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("roundtrip = %+v %v", got, err)
	}
	for _, provider := range []string{"https://other.test/v1", "https://provider.test/v2", ""} {
		got, err := LoadModelMetadata(provider)
		if err != nil || got != nil {
			t.Fatalf("wrong provider %s = %+v %v", provider, got, err)
		}
	}
	snapshot.Models = []config.ExternalModelConfig{{Name: "new"}}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelMetadata(snapshot); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("replace did not restore private mode")
	}
	got, err = LoadModelMetadata(snapshot.BaseURL)
	if err != nil || !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("replace = %+v %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("left temp files: %v", entries)
	}
}

func TestMetadataMalformedCacheIsExplicit(t *testing.T) {
	path := metadataTestRoot(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`broken`, `null`, `{}`, `{"baseUrl":"https://provider.test/v1","fetchedAt":"bad","models":[]}`, `{"baseUrl":"https://provider.test/v1","fetchedAt":"2026-10-24T00:00:00Z","models":[{"name":"m","contextWindow":-1}]}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadModelMetadata("https://provider.test/v1"); err == nil || !strings.Contains(err.Error(), "model metadata") {
			t.Fatalf("malformed %s: %v", body, err)
		}
	}
}

func TestMetadataFetchFailurePreservesLastSuccessfulState(t *testing.T) {
	path := metadataTestRoot(t)
	snapshot := metadataTestSnapshot()
	if err := SaveModelMetadata(snapshot); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "model", Vision: modelsBoolPtr(false)}}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	beforeCache, _ := os.ReadFile(path)
	beforeConfig, _ := os.ReadFile(config.ConfigPath())
	for _, tc := range []struct {
		status int
		body   string
	}{
		{503, `{"error":{"message":"unavailable"}}`}, {200, `not json`}, {200, `{"data":null}`}, {200, `{"data":{}}`}, {200, `[]`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		got, err := FetchWopalModelMetadata(server.URL, "test-key")
		server.Close()
		if err == nil || got != nil {
			t.Fatalf("failed fetch = %+v %v", got, err)
		}
		afterCache, _ := os.ReadFile(path)
		afterConfig, _ := os.ReadFile(config.ConfigPath())
		if string(beforeCache) != string(afterCache) || string(beforeConfig) != string(afterConfig) {
			t.Fatal("failure changed stored state")
		}
	}
	if err := SaveModelMetadata(nil); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(beforeCache) {
		t.Fatal("failed save overwrote cache")
	}
}

func TestFetchMetadataBoundsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"padding":"`+strings.Repeat("x", 8*1024*1024)+`"}`)
	}))
	defer server.Close()
	if got, err := FetchWopalModelMetadata(server.URL, "test-key"); err == nil || got != nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize = %+v %v", got, err)
	}
}

func TestRefreshCatalogResolvesProviderMetadata(t *testing.T) {
	path := metadataTestRoot(t)
	snapshot := metadataTestSnapshot()
	if err := SaveModelMetadata(snapshot); err != nil {
		t.Fatal(err)
	}
	catalogTestStubCommand(t, catalogCommandOutput{stdout: []byte(`{"models":[{"slug":"native"}]}`)}, nil)
	cfg := &config.GatewayConfig{WopalAI: config.WopalAIConfig{BaseURL: snapshot.BaseURL, Models: []config.ExternalModelConfig{{Name: "model"}, {Name: "model-free", Vision: modelsBoolPtr(false)}, {Name: "unknown"}}}}
	check := func(context float64, vision []any, reasoning string) {
		t.Helper()
		if _, err := RefreshCatalog(cfg); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(config.CodexGatewayCatalogPath())
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		first := payload.Models[0]
		if first["context_window"] != context || !reflect.DeepEqual(first["input_modalities"], vision) || first["default_reasoning_level"] != reasoning {
			t.Fatalf("resolved entry = %+v", first)
		}
		if !reflect.DeepEqual(payload.Models[1]["input_modalities"], []any{"text"}) || payload.Models[2]["context_window"] != float64(1000000) {
			t.Fatalf("user/default entries = %+v", payload.Models[1:3])
		}
	}
	check(131072, []any{"text"}, "none")
	if cfg.WopalAI.Models[0].ContextWindow != nil || cfg.WopalAI.Models[0].Vision != nil {
		t.Fatal("refresh mutated user config")
	}
	cfg.WopalAI.BaseURL = "https://other.test/v1"
	check(1000000, []any{"text", "image"}, "high")
	before, err := os.ReadFile(config.CodexGatewayCatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshCatalog(cfg); err == nil {
		t.Fatal("refresh hid malformed cache")
	}
	after, _ := os.ReadFile(config.CodexGatewayCatalogPath())
	if string(before) != string(after) {
		t.Fatal("failed refresh replaced catalog")
	}
}

// Formatting can grow an accepted provider response beyond the persistence
// limit. Reject it before writing and preserve the last usable snapshot.
func TestMetadataSaveRejectsOversizePayloadAndPreservesCache(t *testing.T) {
	path := metadataTestRoot(t)
	snapshot := metadataTestSnapshot()
	if err := SaveModelMetadata(snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oversized := *snapshot
	oversized.Models = make([]config.ExternalModelConfig, 50000)
	for i := range oversized.Models {
		oversized.Models[i] = config.ExternalModelConfig{Name: strings.Repeat("m", 60)}
	}
	if err := SaveModelMetadata(&oversized); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize save = %v; want explicit limit error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("oversize save replaced previous snapshot")
	}
	loaded, err := LoadModelMetadata(snapshot.BaseURL)
	if err != nil || !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("previous snapshot cannot reload: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("oversize save left temporary files: %v", entries)
	}
}
