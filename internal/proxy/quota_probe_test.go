package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/quota"
)

func weeklyProtectedGuard(t *testing.T, accountID string, observedAt time.Time) *quota.Guard {
	t.Helper()
	guard := quota.NewGuard(nil)
	guard.ObserveRateLimitPayload(accountID, []byte(`{"type":"codex.rate_limits","rate_limits":{"secondary":{"used_percent":99.5,"window_minutes":10080,"reset_after_seconds":3600}}}`), observedAt)
	return guard
}

func quotaHeaderServer(t *testing.T, requests *int, capture func(*http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		body, _ := io.ReadAll(r.Body)
		payload := map[string]any{}
		_ = json.Unmarshal(body, &payload)
		if capture != nil {
			capture(r, payload)
		}
		w.Header().Set("X-Codex-Primary-Used-Percent", "0")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "0")
		w.Header().Set("X-Codex-Secondary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Secondary-Reset-After-Seconds", "3600")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
}

func TestOfficialQuotaProbeLearnsResetGrant(t *testing.T) {
	now := time.Now()
	guard := weeklyProtectedGuard(t, "acct-probe", now.Add(-20*time.Minute))
	if blocked, _ := guard.Blocked("acct-probe", now); !blocked {
		t.Fatal("guard should be protecting before the probe")
	}
	if !guard.TelemetryStale(now) {
		t.Fatal("guard should report stale telemetry before the probe")
	}

	requests := 0
	var gotAuth, gotModel string
	server := quotaHeaderServer(t, &requests, func(r *http.Request, payload map[string]any) {
		gotAuth = r.Header.Get("Authorization")
		gotModel, _ = payload["model"].(string)
	})
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer client-token")
	status, err := probeOfficialQuota(context.Background(), server.Client(), target, headers, guard, "acct-probe")
	if err != nil {
		t.Fatalf("probeOfficialQuota() error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("probe status = %d", status)
	}
	if gotAuth != "Bearer client-token" {
		t.Fatalf("probe authorization = %q, want the client credential", gotAuth)
	}
	if gotModel != quotaProbeModel {
		t.Fatalf("probe model = %q, want %q", gotModel, quotaProbeModel)
	}
	if blocked, _ := guard.Blocked("acct-probe", time.Now()); blocked {
		t.Fatal("protection stayed on after the probe reported a reset")
	}
}

func TestOfficialQuotaProbeRespectsCooldown(t *testing.T) {
	now := time.Now()
	guard := weeklyProtectedGuard(t, "acct-probe-cooldown", now.Add(-20*time.Minute))
	logger, err := activity.New(t.TempDir(), config.Logs{MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close()

	requests := 0
	done := make(chan struct{}, 4)
	server := quotaHeaderServer(t, &requests, func(*http.Request, map[string]any) { done <- struct{}{} })
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	probe := newOfficialQuotaProbe()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer client-token")
	if !probe.schedule(request, target, guard, "acct-probe-cooldown", logger) {
		t.Fatal("first probe was not scheduled")
	}
	<-done
	if probe.schedule(request, target, guard, "acct-probe-cooldown", logger) {
		t.Fatal("second probe ignored the cooldown")
	}
}
