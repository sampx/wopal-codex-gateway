package compaction

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func TestDecodeRoutingBodyGzip(t *testing.T) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(`{"model":"gpt-6.1-sol"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBody(buffer.Bytes(), "gzip")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "gpt-6.1-sol") {
		t.Fatalf("decoded = %s", decoded)
	}
}
