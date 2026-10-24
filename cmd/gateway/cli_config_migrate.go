package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"wopal-codex-gateway/internal/config"
)

// Migration always creates a separate candidate; it never rewrites the source.
func runConfigMigrate(args []string) error {
	from, fromOK := option(args, "--from")
	to, toOK := option(args, "--to")
	if !fromOK || !toOK || from == "" || to == "" {
		return fmt.Errorf("usage: config migrate --from <existing.json> --to <new.json>")
	}
	fromAbs, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	toAbs, err := filepath.Abs(to)
	if err != nil {
		return err
	}
	if fromAbs == toAbs {
		return fmt.Errorf("migration requires a separate output file")
	}
	if _, err := os.Lstat(toAbs); !os.IsNotExist(err) {
		return fmt.Errorf("migration output already exists or is inaccessible: %s", toAbs)
	}
	data, err := os.ReadFile(fromAbs)
	if err != nil {
		return fmt.Errorf("read migration source: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("migration source must be a JSON object")
	}
	if version, present := fields["version"]; present && string(version) != "1" {
		return fmt.Errorf("migration source has an unsupported version; expected absent or 1")
	}
	fields["version"] = json.RawMessage("1")
	delete(fields, "ollama")
	payload, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	var validated config.GatewayConfig
	if err := json.Unmarshal(payload, &validated); err != nil {
		return fmt.Errorf("invalid migration configuration: %w", err)
	}
	if err := validated.WopalAI.ValidateToolMode(); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(toAbs), ".gateway-migrate-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(payload, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link atomically publishes the private file and refuses an existing target.
	if err := os.Link(file.Name(), toAbs); err != nil {
		return fmt.Errorf("publish migration candidate: %w", err)
	}
	fmt.Printf("Migration candidate written to %s; source unchanged.\n", toAbs)
	return nil
}
