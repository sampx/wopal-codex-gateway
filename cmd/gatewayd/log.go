package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type activityLogger struct {
	path string
	mu   sync.Mutex
}

func newActivityLogger(root string) (*activityLogger, error) {
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	return &activityLogger{path: filepath.Join(logsDir, "gateway.log")}, nil
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
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(payload, '\n'))
}
