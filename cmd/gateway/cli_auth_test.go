package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
