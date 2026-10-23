package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestClassifyModel(t *testing.T) {
	cases := map[string]routeKind{
		"gpt-6.1-sol":                  routeOfficial,
		"wopal-ai/deepseek-v4.1-flash": routeWopal,
		"ollama/qwen3":                 routeDisabled,
		"other/model":                  routeDisabled,
		"":                             routeUnknown,
	}
	for model, want := range cases {
		if got := classifyModel(model); got != want {
			t.Fatalf("classifyModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestDetectRequestModelPrefersRoutingHint(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Codex-Routing-Hint", "model=wopal-ai/glm-5.3-flash;tier=auto")
	got, err := detectRequestModel(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wopal-ai/glm-5.3-flash" {
		t.Fatalf("model = %q", got)
	}
}

func TestDetectRequestModelPreservesCompressedBody(t *testing.T) {
	payload := []byte(`{"model":"wopal-ai/deepseek-v4.1-flash","input":"hi"}`)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll(payload, nil)
	enc.Close()

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "zstd")
	got, err := detectRequestModel(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wopal-ai/deepseek-v4.1-flash" {
		t.Fatalf("model = %q", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, compressed) {
		t.Fatal("routing inspection modified compressed request body")
	}
}

func TestDecodeRoutingBodyGzip(t *testing.T) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(`{"model":"gpt-6.1-sol"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRoutingBody(buffer.Bytes(), "gzip")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "gpt-6.1-sol") {
		t.Fatalf("decoded = %s", decoded)
	}
}
