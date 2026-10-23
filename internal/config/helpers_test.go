package config

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempGatewayRoot points every path helper at a fresh temp directory for the
// duration of one test.
func useTempGatewayRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Cleanup(SetProjectRootForTest(root))
	if err := os.MkdirAll(filepath.Dir(LaunchAgentTemplatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
