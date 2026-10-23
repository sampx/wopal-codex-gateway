package main

import (
	"bytes"
	"io"
	"sync"
)

// gin writes recovered panics to stderr as "\n\n\x1b[31m[Recovery] ... \x1b[0m".
// A client disconnect mid-stream makes httputil.ReverseProxy panic with
// http.ErrAbortHandler, which is normal and already handled; gin prints a full
// stack for it anyway. Those traces were the bulk of service-error.log, so the
// stderr stream drops blocks whose text is abort-handler noise.
const (
	recoveryBlockStart = "\x1b[31m"
	recoveryBlockEnd   = "\x1b[0m"
	abortHandlerText   = "net/http: abort Handler"
)

// abortHandlerFilterWriter buffers stderr output and discards recovery blocks
// that only report an aborted request. Everything else is passed through with
// minimal delay: output between blocks is forwarded as soon as it arrives.
type abortHandlerFilterWriter struct {
	dst io.Writer

	mu      sync.Mutex
	pending bytes.Buffer
}

func newAbortHandlerFilterWriter(dst io.Writer) io.Writer {
	if dst == nil {
		return nil
	}
	return &abortHandlerFilterWriter{dst: dst}
}

func (w *abortHandlerFilterWriter) Write(p []byte) (int, error) {
	if w == nil || w.dst == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending.Write(p)
	w.drainLocked(false)
	return len(p), nil
}

// flush forwards any buffered partial block. Call when the stream is closing.
func (w *abortHandlerFilterWriter) flush() {
	if w == nil || w.dst == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drainLocked(true)
}

func (w *abortHandlerFilterWriter) drainLocked(final bool) {
	for {
		data := w.pending.Bytes()
		start := bytes.Index(data, []byte(recoveryBlockStart))
		if start < 0 {
			// No recovery block in flight: forward everything we have.
			if len(data) > 0 {
				_, _ = w.dst.Write(data)
				w.pending.Reset()
			}
			return
		}
		if start > 0 {
			_, _ = w.dst.Write(data[:start])
			w.pending.Next(start)
			data = w.pending.Bytes()
		}
		end := bytes.Index(data, []byte(recoveryBlockEnd))
		if end < 0 {
			if final {
				_, _ = w.dst.Write(data)
				w.pending.Reset()
			}
			return
		}
		end += len(recoveryBlockEnd)
		block := data[:end]
		if !bytes.Contains(block, []byte(abortHandlerText)) {
			_, _ = w.dst.Write(block)
		}
		w.pending.Next(end)
	}
}
