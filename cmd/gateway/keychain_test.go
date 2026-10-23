package main

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestKeychainGetWopalAPIKey(t *testing.T) {
	original := keychainExec
	t.Cleanup(func() { keychainExec = original })

	var gotArgs []string
	keychainExec = func(args []string, _ io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, _ = io.WriteString(stdout, "  test-wopal-key\n")
		return nil
	}

	key, err := getWopalAPIKey()
	if err != nil {
		t.Fatalf("getWopalAPIKey() error = %v", err)
	}
	if key != "test-wopal-key" {
		t.Fatalf("getWopalAPIKey() = %q, want %q", key, "test-wopal-key")
	}
	wantArgs := []string{"find-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-w"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("security args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestKeychainGetWopalAPIKeyRequiresConfiguredKey(t *testing.T) {
	original := keychainExec
	t.Cleanup(func() { keychainExec = original })

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
			keychainExec = tc.run
			_, err := getWopalAPIKey()
			if err == nil || err.Error() != "Wopal AI API key is not configured. Run: wopal-codex-gateway auth set" {
				t.Fatalf("getWopalAPIKey() error = %v", err)
			}
		})
	}
}

func TestKeychainSetWopalAPIKeyInteractive(t *testing.T) {
	original := keychainExec
	t.Cleanup(func() { keychainExec = original })

	var gotArgs []string
	keychainExec = func(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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

	if err := setWopalAPIKeyInteractive(); err != nil {
		t.Fatalf("setWopalAPIKeyInteractive() error = %v", err)
	}
	wantArgs := []string{"add-generic-password", "-a", "wopal-ai", "-s", "com.wopal.codex-gateway.wopal-ai", "-U", "-w"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("security args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestKeychainSetWopalAPIKeyInteractiveReportsFailure(t *testing.T) {
	original := keychainExec
	t.Cleanup(func() { keychainExec = original })

	keychainExec = func([]string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("security failed")
	}
	err := setWopalAPIKeyInteractive()
	if err == nil || !strings.Contains(err.Error(), "security failed") {
		t.Fatalf("setWopalAPIKeyInteractive() error = %v", err)
	}
}
