package config

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func decodeJSONWithNumber(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON %s: %v", raw, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("decode JSON %s: trailing data: %v", raw, err)
	}
	return value
}

func decodeObject(t *testing.T, raw json.RawMessage, label string) map[string]json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("%s is missing", label)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	return fields
}

func assertJSONEqual(t *testing.T, label string, want, got json.RawMessage) {
	t.Helper()
	if len(want) == 0 || len(got) == 0 {
		t.Fatalf("%s: want=%s got=%s", label, want, got)
	}
	wantValue := decodeJSONWithNumber(t, want)
	gotValue := decodeJSONWithNumber(t, got)
	if !reflect.DeepEqual(wantValue, gotValue) {
		t.Fatalf("%s changed: want=%s got=%s", label, want, got)
	}
}

func TestGatewayConfigPreservesExtensions(t *testing.T) {
	useTempGatewayRoot(t)
	body := `{"version":1,"future":{"number":9007199254740993},"listen":{"future":null},"wopalAi":{"future":[false,{}],"models":[{"name":"m","vision":false,"future":{"x":1}}]},"notifications":{"enabled":false,"future":true},"logs":{"future":"keep"},"quota":{"future":[]}}`
	cfg, err := LoadGatewayConfig(writeGatewayConfigFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	cfg.WopalAI.BaseURL = "https://new.test/v1"
	if err := SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	var before, after map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("saved config is not valid JSON: %s: %v", data, err)
	}

	// Top-level extension: compare with UseNumber so the exact integer survives.
	assertJSONEqual(t, "top-level future", before["future"], after["future"])
	topLevel := decodeJSONWithNumber(t, after["future"]).(map[string]any)
	number, ok := topLevel["number"].(json.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Fatalf("extension number lost precision: %s", after["future"])
	}

	// Each section gets a fresh map so the assertions cannot contaminate the
	// source objects or prove preservation through a shared map.
	for _, section := range []string{"listen", "wopalAi", "notifications", "logs", "quota"} {
		oldFields := decodeObject(t, before[section], section+" before")
		newFields := decodeObject(t, after[section], section+" after")
		oldFuture, oldOK := oldFields["future"]
		newFuture, newOK := newFields["future"]
		if !oldOK || !newOK {
			t.Fatalf("%s extension lost: before=%s after=%s", section, before[section], after[section])
		}
		assertJSONEqual(t, section+" future", oldFuture, newFuture)
	}

	wopalAfter := decodeObject(t, after["wopalAi"], "wopalAi after")
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(wopalAfter["models"], &models); err != nil || len(models) != 1 {
		t.Fatalf("wopalAi.models = %s: %v", wopalAfter["models"], err)
	}
	assertJSONEqual(t, "model future", json.RawMessage(`{"x":1}`), models[0]["future"])

	reloaded, err := LoadGatewayConfig(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WopalAI.Models[0].Vision == nil || *reloaded.WopalAI.Models[0].Vision {
		t.Fatal("explicit false vision override changed")
	}
	if reloaded.WopalAI.Models[0].Reasoning != nil {
		t.Fatal("absent reasoning override became set")
	}
	if reloaded.Notifications.NotificationsEnabled() {
		t.Fatal("explicit false notifications.enabled changed")
	}
	modelJSON, err := json.Marshal(reloaded.WopalAI.Models[0])
	if err != nil {
		t.Fatal(err)
	}
	modelFields := decodeObject(t, modelJSON, "reloaded model")
	assertJSONEqual(t, "reloaded model future", json.RawMessage(`{"x":1}`), modelFields["future"])
}

func TestMissingRootNeverWritesToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Cleanup(SetProjectRootForTest(""))
	if _, err := ProjectRootDir(); err == nil {
		t.Fatal("test requires a cwd without a project marker")
	}
	if err := SaveGatewayConfig(DefaultGatewayConfig()); err == nil {
		t.Error("save accepted missing root")
	}
	valid := DefaultGatewayConfig()
	valid.WopalAI.BaseURL = "https://example.test/v1"
	if _, _, err := BuildCLIProxyConfig("", valid, "test-key", nil); err == nil {
		t.Error("SDK write accepted missing root")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("MustProjectRoot did not fail closed")
			}
		}()
		MustProjectRoot()
	}()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stray files without a root: %v", entries)
	}
}

func TestSaveConfigDoesNotFollowPredictableTempSymlink(t *testing.T) {
	useTempGatewayRoot(t)
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A predictable temp name is exactly what the old implementation used.
	if err := os.Symlink(sentinel, ConfigPath()+".tmp-"+strconv.Itoa(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	if err := SaveGatewayConfig(DefaultGatewayConfig()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "untouched" {
		t.Fatal("save followed a preexisting temporary symlink")
	}
	info, err := os.Lstat(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("config is not a private regular file: %v", info.Mode())
	}
}

func TestSDKRuntimeIsAtomicallyReplacedAndPrivate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(StateDirFor(root), 0o700); err != nil {
		t.Fatal(err)
	}
	path := CLIProxyRuntimeConfigPathFor(root)
	old := []byte(`{"old":true}`)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "previous")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	gateway := DefaultGatewayConfig()
	gateway.WopalAI.BaseURL = "https://example.test/v1"
	cfg, got, err := BuildCLIProxyConfig(root, gateway, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != path || cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey != "test-key" {
		t.Fatal("SDK config assembly changed")
	}
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(old) {
		t.Error("SDK config truncated the previous inode instead of replacing it")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatal("invalid runtime JSON")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime mode = %v", info.Mode())
	}
	var disk map[string]any
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	entries := disk["openai-compatibility"].([]any)[0].(map[string]any)["api-key-entries"].([]any)
	if entries[0].(map[string]any)["api-key"] != "__KEYCHAIN__" {
		t.Fatal("SDK key persisted")
	}
	authInfo, err := os.Stat(CLIProxyAuthDirFor(root))
	if err != nil {
		t.Fatal(err)
	}
	if !authInfo.IsDir() || authInfo.Mode().Perm() != 0o700 {
		t.Fatalf("auth dir = %v", authInfo.Mode())
	}
}

// encoding/json matches known field names case-insensitively. An unknown-key
// scan that only deletes the exact spelling would re-emit "BaseURL" as an
// extension after the new "baseUrl", letting the stale spelling win on reload.
func TestGatewayConfigKnownFieldsAreCaseInsensitive(t *testing.T) {
	useTempGatewayRoot(t)
	body := `{"version":1,"LISTEN":{"PORT":12000},"wopalAi":{"BaseURL":"https://old.test/v1","Models":[{"NAME":"m","VISION":false}]},"notifications":{"ENABLED":false}}`
	cfg, err := LoadGatewayConfig(writeGatewayConfigFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WopalAI.BaseURL != "https://old.test/v1" || cfg.Listen.Port != 12000 {
		t.Fatalf("known fields in other cases were not parsed: %+v %+v", cfg.Listen, cfg.WopalAI)
	}
	cfg.WopalAI.BaseURL = "https://new.test/v1"
	cfg.Listen.Port = 13000
	if err := SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadGatewayConfig(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WopalAI.BaseURL != "https://new.test/v1" {
		t.Fatalf("stale case variant overwrote the edit: %q", reloaded.WopalAI.BaseURL)
	}
	if reloaded.Listen.Port != 13000 {
		t.Fatalf("stale case variant overwrote the edited port: %d", reloaded.Listen.Port)
	}
	if len(reloaded.WopalAI.Models) != 1 || reloaded.WopalAI.Models[0].Name != "m" {
		t.Fatalf("model changed across save: %+v", reloaded.WopalAI.Models)
	}
	if reloaded.Notifications.NotificationsEnabled() {
		t.Fatal("explicit false notifications in other case was lost")
	}

	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, present := saved["LISTEN"]; present {
		t.Fatalf("case-variant known section was emitted as an extension: %s", data)
	}
	wopal := decodeObject(t, saved["wopalAi"], "wopalAi")
	if _, present := wopal["BaseURL"]; present {
		t.Fatalf("case-variant known field was emitted as an extension: %s", data)
	}
}
