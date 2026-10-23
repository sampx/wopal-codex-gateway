package activity

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"wopal-codex-gateway/internal/config"
)

// Log rotation retention. Size and backup counts come from runtime config so a
// long-running daemon stays bounded without hiding operator-tunable limits.
const (
	maxLogAgeDays  = 14
	compressLogRot = true
)

// NewRotatingWriter returns a size and age bounded writer for one log file.
// lumberjack is safe for concurrent use, so no external mutex is needed.
func NewRotatingWriter(path string, cfg config.Logs) io.WriteCloser {
	cfg = normalizeLogsConfig(cfg)
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     maxLogAgeDays,
		Compress:   compressLogRot,
		LocalTime:  true,
	}
}

func normalizeLogsConfig(cfg config.Logs) config.Logs {
	if cfg.MaxSizeMB <= 0 {
		cfg.MaxSizeMB = config.DefaultLogMaxSizeMB
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = config.DefaultLogMaxBackups
	}
	return cfg
}

type Logger struct {
	path string
	mu   sync.Mutex
	out  io.WriteCloser
}

// New opens the activity log inside logsDir, creating the directory first.
func New(logsDir string, cfg config.Logs) (*Logger, error) {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(logsDir, "gateway.log")
	return &Logger{path: path, out: NewRotatingWriter(path, cfg)}, nil
}

func (l *Logger) Write(fields map[string]any) {
	if l == nil {
		return
	}
	entry := make(map[string]any, len(fields)+1)
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	for key, value := range fields {
		entry[key] = value
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return
	}
	_, _ = l.out.Write(append(payload, '\n'))
}

func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_ = l.out.Close()
		l.out = nil
	}
}
