package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Log rotation retention. Size and backup counts come from runtime config so a
// long-running daemon stays bounded without hiding operator-tunable limits.
const (
	maxLogAgeDays  = 14
	compressLogRot = true
)

// newRotatingWriter returns a size and age bounded writer for one log file.
// lumberjack is safe for concurrent use, so no external mutex is needed.
func newRotatingWriter(path string, cfg logsConfig) io.WriteCloser {
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

func normalizeLogsConfig(cfg logsConfig) logsConfig {
	if cfg.MaxSizeMB <= 0 {
		cfg.MaxSizeMB = defaultLogMaxSizeMB
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = defaultLogMaxBackups
	}
	return cfg
}

type activityLogger struct {
	path string
	mu   sync.Mutex
	out  io.WriteCloser
}

// The optional config keeps existing in-package callers that already receive
// parser-filled defaults source-compatible while the daemon passes its config.
func newActivityLogger(root string, cfgs ...logsConfig) (*activityLogger, error) {
	cfg := logsConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(logsDir, "gateway.log")
	return &activityLogger{path: path, out: newRotatingWriter(path, cfg)}, nil
}

func (l *activityLogger) write(fields map[string]any) {
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

func (l *activityLogger) close() {
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
