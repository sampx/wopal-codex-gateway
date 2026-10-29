package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestServiceLogFilterSuppressesOnlyModelSubstitutionWarning(t *testing.T) {
	var out bytes.Buffer
	filter := newServiceLogFilter(&out)
	lines := []string{
		`level=warning msg="openai-compatibility executor: upstream served model \"gpt-served\" for requested model \"alias\" (auth_index=1)"` + "\n",
		`level=warning msg="openai-compatibility executor: upstream returned malformed usage metadata"` + "\n",
		`level=warning msg="upstream served model changed for another reason"` + "\n",
	}
	for _, line := range lines {
		if _, err := io.WriteString(filter, line); err != nil {
			t.Fatal(err)
		}
	}
	if flusher, ok := filter.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	got := out.String()
	if strings.Contains(got, "upstream served model \"gpt-served\"") {
		t.Fatalf("model substitution warning was not suppressed: %s", got)
	}
	for _, want := range []string{"malformed usage metadata", "changed for another reason"} {
		if !strings.Contains(got, want) {
			t.Fatalf("neighbor warning %q was suppressed: %s", want, got)
		}
	}
}
