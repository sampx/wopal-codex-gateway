package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/quota"
	"wopal-codex-gateway/internal/routing"
)

const (
	// quotaProbeCooldown bounds how often a protected instance asks the official
	// backend for fresh quota numbers. Reset grants arrive out of band, so the
	// only way to see one is to let a single minimal native request through.
	quotaProbeCooldown = 5 * time.Minute
	quotaProbeTimeout  = 10 * time.Second
	// quotaProbeModel is a quota-exempt recovery model: a probe must never eat
	// into a reserve it is trying to verify.
	quotaProbeModel = "gpt-6-luna"
)

// officialQuotaProbe refreshes quota telemetry while native traffic is failed
// over. It runs out of band: the request that triggered it still follows the
// current protection decision, and the next decision uses the fresh numbers.
type officialQuotaProbe struct {
	mu          sync.Mutex
	inFlight    bool
	nextAllowed time.Time
	client      *http.Client
}

func newOfficialQuotaProbe() *officialQuotaProbe {
	return &officialQuotaProbe{client: &http.Client{Timeout: quotaProbeTimeout}}
}

// schedule starts one probe when protection is blocking native traffic and the
// guard has not seen an official response for a while. It never blocks the
// request path; a failed probe only delays the next attempt.
func (p *officialQuotaProbe) schedule(request *http.Request, target *url.URL, guard *quota.Guard, accountID string, logger *activity.Logger) bool {
	if p == nil || request == nil || target == nil || guard == nil || logger == nil {
		return false
	}
	if strings.TrimSpace(accountID) == "" {
		return false
	}
	// Copy what the probe needs before the handler continues: the request is
	// rewritten by the failover path, and the goroutine must not race with it.
	headers := request.Header.Clone()
	headers.Del("Content-Encoding")
	headers.Del("Content-Length")
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("Accept-Encoding", "identity")

	now := time.Now()
	p.mu.Lock()
	if p.inFlight || now.Before(p.nextAllowed) {
		p.mu.Unlock()
		return false
	}
	p.inFlight = true
	p.nextAllowed = now.Add(quotaProbeCooldown)
	p.mu.Unlock()

	go func() {
		status, err := probeOfficialQuota(context.Background(), p.client, target, headers, guard, accountID)
		p.mu.Lock()
		p.inFlight = false
		p.mu.Unlock()
		fields := map[string]any{"event": "official_quota_probe", "status": status}
		if err != nil {
			fields["error"] = err.Error()
		}
		logger.Write(fields)
	}()
	return true
}

// probeOfficialQuota sends the smallest native request that still carries quota
// headers and feeds the response back into the guard. The credential is the one
// the current client request already carried; it is used once and never stored.
func probeOfficialQuota(ctx context.Context, client *http.Client, target *url.URL, headers http.Header, guard *quota.Guard, accountID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
	defer cancel()

	probeURL := *target
	probeURL.Path = routing.JoinURLPath(target.Path, "/responses")
	body := []byte(`{"model":"` + quotaProbeModel + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"quota probe"}]}],"max_output_tokens":16,"stream":false}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, probeURL.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header = headers
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 64<<10)
	guard.ObserveHeaders(accountID, response.Header, time.Now())
	return response.StatusCode, nil
}
