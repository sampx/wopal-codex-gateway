// Package clientfp identifies the Codex build behind a request.
//
// Codex ships several times a week and every release is free to change the
// request shape the gateway has to accept: cipher of the body, which tool
// surface it declares, which beta headers it sends. When a release breaks the
// proxy, the only way to answer "what changed" is to know which build produced
// the request. The gateway therefore records a small, stable fingerprint on
// every logged request instead of guessing from a failure later.
package clientfp

import (
	"net/http"
	"regexp"
	"strings"
)

// Fingerprint is the metadata-only client identity recorded in the activity
// log. Every field is a header value or a bounded summary; no request content
// is retained.
type Fingerprint struct {
	// Version is the Codex CLI version, taken from the `version` header that
	// Codex sends on both HTTP and websocket requests, or parsed from the user
	// agent when the header is absent.
	Version string

	// Originator is the surface that issued the request ("Codex Desktop",
	// "codex_exec", ...). It is a header value, not a capability.
	Originator string

	// UserAgent is the sanitized user agent. It is the only field that survives
	// for clients that send neither `version` nor `originator`.
	UserAgent string

	// Encoding is the Content-Encoding the client chose. It matters because a
	// new release can start compressing with an algorithm the gateway cannot
	// decode.
	Encoding string

	// BetaFeatures is the value of `x-codex-beta-features`, the negotiated
	// feature set. A change here is the earliest warning that the request
	// contract moved.
	BetaFeatures string

	// RoutingHintModel is parsed from `x-codex-routing-hint`.
	RoutingHintModel string
}

// versionInUserAgent matches a semver-ish token inside a user agent.
var versionInUserAgent = regexp.MustCompile(`\d+\.\d+\.\d+[-+0-9A-Za-z.]*`)

// From extracts the fingerprint from request headers. A nil request yields an
// empty fingerprint rather than a panic, so logging can never take a request
// path down.
func From(header http.Header) Fingerprint {
	if header == nil {
		return Fingerprint{}
	}
	userAgent := sanitize(header.Get("User-Agent"))
	version := sanitize(header.Get("version"))
	if version == "" {
		version = versionInUserAgent.FindString(userAgent)
	}
	return Fingerprint{
		Version:          version,
		Originator:       sanitize(header.Get("originator")),
		UserAgent:        userAgent,
		Encoding:         strings.ToLower(sanitize(header.Get("Content-Encoding"))),
		BetaFeatures:     sanitize(header.Get("x-codex-beta-features")),
		RoutingHintModel: sanitize(header.Get("x-codex-routing-hint")),
	}
}

// Fields returns the fingerprint as log fields. Empty values are omitted so a
// missing header stays visibly missing instead of looking like an empty string
// the client really sent.
func (f Fingerprint) Fields() map[string]any {
	fields := make(map[string]any, 6)
	for key, value := range map[string]string{
		"client_version":    f.Version,
		"client_originator": f.Originator,
		"client_user_agent": f.UserAgent,
		"client_encoding":   f.Encoding,
		"client_features":   f.BetaFeatures,
	} {
		if value != "" {
			fields[key] = value
		}
	}
	return fields
}

// summarizeUserAgent truncates a user agent so a hostile client cannot grow the
// log entry without bound.
const maxUserAgent = 200

func sanitize(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maxUserAgent {
		return value[:maxUserAgent]
	}
	return value
}
