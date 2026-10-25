package compaction

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testOfficialCompactionItem() map[string]any {
	return map[string]any{"type": "compaction", "id": "cmp_official", "encrypted_content": "official-openai-capsule-blob"}
}

func containsOfficialSanitizeMarker(payload []byte) bool {
	return bytes.Contains(payload, []byte(CapsulePrefix)) ||
		bytes.Contains(payload, []byte(compactionResponseIDPrefix)) ||
		bytes.Contains(payload, []byte(ReasoningIDPrefix)) ||
		bytes.Contains(payload, []byte("rs_cmb-")) ||
		bytes.Contains(payload, []byte(`"reasoning"`))
}

func TestSanitizeOfficialInputReplacesWopalCapsule(t *testing.T) {
	ConfigureKey("unit-test-secret")
	input := []any{
		testWopalAdditionalTools(),
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		testWopalCompactionItem(t, "cmp_wopal_test-1", "remember ORCHID-7319"),
		testOfficialCompactionItem(),
	}
	expanded, count := sanitizeRequestInput(input)
	if count != 1 {
		t.Fatalf("neutralized count = %d, want 1", count)
	}
	sawSummary := false
	sawUserMessage := false
	sawTools := false
	sawOfficialCapsule := false
	sawWopalCapsule := false
	for _, raw := range expanded {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expanded item is not an object: %v", raw)
		}
		switch item["type"] {
		case "compaction":
			if item["id"] == "cmp_wopal_test-1" {
				sawWopalCapsule = true
			}
			if item["id"] == "cmp_official" {
				if toString(item["encrypted_content"]) != "official-openai-capsule-blob" {
					t.Fatal("official compaction capsule was modified")
				}
				sawOfficialCapsule = true
			}
		case "additional_tools":
			sawTools = true
		case "message":
			if item["role"] == "developer" && strings.Contains(toString(item["content"]), "ORCHID-7319") {
				sawSummary = true
			}
			if item["role"] == "user" && toString(item["content"]) == "hello" {
				sawUserMessage = true
			}
		}
	}
	if sawWopalCapsule {
		t.Fatal("Wopal compaction capsule survived expansion")
	}
	if !sawOfficialCapsule {
		t.Fatal("official compaction capsule was dropped")
	}
	if !sawSummary || !sawUserMessage || !sawTools {
		t.Fatalf("expanded input incomplete: summary=%t user=%t tools=%t", sawSummary, sawUserMessage, sawTools)
	}
}

func TestSanitizeOfficialInputDropsWopalReasoningItems(t *testing.T) {
	ConfigureKey("unit-test-secret")
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		testWopalReasoningItem(),
		map[string]any{"type": "reasoning", "id": "rs_official_reasoning", "encrypted_content": "openai-reasoning-ciphertext"},
	}
	expanded, count := sanitizeRequestInput(input)
	if count != 1 {
		t.Fatalf("neutralized count = %d, want 1", count)
	}
	if len(expanded) != 2 {
		t.Fatalf("expanded length = %d, want 2", len(expanded))
	}
	reasoningCount := 0
	for _, raw := range expanded {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "reasoning" {
			reasoningCount++
			if item["id"] != "rs_official_reasoning" {
				t.Fatalf("unexpected reasoning item: %v", item)
			}
		}
	}
	if reasoningCount != 1 {
		t.Fatalf("reasoning count = %d, want 1 (Wopal reasoning must be dropped)", reasoningCount)
	}
}

func TestSanitizeOfficialInputDropsReasoningWithoutEncryptedContent(t *testing.T) {
	const staleID = "rs_0217926475864278eedaf36e20f5141b2bc84e877dea1f72da2f2_0"
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		map[string]any{"type": "reasoning", "id": staleID, "encrypted_content": "", "summary": []any{}},
		map[string]any{"type": "reasoning", "id": "rs_official_reasoning", "encrypted_content": "openai-reasoning-ciphertext"},
	}
	expanded, count := sanitizeRequestInput(input)
	if count != 1 {
		t.Fatalf("neutralized count = %d, want 1", count)
	}
	if len(expanded) != 2 {
		t.Fatalf("expanded length = %d, want 2", len(expanded))
	}
	for _, raw := range expanded {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "reasoning" && item["id"] != "rs_official_reasoning" {
			t.Fatalf("stale stateless reasoning item survived: %v", item)
		}
	}
}

func TestSanitizeOfficialInputDropsCapsulePrefixWhenRequestCarriesTools(t *testing.T) {
	ConfigureKey("unit-test-secret")
	sealed, err := SealCapsule(Capsule{
		Version: 1,
		ID:      "cmp_wopal_prefix-2",
		Model:   "wopal-ai/deepseek-v4.1-flash-free",
		Summary: "compact summary",
		Prefix:  []json.RawMessage{rawWopalAdditionalTools(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := []any{
		testWopalAdditionalTools(),
		map[string]any{"type": "compaction", "id": "cmp_wopal_prefix-2", "encrypted_content": sealed},
	}
	expanded, count := sanitizeRequestInput(input)
	if count != 1 {
		t.Fatalf("neutralized count = %d, want 1", count)
	}
	tools := 0
	for _, raw := range expanded {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "additional_tools" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("additional_tools count = %d, want 1 (no duplicate declaration)", tools)
	}
}

func TestSanitizeOfficialInputDegradesWhenCapsuleUnreadable(t *testing.T) {
	ConfigureKey("unit-test-secret")
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hi"},
		map[string]any{"type": "compaction", "id": "cmp_wopal_broken", "encrypted_content": "wgcp1.not-a-real-capsule"},
	}
	expanded, count := sanitizeRequestInput(input)
	if count != 1 {
		t.Fatalf("neutralized count = %d, want 1", count)
	}
	if len(expanded) != 2 {
		t.Fatalf("expanded length = %d, want 2", len(expanded))
	}
	item, ok := expanded[1].(map[string]any)
	if !ok || item["type"] != "message" || item["role"] != "developer" {
		t.Fatalf("expected unavailable-summary developer message, got %v", expanded[1])
	}
	if strings.Contains(toString(item["content"]), "not-a-real-capsule") {
		t.Fatal("unavailable message leaked capsule ciphertext")
	}
}

// The proxy reuses one parsed payload for routing and sanitization. This entry
// must mutate that payload only when a rewrite is needed, and the legacy HTTP
// entry must keep its all-or-nothing passthrough contract.
func TestSanitizeParsedRequestReusesPayloadAndLegacyPassthroughStaysExact(t *testing.T) {
	ConfigureKey("parsed-sanitizer-test")
	plain := map[string]any{
		"model": "gpt-6.1-sol",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "keep"}},
	}
	if SanitizeParsedRequest(plain, nil) {
		t.Fatal("plain payload reported a rewrite")
	}

	item := testWopalCompactionItem(t, "cmp_wopal_parsed-1", "remember PARSED-91")
	payload := map[string]any{"model": "gpt-6.1-sol", "input": []any{item}}
	if !SanitizeParsedRequest(payload, nil) {
		t.Fatal("parsed Wopal capsule was not rewritten")
	}
	input, _ := payload["input"].([]any)
	if len(input) == 0 || !strings.Contains(toString(input[0]), "PARSED-91") {
		t.Fatalf("parsed payload lost expanded summary: %#v", payload)
	}

	original := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"compaction","id":"cmp_official","encrypted_content":"official-openai-capsule-blob"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original))
	request.Header.Set("Content-Encoding", "gzip")
	RewriteOfficialRequestBody(request, nil)
	got, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) || request.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("legacy passthrough changed bytes/encoding: body=%s encoding=%q", got, request.Header.Get("Content-Encoding"))
	}
}

func TestRewriteOfficialRequestPreservesBodyWithoutWopalMarkers(t *testing.T) {
	ConfigureKey("unit-test-secret")
	original := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"compaction","id":"cmp_official","encrypted_content":"official-openai-capsule-blob"}]}`)
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Encoding", "gzip")
	RewriteOfficialRequestBody(request, nil)
	got, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("official request body was modified: %s", got)
	}
	if request.ContentLength != int64(len(original)) {
		t.Fatalf("content length = %d, want %d", request.ContentLength, len(original))
	}
	if request.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("content-encoding header was dropped")
	}
}

func TestRewriteOfficialRequestExpandsGzippedWopalCapsule(t *testing.T) {
	ConfigureKey("unit-test-secret")
	item := testWopalCompactionItem(t, "cmp_wopal_gzip-1", "remember SPARK-GB10")
	payload, err := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol",
		"input": []any{item, map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Encoding", "gzip")
	RewriteOfficialRequestBody(request, nil)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Content-Encoding") != "" {
		t.Fatal("content-encoding should be dropped after expansion")
	}
	if request.ContentLength != int64(len(body)) {
		t.Fatalf("content length = %d, body = %d", request.ContentLength, len(body))
	}
	expanded := string(body)
	if strings.Contains(expanded, "wgcp1.") {
		t.Fatalf("expanded body still contains Wopal capsule: %s", expanded)
	}
	if !strings.Contains(expanded, "remember SPARK-GB10") {
		t.Fatalf("expanded body missing expanded summary: %s", expanded)
	}
}

func TestRewriteOfficialRequestDropsStatelessReasoningItem(t *testing.T) {
	const staleID = "rs_0217926475864278eedaf36e20f5141b2bc84e877dea1f72da2f2_0"
	payload, err := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol",
		"store": false,
		"input": []any{
			map[string]any{"type": "reasoning", "id": staleID, "encrypted_content": "", "summary": []any{}},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsOfficialSanitizeMarker(payload) {
		t.Fatal("stopping-point reasoning item did not trigger sanitization")
	}
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	RewriteOfficialRequestBody(request, nil)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), staleID) {
		t.Fatalf("body still contains stale reasoning id: %s", body)
	}
	if !strings.Contains(string(body), `"role":"user"`) {
		t.Fatalf("user message was lost: %s", body)
	}
}

func TestRewriteOfficialRequestDropsWopalReasoningItem(t *testing.T) {
	ConfigureKey("unit-test-secret")
	payload, err := json.Marshal(map[string]any{
		"model": "codex-auto-review",
		"input": []any{testWopalReasoningItem(), map[string]any{"type": "message", "role": "user", "content": "review"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	RewriteOfficialRequestBody(request, nil)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "rs_chatcmpl") {
		t.Fatalf("body still contains Wopal reasoning id: %s", body)
	}
	if !strings.Contains(string(body), `"role":"user"`) {
		t.Fatalf("user message was lost: %s", body)
	}
}

func TestSanitizeOfficialInputCoversAllCapsuleCarriers(t *testing.T) {
	ConfigureKey("unit-test-secret")
	sealed, err := SealCapsule(Capsule{
		Version: 1,
		ID:      "cmp_wopal_carrier-1",
		Model:   "wopal-ai/deepseek-v4.1-flash-free",
		Summary: "remember CARRIER-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, itemType := range []string{"compaction", "compaction_summary", "context_compaction"} {
		input := []any{
			map[string]any{"type": itemType, "id": "cmp_wopal_carrier-1", "encrypted_content": sealed},
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		}
		expanded, neutralized := sanitizeRequestInput(input)
		if neutralized != 1 {
			t.Fatalf("%s: neutralized = %d, want 1", itemType, neutralized)
		}
		found := false
		for _, raw := range expanded {
			if strings.Contains(toString(raw), "CARRIER-9") {
				found = true
			}
			if strings.Contains(toString(raw), CapsulePrefix) {
				t.Fatalf("%s: capsule survived: %v", itemType, raw)
			}
		}
		if !found {
			t.Fatalf("%s: expanded summary missing", itemType)
		}
	}
}

func TestOfficialSanitizerDropsLocalPreviousResponseAfterWopalCompaction(t *testing.T) {
	ConfigureKey("previous-response-boundary-test")
	item := testWopalCompactionItem(t, "cmp-local-response", "remember PORTABLE-77")
	body, err := json.Marshal(map[string]any{
		"model":                "gpt-6.1-sol",
		"previous_response_id": "resp_wopal_compact_local_only",
		"input":                []any{item, map[string]any{"type": "message", "role": "user", "content": "continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	RewriteOfficialRequestBody(req, nil)
	rewritten, _ := io.ReadAll(req.Body)
	if bytes.Contains(rewritten, []byte("previous_response_id")) {
		t.Fatalf("HTTP sanitizer kept local previous_response_id: %s", rewritten)
	}
	if !bytes.Contains(rewritten, []byte("PORTABLE-77")) {
		t.Fatalf("HTTP sanitizer lost portable summary: %s", rewritten)
	}
	var rewrittenPayload map[string]any
	if err := json.Unmarshal(rewritten, &rewrittenPayload); err != nil {
		t.Fatal(err)
	}
	rewrittenInput, _ := rewrittenPayload["input"].([]any)
	if len(rewrittenInput) == 0 {
		t.Fatalf("HTTP sanitizer produced no input: %s", rewritten)
	}
	summaryMessage, _ := rewrittenInput[0].(map[string]any)
	if rawID, ok := summaryMessage["id"]; ok {
		summaryID := strings.TrimSpace(fmt.Sprint(rawID))
		if !strings.HasPrefix(summaryID, "msg_") || len(summaryID) > 64 {
			t.Fatalf("HTTP sanitizer produced invalid official message id %q", summaryID)
		}
	}

	frame := append([]byte(nil), body...)
	rewrittenFrame, ok := SanitizeWebsocketFrame(frame, nil)
	if !ok {
		t.Fatal("websocket sanitizer did not rewrite Wopal compaction")
	}
	if bytes.Contains(rewrittenFrame, []byte("previous_response_id")) {
		t.Fatalf("websocket sanitizer kept local previous_response_id: %s", rewrittenFrame)
	}
	if !bytes.Contains(rewrittenFrame, []byte("PORTABLE-77")) {
		t.Fatalf("websocket sanitizer lost portable summary: %s", rewrittenFrame)
	}
	var framePayload map[string]any
	if err := json.Unmarshal(rewrittenFrame, &framePayload); err != nil {
		t.Fatal(err)
	}
	frameInput, _ := framePayload["input"].([]any)
	frameSummary, _ := frameInput[0].(map[string]any)
	if rawID, ok := frameSummary["id"]; ok {
		frameSummaryID := strings.TrimSpace(fmt.Sprint(rawID))
		if !strings.HasPrefix(frameSummaryID, "msg_") || len(frameSummaryID) > 64 {
			t.Fatalf("websocket sanitizer produced invalid official message id %q", frameSummaryID)
		}
	}
}

func TestOfficialSanitizerDropsLocalCompactionResponseIDWithoutCapsule(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","previous_response_id":"resp_wopal_compact_local_only","input":[{"type":"message","role":"user","content":"continue"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	RewriteOfficialRequestBody(req, nil)
	rewritten, _ := io.ReadAll(req.Body)
	if bytes.Contains(rewritten, []byte("resp_wopal_compact_")) {
		t.Fatalf("HTTP sanitizer kept local compaction response id: %s", rewritten)
	}

	rewrittenFrame, ok := SanitizeWebsocketFrame(body, nil)
	if !ok {
		t.Fatal("websocket sanitizer did not remove local compaction response id")
	}
	if bytes.Contains(rewrittenFrame, []byte("resp_wopal_compact_")) {
		t.Fatalf("websocket sanitizer kept local compaction response id: %s", rewrittenFrame)
	}
}

func TestWopalCompactionSummaryIDsFitOfficialLimit(t *testing.T) {
	capsule := Capsule{
		Version: 1,
		ID:      "cmp_wopal_12345678-1234-1234-1234-123456789abc",
		Summary: "preserved context",
	}
	message := summaryMessage(capsule)
	id := strings.TrimSpace(fmt.Sprint(message["id"]))
	if len(id) == 0 || len(id) > 64 {
		t.Fatalf("summary id length = %d, want 1..64: %q", len(id), id)
	}
	if !strings.HasPrefix(id, "msg_") {
		t.Fatalf("summary id must use official message prefix: %q", id)
	}

	fallback := unavailableMessage(capsule.ID)
	fallbackID := strings.TrimSpace(fmt.Sprint(fallback["id"]))
	if len(fallbackID) == 0 || len(fallbackID) > 64 {
		t.Fatalf("fallback id length = %d, want 1..64: %q", len(fallbackID), fallbackID)
	}
	if fallbackID != id {
		t.Fatalf("summary id mismatch: normal=%q fallback=%q", id, fallbackID)
	}
}

func TestNormalizeOfficialInputStripsSelfDescribingItemIDs(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "id": "msg_01abc", "role": "user", "content": "hello"},
		map[string]any{"type": "function_call", "id": "fc_01abc", "call_id": "call_1", "name": "exec_command", "arguments": "{}"},
		map[string]any{"type": "reasoning", "id": "rs_official", "encrypted_content": "cipher", "summary": []any{}},
	}
	got, stripped, dropped, _ := normalizeInputItems(input)
	if stripped != 2 || dropped != 0 {
		t.Fatalf("stripped=%d dropped=%d, want 2/0", stripped, dropped)
	}
	if _, ok := got[0].(map[string]any)["id"]; ok {
		t.Fatal("message id was not stripped")
	}
	if _, ok := got[1].(map[string]any)["id"]; ok {
		t.Fatal("function_call id was not stripped")
	}
	if got[2].(map[string]any)["id"] != "rs_official" {
		t.Fatal("reasoning id must be preserved")
	}
}

func TestNormalizeOfficialInputDropsReferencesAndIDOnlyItems(t *testing.T) {
	input := []any{
		map[string]any{"type": "item_reference", "id": "msg_persisted"},
		map[string]any{"type": "mystery", "id": "mystery_1"},
		map[string]any{"type": "message", "role": "user", "content": "hi"},
	}
	got, stripped, dropped, _ := normalizeInputItems(input)
	if dropped != 2 || stripped != 0 {
		t.Fatalf("dropped=%d stripped=%d, want 2/0", dropped, stripped)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
}

func TestSanitizeOfficialRequestInputStillNeutralizesWopalReasoning(t *testing.T) {
	input := []any{
		map[string]any{"type": "reasoning", "id": "rs_chatcmpl-abc_0", "encrypted_content": ""},
		map[string]any{"type": "message", "id": "msg_1", "role": "user", "content": "hi"},
	}
	got, neutralized := sanitizeRequestInput(input)
	got, stripped, dropped, _ := normalizeInputItems(got)
	if neutralized != 1 || stripped != 1 || dropped != 0 {
		t.Fatalf("neutralized=%d stripped=%d dropped=%d", neutralized, stripped, dropped)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
}

func TestCmbReasoningSanitizedAcrossTransports(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			raw := []byte(`{"type":"response.create","model":"gpt-6-luna","input":[{"type":"reasoning","id":"rs_cmb-abc_0","encrypted_content":""},{"type":"reasoning","id":"rs_native","encrypted_content":"keep-me"},{"type":"message","role":"user","content":"hello"}]}`)
			if transport == "http" {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
				RewriteOfficialRequestBody(req, nil)
				raw, _ = io.ReadAll(req.Body)
			} else {
				if !containsOfficialSanitizeMarker(raw) {
					t.Fatal("cmb reasoning bypasses websocket sanitizer")
				}
				var ok bool
				raw, ok = SanitizeWebsocketFrame(raw, nil)
				if !ok {
					t.Fatal("frame not sanitized")
				}
			}
			if strings.Contains(string(raw), "rs_cmb-") || !strings.Contains(string(raw), "keep-me") {
				t.Fatalf("wrong reasoning sanitization: %s", raw)
			}
		})
	}
}
