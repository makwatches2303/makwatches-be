package shiprocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// Token lifetime handling.
//
// Shiprocket documents the login JWT as valid for roughly 10 days. We prefer
// the `exp` claim carried in the token itself and fall back to a conservative
// default, then subtract a skew so a token is replaced before it can expire
// mid-request.
const (
	defaultTokenLifetime = 9 * 24 * time.Hour
	expirySkew           = 6 * time.Hour
	// authFailureCooldown throttles re-authentication after a rejected login.
	// Without it, a wrong password would drive a login attempt per request and
	// invite an account lockout.
	authFailureCooldown    = 30 * time.Second
	maxAuthFailureCooldown = 5 * time.Minute
)

// tokenManager owns the Shiprocket JWT for the process.
//
// The token is held in memory only. Persisting it would put a live credential
// in the database for no benefit: re-authenticating after a restart costs one
// request, and a leaked database backup should not contain a usable carrier
// session.
type tokenManager struct {
	email    string
	password string
	tr       *transport

	mu        sync.Mutex
	token     string
	expiresAt time.Time

	// inflight is the authentication currently running. Concurrent callers
	// wait on it instead of each firing their own login, which is what keeps
	// a burst of checkout requests from hammering /auth/login.
	inflight *authCall

	failures    int
	lastFailure time.Time

	now func() time.Time
}

// authCall is one shared in-flight authentication.
type authCall struct {
	done  chan struct{}
	token string
	err   error
}

func newTokenManager(email, password string, tr *transport) *tokenManager {
	return &tokenManager{
		email:    email,
		password: password,
		tr:       tr,
		now:      time.Now,
	}
}

// loginResponse is the documented shape of POST /auth/login.
type loginResponse struct {
	ID        int    `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	CompanyID int    `json:"company_id"`
	Token     string `json:"token"`
}

// Token returns a valid bearer token, authenticating only when needed.
func (m *tokenManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()

	if m.email == "" || m.password == "" {
		m.mu.Unlock()
		return "", shipping.Errf(shipping.CodeProviderAuthFailed,
			"shiprocket credentials are not configured")
	}

	if m.token != "" && m.now().Before(m.expiresAt) {
		tok := m.token
		m.mu.Unlock()
		return tok, nil
	}

	// Back off after a rejected login rather than retrying on every request.
	if m.failures > 0 {
		cooldown := time.Duration(m.failures) * authFailureCooldown
		if cooldown > maxAuthFailureCooldown {
			cooldown = maxAuthFailureCooldown
		}
		if m.now().Sub(m.lastFailure) < cooldown {
			m.mu.Unlock()
			return "", shipping.Errf(shipping.CodeProviderAuthFailed,
				"shiprocket authentication is in cooldown after %d consecutive failures", m.failures)
		}
	}

	if call := m.inflight; call != nil {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	call := &authCall{done: make(chan struct{})}
	m.inflight = call
	m.mu.Unlock()

	token, expiry, err := m.login(ctx)

	m.mu.Lock()
	m.inflight = nil
	if err != nil {
		m.failures++
		m.lastFailure = m.now()
		call.err = err
	} else {
		m.token = token
		m.expiresAt = expiry
		m.failures = 0
		call.token = token
	}
	m.mu.Unlock()

	close(call.done)
	return call.token, call.err
}

// Invalidate drops the cached token so the next call re-authenticates. Used
// when the carrier answers 401 with a token we believed was still valid.
func (m *tokenManager) Invalidate(stale string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Only clear when the stale token is the one we are holding, so a slow
	// request failing with an old token cannot discard a fresh one.
	if stale == "" || m.token == stale {
		m.token = ""
		m.expiresAt = time.Time{}
	}
}

// login performs POST /auth/login.
//
// Neither the password nor the returned token is ever logged or included in an
// error message; only the HTTP status reaches the operator detail.
func (m *tokenManager) login(ctx context.Context) (string, time.Time, error) {
	resp, err := m.tr.do(ctx, request{
		method: http.MethodPost,
		path:   "/auth/login",
		body: map[string]string{
			"email":    m.email,
			"password": m.password,
		},
		// Authentication is a read of sorts -- a duplicate login is harmless --
		// but the transport still caps it at maxAttempts, so this cannot loop.
		replayable: true,
	})
	if err != nil {
		return "", time.Time{}, shipping.Wrap(shipping.CodeProviderAuthFailed, err,
			"shiprocket login transport failure")
	}

	if resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
		return "", time.Time{}, shipping.Errf(shipping.CodeProviderAuthFailed,
			"shiprocket rejected the configured credentials (status=%d)", resp.status)
	}
	if resp.status < 200 || resp.status >= 300 {
		return "", time.Time{}, shipping.Errf(shipping.CodeProviderAuthFailed,
			"shiprocket login failed: %s", safeDetail(resp.status, resp.body))
	}

	var out loginResponse
	if err := decode(resp.body, &out); err != nil {
		return "", time.Time{}, shipping.Wrap(shipping.CodeProviderAuthFailed, err,
			"shiprocket login response was not readable (status=%d)", resp.status)
	}
	if out.Token == "" {
		return "", time.Time{}, shipping.Errf(shipping.CodeProviderAuthFailed,
			"shiprocket login returned no token (status=%d)", resp.status)
	}

	return out.Token, m.expiryFor(out.Token), nil
}

// expiryFor derives when a token should be replaced.
//
// The JWT's own exp claim is read without verifying the signature: we are not
// making a trust decision here, only scheduling a refresh, and the carrier is
// the one that signed it. A token without a usable claim gets the conservative
// default.
func (m *tokenManager) expiryFor(token string) time.Time {
	now := m.now()
	if exp, ok := jwtExpiry(token); ok {
		if refresh := exp.Add(-expirySkew); refresh.After(now) {
			return refresh
		}
		// Already inside the skew window; keep it briefly so one request can
		// still proceed rather than failing outright.
		if exp.After(now) {
			return exp
		}
	}
	return now.Add(defaultTokenLifetime - expirySkew)
}

// jwtExpiry extracts the exp claim from an unverified JWT payload.
func jwtExpiry(token string) (time.Time, bool) {
	parts := splitN(token, '.', 3)
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some issuers pad; tolerate the standard encoding too.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// splitN splits without pulling in strings for one hot-path helper.
func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
