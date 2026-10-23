package proxy

import (
	"testing"

	"wopal-codex-gateway/internal/compaction"
)

func TestWopalSessionReleaseDropsTranscriptReferences(t *testing.T) {
	s := &wopalWebsocketSession{state: compaction.ConversationState{
		LastRequest:        []byte("request-bytes"),
		LastResponseOutput: []byte("output-bytes"),
		LastResponseID:     "r1",
	}}
	want := len(s.state.LastRequest) + len(s.state.LastResponseOutput)
	if got := s.releaseState(); got != want {
		t.Fatalf("released=%d want=%d", got, want)
	}
	if s.state.LastRequest != nil || s.state.LastResponseOutput != nil || s.state.LastResponseID != "" {
		t.Fatalf("state retained after release: %#v", s.state)
	}
}

func TestWopalSessionStateSizeGuard(t *testing.T) {
	if err := validateWopalSessionStateSize(maxWopalSessionStateBytes-1, 1); err != nil {
		t.Fatalf("boundary state rejected: %v", err)
	}
	if err := validateWopalSessionStateSize(maxWopalSessionStateBytes, 1); err == nil {
		t.Fatal("oversized session state was accepted")
	}
}
