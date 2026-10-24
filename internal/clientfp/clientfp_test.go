package clientfp

import (
	"net/http"
	"testing"
)

func TestFromPrefersVersionHeaderThenUserAgent(t *testing.T) {
	header := http.Header{}
	header.Set("User-Agent", "Codex Desktop/0.164.0-alpha.11.1 (Mac OS 26.6.2; arm64) dumb")
	header.Set("version", "0.165.0-alpha.1")
	header.Set("originator", "Codex Desktop")
	header.Set("Content-Encoding", "Zstd")
	header.Set("x-codex-beta-features", "remote_compaction_v2")

	got := From(header)
	if got.Version != "0.165.0-alpha.1" {
		t.Fatalf("version = %q, want the explicit header", got.Version)
	}
	if got.Encoding != "zstd" {
		t.Fatalf("encoding = %q, want lowercased zstd", got.Encoding)
	}
	if got.Originator != "Codex Desktop" {
		t.Fatalf("originator = %q", got.Originator)
	}
	if got.BetaFeatures != "remote_compaction_v2" {
		t.Fatalf("beta features = %q", got.BetaFeatures)
	}

	// A client that stops sending the header must still be identifiable, because
	// dropping a header is itself a release change worth noticing.
	fallback := http.Header{}
	fallback.Set("User-Agent", "Codex Desktop/0.163.0-alpha.10.2 (Mac OS 26.6.2; arm64) dumb")
	if version := From(fallback).Version; version != "0.163.0-alpha.10.2" {
		t.Fatalf("fallback version = %q, want parsed from user agent", version)
	}
}

func TestFromToleratesMissingAndHostileHeaders(t *testing.T) {
	if got := From(nil); got != (Fingerprint{}) {
		t.Fatalf("nil header produced %#v", got)
	}
	header := http.Header{}
	header.Set("User-Agent", "bad\r\ninjected\tvalue")
	got := From(header)
	if got.UserAgent != "bad injected value" {
		t.Fatalf("user agent = %q, want control characters folded", got.UserAgent)
	}
}

func TestFieldsOmitEmptyValues(t *testing.T) {
	fields := Fingerprint{Version: "1.2.3"}.Fields()
	if len(fields) != 1 || fields["client_version"] != "1.2.3" {
		t.Fatalf("fields = %#v, want only client_version", fields)
	}
}
