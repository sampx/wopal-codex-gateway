package keychain

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestKeychainGetWopalAPIKey(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	var gotArgs []string
	KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "  test-wopal-key\n")
		return nil
	}

	key, err := GetWopalAPIKey()
	if err != nil {
		t.Fatalf("GetWopalAPIKey() error = %v", err)
	}
	if key != "test-wopal-key" {
		t.Fatalf("GetWopalAPIKey() = %q, want %q", key, "test-wopal-key")
	}
	wantArgs := []string{"find-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-w"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("security args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestKeychainGetWopalAPIKeyRequiresConfiguredKey(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	for _, tc := range []struct {
		name string
		run  func([]string, io.Reader, io.Writer, io.Writer) error
	}{
		{name: "command error", run: func([]string, io.Reader, io.Writer, io.Writer) error { return errors.New("not found") }},
		{name: "empty output", run: func(_ []string, _ io.Reader, stdout, _ io.Writer) error {
			_, _ = io.WriteString(stdout, "  \n")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			KeychainExec = tc.run
			_, err := GetWopalAPIKey()
			if err == nil || err.Error() != "Wopal AI API key is not configured. Run: wopal-codex-gateway auth set" {
				t.Fatalf("GetWopalAPIKey() error = %v", err)
			}
		})
	}
}

func TestKeychainSetWopalAPIKeyInteractive(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	var gotArgs []string
	KeychainExec = func(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		gotArgs = append([]string(nil), args...)
		if stdin != os.Stdin {
			t.Errorf("stdin was not passed through")
		}
		if stdout != os.Stdout {
			t.Errorf("stdout was not passed through")
		}
		if stderr != os.Stderr {
			t.Errorf("stderr was not passed through")
		}
		return nil
	}

	if err := SetWopalAPIKeyInteractive(); err != nil {
		t.Fatalf("SetWopalAPIKeyInteractive() error = %v", err)
	}
	wantArgs := []string{"add-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-U", "-w"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("security args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestKeychainSetWopalAPIKeyInteractiveReportsFailure(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	KeychainExec = func([]string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("security failed")
	}
	err := SetWopalAPIKeyInteractive()
	if err == nil || !strings.Contains(err.Error(), "security failed") {
		t.Fatalf("SetWopalAPIKeyInteractive() error = %v", err)
	}
}

func TestProviderAPIKeyUsesSharedServiceAndProviderAccount(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	var calls [][]string
	KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "find-generic-password" {
			_, _ = io.WriteString(stdout, "provider-secret\n")
		}
		return nil
	}

	key, err := GetProviderAPIKey("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if key != "provider-secret" {
		t.Fatalf("key = %q", key)
	}
	if err := SetProviderAPIKeyInteractive("deepseek"); err != nil {
		t.Fatal(err)
	}

	wantGet := []string{"find-generic-password", "-a", "opencode-go", "-s", "com.wopal.codex-gateway.providers", "-w"}
	wantSet := []string{"add-generic-password", "-a", "deepseek", "-s", "com.wopal.codex-gateway.providers", "-U", "-w"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], wantGet) || !reflect.DeepEqual(calls[1], wantSet) {
		t.Fatalf("security calls = %#v, want %#v then %#v", calls, wantGet, wantSet)
	}
}

func TestProviderAPIKeyKeepsWopalLegacyLocation(t *testing.T) {
	original := KeychainExec
	t.Cleanup(func() { KeychainExec = original })

	var gotArgs []string
	KeychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "legacy-key\n")
		return nil
	}
	key, err := GetProviderAPIKey("wopal-ai")
	if err != nil || key != "legacy-key" {
		t.Fatalf("GetProviderAPIKey(wopal-ai) = %q, %v", key, err)
	}
	want := []string{"find-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-w"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("security args = %#v, want %#v", gotArgs, want)
	}
}
