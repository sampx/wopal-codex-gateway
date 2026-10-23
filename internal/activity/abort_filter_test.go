package activity

import (
	"bytes"
	"strings"
	"testing"
)

func TestAbortHandlerFilterDropsRecoveryNoise(t *testing.T) {
	var out bytes.Buffer
	w := NewAbortHandlerFilterWriter(&out)

	noise := "\n\n\x1b[31m[Recovery] panic recovered:\nnet/http: abort Handler\nstack...\x1b[0m"
	if _, err := w.Write([]byte(noise)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "abort Handler") {
		t.Fatalf("abort-handler noise was forwarded: %q", out.String())
	}

	real := "\n\n\x1b[31m[Recovery] panic recovered:\nboom\nstack...\x1b[0m"
	if _, err := w.Write([]byte(real)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Fatalf("genuine panic was dropped: %q", out.String())
	}
}

func TestAbortHandlerFilterForwardsPlainOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewAbortHandlerFilterWriter(&out)
	if _, err := w.Write([]byte("plain line\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "plain line\n" {
		t.Fatalf("plain output not forwarded: %q", out.String())
	}
}
