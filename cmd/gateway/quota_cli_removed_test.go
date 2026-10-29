package main

import (
	"strings"
	"testing"
)

func TestQuotaResetLearningIsNotACommandOrHelpEntry(t *testing.T) {
	if err := run([]string{"quota", "reset-learning"}); err == nil {
		t.Fatal("quota reset-learning is still accepted")
	}
	if strings.Contains(helpText, "reset-learning") {
		t.Fatalf("help still exposes reset-learning: %s", helpText)
	}
}
