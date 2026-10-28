package main

import (
	"strings"
	"testing"
)

// Every command group must answer -h/--help with the command list. Before this
// existed, "providers --help" failed as an unknown subcommand and
// "models add --help" failed as bad usage, so the binary looked like it had no
// help flag at all.
func TestCommandGroupsAnswerHelpFlags(t *testing.T) {
	cases := [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"providers", "--help"},
		{"providers", "-h"},
		{"providers", "help"},
		{"models", "--help"},
		{"models", "add", "--help"},
		{"config", "show", "-h"},
		{"quota", "--help"},
		{"codex", "install", "--help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			output, err := captureConfigAuthTestStdout(t, func() error { return run(args) })
			if err != nil {
				t.Fatalf("run %v: %v", args, err)
			}
			if !strings.Contains(output, "providers set <provider>") {
				t.Fatalf("run %v printed no command list: %q", args, output)
			}
		})
	}
}

// A model named "help" must stay addressable: only the group's own first
// argument counts as a help request.
func TestHelpSubcommandIsNotTreatedAsModelName(t *testing.T) {
	if requestsHelp([]string{"add", "help"}) {
		t.Fatal("models add help must not be interpreted as a help request")
	}
	if !requestsHelp([]string{"help"}) {
		t.Fatal("group-level help must be interpreted as a help request")
	}
}

// A flag value that happens to be "-h" or "--help" is data, not a help
// request. Treating it as help would silently skip the command and exit 0.
func TestHelpFlagsAsOptionValuesAreNotHelpRequests(t *testing.T) {
	cases := [][]string{
		{"config", "role", "set", "compaction", "--provider", "vendor", "--model", "-h"},
		{"config", "role", "set", "compaction", "--provider", "vendor", "--model", "--help"},
		{"models", "add", "help", "--provider", "vendor"},
		{"models", "set", "x", "--alias", "--help"},
		{"providers", "set", "vendor", "--header", "x-help=--help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if requestsHelp(args) {
				t.Fatalf("requestsHelp(%v) = true, want false: the token is an option value", args)
			}
		})
	}
}

// serve parses its own flags, so its help request arrives as flag.ErrHelp
// rather than through requestsHelp. It must still exit successfully.
func TestServeHelpExitsSuccessfully(t *testing.T) {
	if err := run([]string{"serve", "--help"}); err != nil {
		t.Fatalf("serve --help = %v, want nil", err)
	}
}
