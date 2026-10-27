package codex

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"wopal-codex-gateway/internal/catalog"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
	"wopal-codex-gateway/internal/launchd"
)

func TestCodexFileAtomicWriteUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeCodexFileAtomic(path, "first\n"); err != nil {
		t.Fatalf("writeCodexFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first\n" {
		t.Fatalf("content = %q", data)
	}

	if err := writeCodexFileAtomic(path, "second\n"); err != nil {
		t.Fatalf("rewrite writeCodexFileAtomic: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten: %v", err)
	}
	if string(data) != "second\n" {
		t.Fatalf("rewritten content = %q", data)
	}
	matches, err := filepath.Glob(path + ".wopal-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

// installForTest runs the real install/restore code paths with launchd and the
// catalog baseline replaced by seams, so the config.toml takeover can be proven
// without touching the user's LaunchAgent.
func installForTest(t *testing.T, cfg *config.GatewayConfig) error {
	t.Helper()

	previousInstall := launchd.InstallLaunchAgentHook
	previousHealth := launchd.WaitForHealthHook
	previousBinary := launchd.RequireGatewayBinaryHook
	previousRefresh := catalog.RefreshCatalogHook
	previousHasKey := keychain.HasAPIKeyHook
	previousResolve := catalog.ResolveCodexHook
	previousExecute := catalog.ExecuteCodexHook
	t.Cleanup(func() {
		launchd.InstallLaunchAgentHook = previousInstall
		launchd.WaitForHealthHook = previousHealth
		launchd.RequireGatewayBinaryHook = previousBinary
		catalog.RefreshCatalogHook = previousRefresh
		keychain.HasAPIKeyHook = previousHasKey
		catalog.ResolveCodexHook = previousResolve
		catalog.ExecuteCodexHook = previousExecute
	})

	launchd.InstallLaunchAgentHook = func(string, int) error { return nil }
	launchd.WaitForHealthHook = func(string, int) error { return nil }
	launchd.RequireGatewayBinaryHook = func() error { return nil }
	keychain.HasAPIKeyHook = func() bool { return true }
	catalog.ResolveCodexHook = func() (string, error) { return "/usr/bin/codex", nil }
	catalog.ExecuteCodexHook = func(string) catalog.CatalogCommandOutput {
		return catalog.CatalogCommandOutput{}
	}
	catalog.RefreshCatalogHook = func(*config.GatewayConfig) (*catalog.CatalogRefreshResult, error) {
		return &catalog.CatalogRefreshResult{Native: 2, NativeVisible: 1, NativeHidden: 1}, nil
	}

	_, err := InstallIntoCodex(cfg, "wopal-ai/deepseek-v4.1-flash")
	return err
}

func restoreForTest(t *testing.T) error {
	t.Helper()
	previousUninstall := launchd.UninstallLaunchAgentHook
	t.Cleanup(func() { launchd.UninstallLaunchAgentHook = previousUninstall })
	launchd.UninstallLaunchAgentHook = func() error { return nil }
	return RestoreCodex()
}

func TestInstallLeavesModelAloneAndRestoreKeepsOfficialChoice(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	restoreRoot := config.SetProjectRootForTest(root)
	t.Cleanup(restoreRoot)
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.CodexConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	original := "model = \"gpt-6.1-sol\"\n\n[other]\nkeep = true\n"
	writeConfig := func(body string) {
		if err := os.WriteFile(config.CodexConfigPath(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readConfig := func() string {
		data, err := os.ReadFile(config.CodexConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	writeConfig(original)

	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	if err := installForTest(t, cfg); err != nil {
		t.Fatalf("install: %v", err)
	}

	afterInstall := readConfig()
	if !strings.Contains(afterInstall, `model = "gpt-6.1-sol"`) {
		t.Fatalf("install rewrote the current model:\n%s", afterInstall)
	}
	if !strings.Contains(afterInstall, "model_catalog_json") || !strings.Contains(afterInstall, "openai_base_url") {
		t.Fatalf("install did not take over the managed keys:\n%s", afterInstall)
	}
	if !strings.Contains(afterInstall, "[other]") || !strings.Contains(afterInstall, "keep = true") {
		t.Fatalf("install dropped an unrelated section:\n%s", afterInstall)
	}

	// The user later picks an official model; restore must keep that choice.
	writeConfig(strings.Replace(afterInstall, `model = "gpt-6.1-sol"`, `model = "gpt-6.1-luna"`, 1))
	if err := restoreForTest(t); err != nil {
		t.Fatalf("restore: %v", err)
	}
	afterRestore := readConfig()
	if !strings.Contains(afterRestore, `model = "gpt-6.1-luna"`) {
		t.Fatalf("restore overwrote the user's official model:\n%s", afterRestore)
	}
	if strings.Contains(afterRestore, "wopal-codex-gateway-models.json") || strings.Contains(afterRestore, "openai_base_url") {
		t.Fatalf("restore left a gateway reference behind:\n%s", afterRestore)
	}
}

func TestRestoreReturnsToPreInstallModelWhenStillOnWopal(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	restoreRoot := config.SetProjectRootForTest(root)
	t.Cleanup(restoreRoot)
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.CodexConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	original := "model = \"gpt-6.1-sol\"\n"
	if err := os.WriteFile(config.CodexConfigPath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultGatewayConfig()
	cfg.WopalAI.BaseURL = "https://example.test/v1"
	if err := installForTest(t, cfg); err != nil {
		t.Fatalf("install: %v", err)
	}

	installed, err := os.ReadFile(config.CodexConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CodexConfigPath(), []byte(strings.Replace(string(installed), `model = "gpt-6.1-sol"`, `model = "wopal-ai/glm-5.3-flash"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := restoreForTest(t); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := os.ReadFile(config.CodexConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), `model = "gpt-6.1-sol"`) {
		t.Fatalf("restore did not return to the pre-install model:\n%s", restored)
	}
}

func TestRecordExposedExternalModelsAccumulatesOnlyForInstalledGateway(t *testing.T) {
	root := t.TempDir()
	restoreRoot := config.SetProjectRootForTest(root)
	t.Cleanup(restoreRoot)
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := RecordExposedExternalModels([]string{"ignored"}); err != nil {
		t.Fatalf("record without install: %v", err)
	}
	if _, err := os.Stat(config.RestoreStatePath()); !os.IsNotExist(err) {
		t.Fatalf("record without install created restore state: %v", err)
	}

	if err := writeCodexRestoreState(CodexRestoreState{ExposedExternalModels: []string{"old-alias"}}); err != nil {
		t.Fatal(err)
	}
	if err := RecordExposedExternalModels([]string{"new-alias", "old-alias", "vendor/model"}); err != nil {
		t.Fatal(err)
	}
	state, found, err := readCodexRestoreState()
	if err != nil || !found {
		t.Fatalf("read state found=%t err=%v", found, err)
	}
	if got, want := strings.Join(state.ExposedExternalModels, ","), "old-alias,new-alias,vendor/model"; got != want {
		t.Fatalf("exposed external models = %q, want %q", got, want)
	}
}

func TestUpdateManagedMemoriesModelOnlyTouchesActiveGatewayConfig(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	restoreRoot := config.SetProjectRootForTest(root)
	t.Cleanup(restoreRoot)
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.CodexConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	gatewayBaseURL := "http://127.0.0.1:11435/v1"
	original := "model = \"gpt-6.1-sol\"\n" +
		"model_catalog_json = " + strconv.Quote(config.CodexGatewayCatalogPath()) + "\n" +
		"openai_base_url = " + strconv.Quote(gatewayBaseURL) + "\n\n" +
		"[memories]\n" +
		"extract_model = \"wopal-ai/old\"\n" +
		"consolidation_model = \"wopal-ai/old\"\n\n" +
		"[other]\nkeep = true\n"
	if err := os.WriteFile(config.CodexConfigPath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, err := UpdateManagedMemoriesModel(gatewayBaseURL, "vendor/friendly")
	if err != nil {
		t.Fatalf("UpdateManagedMemoriesModel: %v", err)
	}
	if !updated {
		t.Fatal("managed Codex config was not updated")
	}
	data, err := os.ReadFile(config.CodexConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, `vendor/friendly`) != 2 {
		t.Fatalf("memory models were not updated in both fields:\n%s", got)
	}
	if !strings.Contains(got, "[other]\nkeep = true") {
		t.Fatalf("unrelated Codex config was changed:\n%s", got)
	}

	updated, err = UpdateManagedMemoriesModel("http://127.0.0.1:9999/v1", "vendor/other")
	if err != nil || updated {
		t.Fatalf("non-gateway config update = %v, %v", updated, err)
	}
	after, err := os.ReadFile(config.CodexConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != got {
		t.Fatal("non-gateway Codex config was modified")
	}
}
