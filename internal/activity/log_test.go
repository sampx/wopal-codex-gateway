package activity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"wopal-codex-gateway/internal/config"
)

func TestLogActivityLoggerAdoptsConfigAndRotates(t *testing.T) {
	root := t.TempDir()
	logger, err := New(filepath.Join(root, "logs"), config.Logs{MaxSizeMB: 1, MaxBackups: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)

	rotator, ok := logger.out.(*lumberjack.Logger)
	if !ok {
		t.Fatalf("activity logger writer type = %T, want *lumberjack.Logger", logger.out)
	}
	if rotator.MaxSize != 1 || rotator.MaxBackups != 2 {
		t.Fatalf("rotation limits = %d MB / %d backups, want 1 MB / 2 backups", rotator.MaxSize, rotator.MaxBackups)
	}

	// Each entry is smaller than the 1 MiB threshold, but every write after the
	// first exceeds it, forcing real rotations rather than only checking fields.
	blob := strings.Repeat("x", 600*1024)
	for i := 0; i < 5; i++ {
		logger.Write(map[string]any{"event": "probe", "payload": blob})
		time.Sleep(10 * time.Millisecond)
	}
	logger.Close()

	live, err := os.Stat(filepath.Join(root, "logs", "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	if live.Size() > int64(rotator.MaxSize)<<20 {
		t.Fatalf("live log grew to %d bytes, want <= %d MiB", live.Size(), rotator.MaxSize)
	}
	backups := waitForRotatedLogs(t, filepath.Join(root, "logs"), 2)
	if len(backups) != rotator.MaxBackups {
		t.Fatalf("kept %d backups, want %d: %v", len(backups), rotator.MaxBackups, backups)
	}
}

func TestLogRotatingWriterZeroConfigUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	writer := NewRotatingWriter(path, config.Logs{})
	defer writer.Close()

	rotator, ok := writer.(*lumberjack.Logger)
	if !ok {
		t.Fatalf("writer type = %T, want *lumberjack.Logger", writer)
	}
	if rotator.MaxSize != config.DefaultLogMaxSizeMB || rotator.MaxBackups != config.DefaultLogMaxBackups {
		t.Fatalf("rotation defaults = %d MB / %d backups, want %d MB / %d backups",
			rotator.MaxSize, rotator.MaxBackups, config.DefaultLogMaxSizeMB, config.DefaultLogMaxBackups)
	}
	if rotator.MaxAge != maxLogAgeDays || !rotator.Compress {
		t.Fatalf("rotation retention = age %d compress %t, want age %d compress true", rotator.MaxAge, rotator.Compress, maxLogAgeDays)
	}
}

func waitForRotatedLogs(t *testing.T, dir string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		backups, err := filepath.Glob(filepath.Join(dir, "gateway-*.log*"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := 0
		for _, backup := range backups {
			if strings.HasSuffix(backup, ".gz") {
				compressed++
			}
		}
		if len(backups) == want && compressed == want {
			return backups
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated logs = %v, want %d compressed backups", backups, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
