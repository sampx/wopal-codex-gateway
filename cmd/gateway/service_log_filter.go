package main

import (
	"bytes"
	"io"
	"sync"
)

// serviceLogFilterWriter removes only CLIProxyAPI's model-substitution warning.
// External providers are allowed to map a client-visible model name to another
// served model, so this warning is false-positive noise for the gateway. Other
// warnings and errors must remain untouched.
type serviceLogFilterWriter struct {
	dst io.Writer

	mu      sync.Mutex
	pending bytes.Buffer
}

func newServiceLogFilter(dst io.Writer) io.Writer {
	if dst == nil {
		return nil
	}
	return &serviceLogFilterWriter{dst: dst}
}

func (w *serviceLogFilterWriter) Write(p []byte) (int, error) {
	if w == nil || w.dst == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending.Write(p)
	w.drainLocked(false)
	return len(p), nil
}

func (w *serviceLogFilterWriter) Flush() {
	if w == nil || w.dst == nil {
		return
	}
	w.mu.Lock()
	w.drainLocked(true)
	w.mu.Unlock()
	if flusher, ok := w.dst.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (w *serviceLogFilterWriter) drainLocked(final bool) {
	for {
		data := w.pending.Bytes()
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if final && len(data) > 0 {
				w.writeLineLocked(data)
				w.pending.Reset()
			}
			return
		}
		line := append([]byte(nil), data[:newline+1]...)
		w.pending.Next(newline + 1)
		w.writeLineLocked(line)
	}
}

func (w *serviceLogFilterWriter) writeLineLocked(line []byte) {
	if isModelSubstitutionWarning(line) {
		return
	}
	_, _ = w.dst.Write(line)
}

func isModelSubstitutionWarning(line []byte) bool {
	return bytes.Contains(line, []byte(" executor: upstream served model ")) &&
		bytes.Contains(line, []byte(" for requested model ")) &&
		bytes.Contains(line, []byte("(auth_index="))
}
