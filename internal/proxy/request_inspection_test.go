package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"wopal-codex-gateway/internal/clientfp"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

// canonical: a body that yields fully parseable JSON together with a terminal
// read error must never continue into normal inference. Cover both an official
// model routed through the reverse proxy and a Wopal model routed through the
// fake local SDK handler; neither may report successful inference.
func TestIndependentBodyReadFailureCannotBecomeSuccessfulInference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name  string
		model string
		head  http.Header
	}{
		{name: "official reverse proxy", model: "gpt-6.1-sol", head: http.Header{"ChatGPT-Account-ID": {"acct-review"}}},
		{name: "wopal fake SDK", model: "wopal-ai/test", head: http.Header{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gin.New()
			target := mustURL("http://127.0.0.1:1")
			engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, quota.NewGuard(nil)))
			inferenceRan := false
			engine.POST("/v1/responses", func(c *gin.Context) {
				raw, err := io.ReadAll(c.Request.Body)
				if err != nil || !bytes.Contains(raw, []byte(tc.model)) {
					c.Status(http.StatusBadRequest)
					return
				}
				inferenceRan = true
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Header = tc.head.Clone()
			req.Body = &completeJSONThenErrorBody{payload: `{"model":"` + tc.model + `","input":[]}`}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			if recorder.Code == http.StatusNoContent || inferenceRan {
				t.Fatalf("incomplete body reached successful inference: status=%d", recorder.Code)
			}
		})
	}
}

type completeJSONThenErrorBody struct {
	payload string
	sent    bool
}

func (b *completeJSONThenErrorBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	return copy(p, b.payload), io.ErrUnexpectedEOF
}

func (*completeJSONThenErrorBody) Close() error { return nil }

// detectRequestShape is a test-only adapter for tests that need routing fields.
func detectRequestShape(req *http.Request) (string, clientfp.Shape, error) {
	inspection, err := inspectRequest(req)
	if err != nil {
		return "", clientfp.Shape{}, err
	}
	return inspection.model, inspection.shape, nil
}

func TestHTTPPreparationPreservesJSONNumbers(t *testing.T) {
	for _, failover := range []bool{false, true} {
		t.Run(map[bool]string{false: "wopal", true: "quota failover"}[failover], func(t *testing.T) {
			guard := quota.NewGuard(nil)
			model := "wopal-ai/test"
			if failover {
				model = "gpt-6.1-sol"
				guard.ObserveRateLimitPayload("acct", []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":98,"window_minutes":300,"reset_after_seconds":3600}}}`), time.Now())
			}
			engine := ginTestEngine()
			target := mustURL("http://127.0.0.1:1")
			engine.Use(routingMiddlewareWithTargetsAndQuotaGuard(nil, target, target, guard))
			var got map[string]any
			engine.POST("/v1/responses", func(c *gin.Context) {
				decoder := json.NewDecoder(c.Request.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&got); err != nil {
					t.Error(err)
				}
				if c.GetHeader("Content-Encoding") != "" {
					t.Error("prepared request still compressed")
				}
				c.Status(http.StatusNoContent)
			})
			const numbers = `"large":9007199254740993,"decimal":1.234567890123456789,"exponent":1e400`
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write([]byte(`{"model":"` + model + `","input":[],"metadata":{` + numbers + `}}`))
			_ = writer.Close()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", &compressed)
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("ChatGPT-Account-ID", "acct")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			wantModel := model
			if failover {
				wantModel = routing.OfficialQuotaFailoverModel
			}
			if got["model"] != wantModel {
				t.Fatalf("model=%v want=%s", got["model"], wantModel)
			}
			want := map[string]any{"large": json.Number("9007199254740993"), "decimal": json.Number("1.234567890123456789"), "exponent": json.Number("1e400")}
			if !reflect.DeepEqual(got["metadata"], want) {
				t.Fatalf("numbers changed: %v", got["metadata"])
			}
		})
	}
}

func TestInspectionLeavesBodyAndHeadersForDownstream(t *testing.T) {
	for _, raw := range []string{
		` { "model":"gpt-6.1-sol", "input":[], "n":9007199254740993 } `,
		`{"model":"gpt-6.1-sol"} {"trailing":true}`,
		`{"model":`,
	} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			req.ContentLength = -1 // Inspection must preserve unknown/chunked length.
			req.Header.Set("Content-Type", "application/json")
			headers := req.Header.Clone()
			_, _, err := detectRequestShape(req)
			if json.Valid([]byte(raw)) != (err == nil) {
				t.Fatalf("parse error=%v for %s", err, raw)
			}
			got, err := io.ReadAll(req.Body)
			if err != nil || string(got) != raw || req.ContentLength != -1 || !reflect.DeepEqual(req.Header, headers) {
				t.Fatalf("inspection modified request: body=%q length=%d err=%v", got, req.ContentLength, err)
			}
		})
	}
}

type failingRequestBody struct {
	err error
}

func (b *failingRequestBody) Read(p []byte) (int, error) {
	return copy(p, `{"model":`), b.err
}

func (*failingRequestBody) Close() error { return nil }

// A fresh self-loop POST must not reuse the GET handshake's or a previous
// request's inspection. The local inspection is scoped to this exact body.
func TestInspectionDoesNotCrossRequestGeneration(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	first.Header.Set("X-Codex-Routing-Hint", "model=wopal-ai/from-handshake")
	firstInspection, err := inspectRequest(first)
	if err != nil || firstInspection.payload != nil {
		t.Fatalf("handshake inspection = %#v, %v", firstInspection, err)
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"wopal-ai/from-post","input":[]}`))
	secondInspection, err := inspectRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if secondInspection.model != "wopal-ai/from-post" || secondInspection.shape.Model != "wopal-ai/from-post" {
		t.Fatalf("new POST inherited handshake inspection: %#v", secondInspection)
	}

	// The real self-loop path derives a new POST from the upgraded request's
	// context. The inspection remains a local value, so the new body wins.
	loop := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"wopal-ai/generated-compactor","input":[]}`))
	loop = loop.WithContext(second.Context())
	loopInspection, err := inspectRequest(loop)
	if err != nil {
		t.Fatal(err)
	}
	if loopInspection.model != "wopal-ai/generated-compactor" {
		t.Fatalf("self-loop POST reused prior body inspection: %#v", loopInspection)
	}
}

func TestInspectionReadsBodyOnceAndPreservesUntouchedCompression(t *testing.T) {
	payload := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":"keep"}]}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := &countingReadCloser{Reader: bytes.NewReader(compressed.Bytes())}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.ContentLength = -1
	req.Header.Set("Content-Encoding", "gzip")
	inspection, err := inspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if body.payloadReads != 1 {
		t.Fatalf("body payload reads = %d, want 1", body.payloadReads)
	}
	if inspection.payload == nil || req.Header.Get("Content-Encoding") != "gzip" || req.ContentLength != -1 {
		t.Fatalf("inspection changed untouched compressed request: encoding=%q length=%d", req.Header.Get("Content-Encoding"), req.ContentLength)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(got, compressed.Bytes()) {
		t.Fatalf("downstream bytes changed: err=%v equal=%t", err, bytes.Equal(got, compressed.Bytes()))
	}
}

type countingReadCloser struct {
	*bytes.Reader
	payloadReads int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.payloadReads++
	}
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

func TestInspectionReadLimitPreservesPartialBodyAndFailure(t *testing.T) {
	wantErr := errors.New("limit read interrupted")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Body = &failingRequestBody{err: wantErr}
	if _, err := inspectRequestWithLimit(req, 1024); !errors.Is(err, wantErr) {
		t.Fatalf("inspection error=%v want=%v", err, wantErr)
	}
	got, err := io.ReadAll(req.Body)
	if string(got) != `{"model":` || !errors.Is(err, wantErr) {
		t.Fatalf("partial bytes/terminal error not restored: body=%q err=%v", got, err)
	}
}

func TestOversizeInspectionPreservesOriginalLength(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol","input":[]}`))
	if _, err := inspectRequestWithLimit(req, 4); err == nil {
		t.Fatal("oversize body was accepted")
	}
	want, _ := io.ReadAll(req.Body)
	if req.ContentLength != int64(len(want)) {
		t.Fatalf("oversize length=%d, restored=%d", req.ContentLength, len(want))
	}
}

func TestDecodedInspectionOversizePreservesCompressedBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 256)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), compressed.Bytes()...)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(want))
	req.Header.Set("Content-Encoding", "gzip")
	if _, err := inspectRequestWithLimit(req, 64); err == nil {
		t.Fatal("decoded oversize body was accepted")
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("compressed bytes changed after decoded oversize: err=%v equal=%t", err, bytes.Equal(got, want))
	}
}

func TestInspectionPreservesBodyReadFailure(t *testing.T) {
	wantErr := errors.New("interrupted body")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Body = &failingRequestBody{err: wantErr}
	if _, _, err := detectRequestShape(req); !errors.Is(err, wantErr) {
		t.Fatalf("inspection error=%v", err)
	}
	// The partial bytes remain readable after inspection; the original read
	// failure was already returned to the caller.
	got, err := io.ReadAll(req.Body)
	if string(got) != `{"model":` || !errors.Is(err, wantErr) {
		t.Fatalf("downstream lost partial bytes or terminal error: body=%q err=%v", got, err)
	}
}
