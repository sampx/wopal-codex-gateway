package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// requestLogPurgeInterval bounds how long a request-body log can survive on
// disk. The embedded CLIProxyAPI writes error logs with full request bodies
// even when request logging is disabled (it forces them on failures), and it
// exposes no switch to turn that off. The gateway therefore deletes those
// artifacts continuously instead of persisting prompt text.
const requestLogPurgeInterval = 5 * time.Second

type requestLogPurgeResult struct {
	Files int
	Dirs  int
	Bytes int64
}

// purgeRequestBodyLogs removes CLIProxyAPI request-body artifacts from the
// auth directory's logs folder. Only files and part directories created by the
// request logger are touched.
func purgeRequestBodyLogs(authDir string) requestLogPurgeResult {
	var result requestLogPurgeResult
	logsDir := filepath.Join(authDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(logsDir, name)
		if entry.IsDir() {
			if strings.HasPrefix(name, "request-log-parts-") {
				if size, err := directorySize(path); err == nil {
					result.Bytes += size
				}
				if err := os.RemoveAll(path); err == nil {
					result.Dirs++
				}
			}
			continue
		}
		if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if err := os.Remove(path); err == nil {
			result.Files++
			result.Bytes += info.Size()
		}
	}
	return result
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// startRequestBodyLogJanitor purges request-body logs immediately and then on
// every interval until the context is cancelled. Purges are reported as
// metadata-only activity events.
func startRequestBodyLogJanitor(ctx context.Context, authDir string, logger *activityLogger) {
	purge := func() {
		result := purgeRequestBodyLogs(authDir)
		if result.Files == 0 && result.Dirs == 0 {
			return
		}
		if logger != nil {
			logger.write(map[string]any{
				"event": "request_body_logs_purged",
				"files": result.Files,
				"dirs":  result.Dirs,
				"bytes": result.Bytes,
			})
		}
	}
	purge()
	go func() {
		ticker := time.NewTicker(requestLogPurgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				purge()
			}
		}
	}()
}
