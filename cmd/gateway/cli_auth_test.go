package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

func TestAuthCommandStatus(t *testing.T) {
	setupConfigAuthTestRoot(t)
	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })

	t.Run("configured", func(t *testing.T) {
		keychain.KeychainExec = func(_ []string, _ io.Reader, stdout, _ io.Writer) error {
			_, _ = io.WriteString(stdout, "stored-key\n")
			return nil
		}
		output, err := captureConfigAuthTestStdout(t, func() error { return runAuthCommand("status", nil) })
		if err != nil {
			t.Fatalf("runAuthCommand(status) error = %v", err)
		}
		if output != "Wopal AI API key: configured\n" {
			t.Fatalf("output = %q", output)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		keychain.KeychainExec = func([]string, io.Reader, io.Writer, io.Writer) error {
			return io.EOF
		}
		output, err := captureConfigAuthTestStdout(t, func() error { return runAuthCommand("status", nil) })
		if err != nil {
			t.Fatalf("runAuthCommand(status) error = %v", err)
		}
		if output != "Wopal AI API key: not configured\n" {
			t.Fatalf("output = %q", output)
		}
	})
}

func TestAuthCommandTestValidatesStoredKey(t *testing.T) {
	setupConfigAuthTestRoot(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer stored-key" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer server.Close()

	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatalf("config.LoadGatewayConfig() error = %v", err)
	}
	cfg.WopalAI.BaseURL = server.URL
	cfg.WopalAI.Models = []config.ExternalModelConfig{{Name: "a"}, {Name: "missing"}}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatalf("config.SaveGatewayConfig() error = %v", err)
	}

	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })
	keychain.KeychainExec = func(_ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "stored-key\n")
		return nil
	}

	output, err := captureConfigAuthTestStdout(t, func() error { return runAuthCommand("test", nil) })
	if err != nil {
		t.Fatalf("runAuthCommand(test) error = %v", err)
	}
	want := "Wopal AI API key validation succeeded: 2 models available.\nConfigured model(s) available: a\n"
	if output != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestAuthCommandTestRequiresBaseURL(t *testing.T) {
	setupConfigAuthTestRoot(t)
	err := runAuthCommand("test", nil)
	if err == nil || err.Error() != "Wopal AI base URL is not configured" {
		t.Fatalf("runAuthCommand(test) error = %v", err)
	}
}

func TestAuthCommandSetReportsValidationFailure(t *testing.T) {
	setupConfigAuthTestRoot(t)
	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })

	var calls [][]string
	keychain.KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "find-generic-password" {
			_, _ = io.WriteString(stdout, "saved-key\n")
		}
		return nil
	}

	output, err := captureConfigAuthTestStdout(t, func() error { return runAuthCommand("set", nil) })
	if output != "" {
		t.Fatalf("output = %q, want empty", output)
	}
	wantPrefix := "Wopal AI API key saved in macOS Keychain, but validation failed: "
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("runAuthCommand(set) error = %v", err)
	}
	if len(calls) != 1 || calls[0][0] != "add-generic-password" {
		t.Fatalf("keychain calls = %#v", calls)
	}
}

func TestAuthCommandStatusUsesRequestedProviderAccount(t *testing.T) {
	setupConfigAuthTestRoot(t)
	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["opencode-go"] = config.ProviderConfig{BaseURL: "https://opencode.example/v1"}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })
	var gotArgs []string
	keychain.KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "stored-key\n")
		return nil
	}
	output, err := captureConfigAuthTestStdout(t, func() error {
		return runAuthCommand("status", []string{"--provider", "opencode-go"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "opencode-go API key: configured\n" {
		t.Fatalf("output = %q", output)
	}
	want := []string{"find-generic-password", "-a", "opencode-go", "-s", "com.wopal.codex-gateway.providers"}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("security args = %#v, want %#v", gotArgs, want)
	}
}

func TestAuthCommandTestProviderUsesConfiguredModelsURLAndKey(t *testing.T) {
	setupConfigAuthTestRoot(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer stored-provider-key" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer server.Close()

	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["opencode-go"] = config.ProviderConfig{
		BaseURL:    server.URL + "/inference",
		ModelsURL:  server.URL + "/models",
		ModelsAuth: "provider-key",
	}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })
	keychain.KeychainExec = func(_ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "stored-provider-key\n")
		return nil
	}
	output, err := captureConfigAuthTestStdout(t, func() error {
		return runAuthCommand("test", []string{"--provider", "opencode-go"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "opencode-go API key validation succeeded: 2 models available.\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestAuthCommandTestDiscoveryDisabledProviderNeverCallsNetwork(t *testing.T) {
	setupConfigAuthTestRoot(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["gemini-native"] = config.ProviderConfig{BaseURL: server.URL, Protocol: config.ProtocolGemini}
	if err := config.SaveGatewayConfig(cfg); err != nil {
		t.Fatal(err)
	}

	original := keychain.KeychainExec
	t.Cleanup(func() { keychain.KeychainExec = original })
	keychain.KeychainExec = func(_ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "stored-provider-key\n")
		return nil
	}
	output, err := captureConfigAuthTestStdout(t, func() error {
		return runAuthCommand("test", []string{"--provider", "gemini-native"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != "gemini-native API key: configured; model discovery disabled, network validation skipped.\n" {
		t.Fatalf("output = %q", output)
	}
	if requests.Load() != 0 {
		t.Fatalf("discovery-disabled auth test made %d network request(s)", requests.Load())
	}
}

func TestAuthCommandRejectsUnknownProvider(t *testing.T) {
	setupConfigAuthTestRoot(t)
	err := runAuthCommand("status", []string{"--provider", "missing"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v", err)
	}
}
