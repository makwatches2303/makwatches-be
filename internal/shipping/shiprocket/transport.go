package shiprocket

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Shiprocket external API root.
const DefaultBaseURL = "https://apiv2.shiprocket.in/v1/external"

// maxResponseBytes bounds every response we read. A carrier that answers with
// an unexpectedly large body -- an HTML error page from a proxy, for instance
// -- must not be able to exhaust our memory.
const maxResponseBytes = 4 << 20 // 4 MiB

// maxLabelBytes bounds a fetched label document, which is a PDF rather than
// JSON and so gets its own, larger ceiling.
const maxLabelBytes = 16 << 20 // 16 MiB

// Timeouts. The request timeout is per attempt, not per call, so a retried
// request cannot extend past the caller's context deadline.
const (
	dialTimeout    = 5 * time.Second
	requestTimeout = 30 * time.Second
	// labelTimeout is longer: Shiprocket generates the PDF on demand.
	labelTimeout = 60 * time.Second
)

// retry policy
const (
	maxAttempts   = 3
	baseBackoff   = 400 * time.Millisecond
	maxBackoff    = 5 * time.Second
	maxRetryAfter = 30 * time.Second
)

// transport performs HTTP against Shiprocket with bounded bodies, explicit
// timeouts and a retry policy that only ever replays requests it is safe to
// replay.
type transport struct {
	baseURL string
	http    *http.Client
	// sleep is injectable so tests exercise the backoff path without waiting.
	sleep func(ctx context.Context, d time.Duration) error
}

func newTransport(baseURL string) *transport {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &transport{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				// TLS verification is left at its secure default. This is
				// stated explicitly so nobody "fixes" a certificate problem by
				// turning it off: MinVersion is the only knob touched.
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 25 * time.Second,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// request describes one call.
type request struct {
	method string
	path   string
	// query is appended verbatim; callers must pre-encode.
	query string
	body  any
	// bearer is the JWT, empty for the login call itself.
	bearer string
	// replayable marks a request that may be retried after a transient
	// failure. Reads are replayable. A mutation is replayable only when a
	// duplicate is harmless or is caught by our own idempotency layer -- never
	// merely because it failed.
	replayable bool
	// maxBytes overrides the response ceiling, for label downloads.
	maxBytes int64
	// timeout overrides the per-attempt timeout.
	timeout time.Duration
}

// response is a completed HTTP exchange with the body already read.
type response struct {
	status int
	body   []byte
	header http.Header
}

// do executes a request, retrying transient failures with exponential backoff
// and honouring Retry-After when the carrier sends it.
func (t *transport) do(ctx context.Context, req request) (*response, error) {
	var payload []byte
	if req.body != nil {
		var err error
		payload, err = json.Marshal(req.body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
	}

	url := t.baseURL + req.path + req.query
	limit := req.maxBytes
	if limit <= 0 {
		limit = maxResponseBytes
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := t.attempt(ctx, req, url, payload, limit)
		if err == nil && !isTransientStatus(resp.status) {
			return resp, nil
		}
		if err != nil {
			lastErr = err
			// A transport error may have delivered the request anyway, so a
			// non-replayable mutation must not be sent twice.
			if !req.replayable {
				return nil, err
			}
		} else {
			lastErr = fmt.Errorf("shiprocket returned %d", resp.status)
			// A 429 or 5xx means the carrier did not accept the request, so
			// replaying a mutation is safe here in a way a timeout is not.
			// Non-replayable requests still return so the caller can decide.
			if !req.replayable && resp.status != http.StatusTooManyRequests {
				return resp, nil
			}
		}

		if attempt == maxAttempts {
			break
		}

		delay := backoffFor(attempt)
		if resp != nil {
			if ra, ok := retryAfter(resp.header); ok {
				delay = ra
			}
		}
		if err := t.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}

	if lastErr == nil {
		lastErr = errors.New("shiprocket request failed")
	}
	return nil, lastErr
}

// attempt performs exactly one HTTP exchange.
func (t *transport) attempt(ctx context.Context, req request, url string, payload []byte, limit int64) (*response, error) {
	timeout := req.timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}

	httpReq, err := http.NewRequestWithContext(attemptCtx, req.method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.bearer)
	}

	httpResp, err := t.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	// Bounded read. LimitReader caps what a misbehaving or compromised
	// upstream can make us allocate.
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return &response{status: httpResp.StatusCode, body: body, header: httpResp.Header}, nil
}

// getAbsolute downloads a document from a fully-qualified URL.
//
// Used for the label PDF, which Shiprocket hosts outside the API base. No
// bearer token is attached: the link is already capability-bearing, and
// forwarding our carrier credential to a third-party host would leak it. Only
// https is accepted so a redirected or spoofed http link cannot downgrade the
// fetch.
func (t *transport) getAbsolute(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("label url is not parseable: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("refusing to fetch a label over %q", parsed.Scheme)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, labelTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("label download returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", errors.New("label download was empty")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// isTransientStatus reports whether a status is worth retrying.
func isTransientStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoffFor returns the exponential delay for an attempt, capped.
func backoffFor(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt-1))) * baseBackoff
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// retryAfter reads a Retry-After header in either of its documented forms,
// clamped so a hostile or mistaken value cannot park a request indefinitely.
func retryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		d := time.Duration(secs) * time.Second
		return min(d, maxRetryAfter), true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d <= 0 {
			return 0, false
		}
		return min(d, maxRetryAfter), true
	}
	return 0, false
}

// decode unmarshals a JSON response body into dest.
func decode(body []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	// Shiprocket adds fields over time; refusing unknown ones would break the
	// integration on their next release. Numbers stay exact via json.Number
	// only where a type needs it, so no UseNumber here.
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("decoding shiprocket response: %w", err)
	}
	return nil
}

// safeDetail renders a provider response for an operator log.
//
// Response bodies are truncated and only ever reach server logs. Request
// bodies are never logged at all: they carry customer names, addresses and
// phone numbers, and the login body carries the account password.
func safeDetail(status int, body []byte) string {
	const cap = 512
	s := strings.TrimSpace(string(body))
	if len(s) > cap {
		s = s[:cap] + "...(truncated)"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	return fmt.Sprintf("status=%d body=%s", status, s)
}
