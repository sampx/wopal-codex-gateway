package compaction

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"wopal-codex-gateway/internal/routing"
)

// DecodeBody decodes a request body according to Content-Encoding. It lives
// here rather than in routing because zstd is a third-party dependency and
// routing must stay standard-library only.
func DecodeBody(raw []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode gzip routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, routing.MaxBodyBytes+1))
	case "deflate":
		reader, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode deflate routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, routing.MaxBodyBytes+1))
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decode zstd routing body: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, routing.MaxBodyBytes+1))
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding for routing: %s", encoding)
	}
}
