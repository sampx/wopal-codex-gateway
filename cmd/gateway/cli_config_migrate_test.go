package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigMigrationPreservesExplicitModelsAndSource(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old.json"), filepath.Join(root, "new.json")
	text := `{"wopalAi":{"baseUrl":"https://example.test/v1","models":[{"name":"custom","contextWindow":123456,"vision":false,"reasoning":false}],"excludedModels":["excluded"]},"ollama":{"enabled":false},"extra":{"keep":true}}`
	if err := os.WriteFile(source, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runConfigMigrate([]string{"--from", source, "--to", target}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(source)
	if string(before) != text {
		t.Fatal("source changed")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var old, new map[string]any
	_ = json.Unmarshal(before, &old)
	_ = json.Unmarshal(data, &new)
	if new["version"] != float64(1) || new["ollama"] != nil || !reflect.DeepEqual(old["wopalAi"], new["wopalAi"]) || !reflect.DeepEqual(old["extra"], new["extra"]) {
		t.Fatalf("migration lost configuration: %s", data)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("migration output not private")
	}
	if err := runConfigMigrate([]string{"--from", source, "--to", source}); err == nil {
		t.Fatal("in-place migration accepted")
	}
	if err := runConfigMigrate([]string{"--from", source, "--to", target}); err == nil {
		t.Fatal("existing output overwritten")
	}
}

func TestConfigMigrationRejectsInvalidInputBeforeWriting(t *testing.T) {
	for _, text := range []string{`null`, `[]`, `broken`, `{"version":2}`, `{"version":0}`, `{"wopalAi":{"models":"bad"}}`, `{"wopalAi":{"toolMode":"typo"}}`} {
		root := t.TempDir()
		source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
		if err := os.WriteFile(source, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runConfigMigrate([]string{"--from", source, "--to", target}); err == nil {
			t.Fatalf("accepted %s", text)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("invalid input produced output: %s", text)
		}
	}
}
