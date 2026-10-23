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

// Log rotation limits. Every file the gateway writes is capped so a long
// running daemon can never fill the disk: at most maxLogBackups previous
// files of maxLogSizeMB each, kept for maxLogAgeDays.
const (
	maxLogSizeMB   = 10
	maxLogBackups  = 3
	maxLogAgeDays  = 14
	compressLogRot = true
)

// newRotatingWriter returns a size and age bounded writer for one log file.
// lumberjack is safe for concurrent use, so no external mutex is needed.
func newRotatingWriter(path string) io.WriteCloser {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxLogSizeMB,
		MaxBackups: maxLogBackups,
		MaxAge:     maxLogAgeDays,
		Compress:   compressLogRot,
		LocalTime:  true,
	}
}

type activityLogger struct {
	path string
	mu   sync.Mutex
	out  io.WriteCloser
}

func newActivityLogger(root string) (*activityLogger, error) {
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(logsDir, "gateway.log")
	return &activityLogger{path: path, out: newRotatingWriter(path)}, nil
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
