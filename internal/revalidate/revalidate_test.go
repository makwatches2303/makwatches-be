package revalidate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The storefront cache notifier.
//
// Everything here runs against a local httptest server. Nothing reaches a real
// storefront, and the notifier is required to fail open in every direction: a
// missing configuration, a refusing endpoint and an unreachable host must all
// leave the caller's write untouched.

// recorder is a stand-in storefront that captures what it was sent.
type recorder struct {
	mu     sync.Mutex
	calls  int
	secret string
	tags   []string
	status int
}

func (r *recorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++
	r.secret = req.Header.Get("X-Revalidate-Secret")

	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(body, &payload)
	r.tags = payload.Tags

	if r.status != 0 {
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(`{"revalidated":false}`))
		return
	}
	_, _ = w.Write([]byte(`{"revalidated":true}`))
}

func (r *recorder) snapshot() (int, string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.secret, append([]string(nil), r.tags...)
}

func TestInvalidateSendsTheTagsAndTheSecret(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()

	n := New(server.URL, "s3cr3t")
	if n == nil {
		t.Fatal("New returned nil for a fully configured notifier")
	}

	if err := n.InvalidateContext(context.Background(), TagStorefront, TagHomeContent); err != nil {
		t.Fatalf("InvalidateContext: %v", err)
	}

	calls, secret, tags := rec.snapshot()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if secret != "s3cr3t" {
		t.Fatalf("secret header = %q, want the configured secret", secret)
	}
	if len(tags) != 2 || tags[0] != TagStorefront || tags[1] != TagHomeContent {
		t.Fatalf("tags = %v, want [%s %s]", tags, TagStorefront, TagHomeContent)
	}
}

// A URL with no secret would mean posting to an unauthenticated purge
// endpoint, which is a denial-of-service lever. Half-configured is refused
// rather than run.
func TestNewRefusesAHalfConfiguration(t *testing.T) {
	cases := map[string][2]string{
		"nothing configured":  {"", ""},
		"url without secret":  {"https://example.test/api/revalidate", ""},
		"secret without url":  {"", "s3cr3t"},
		"not an absolute url": {"example.test/api/revalidate", "s3cr3t"},
	}
	for name, pair := range cases {
		if n := New(pair[0], pair[1]); n != nil {
			t.Errorf("%s: New returned a live notifier", name)
		}
	}
}

// A nil notifier is the unconfigured case, and every method on it has to be a
// no-op rather than a panic -- the handlers call it unconditionally.
func TestNilNotifierIsInert(t *testing.T) {
	var n *Notifier

	if n.Enabled() {
		t.Error("Enabled() = true on a nil notifier")
	}
	n.Invalidate(TagStorefront) // must not panic
	if err := n.InvalidateContext(context.Background(), TagStorefront); err != nil {
		t.Errorf("InvalidateContext on a nil notifier = %v, want nil", err)
	}
}

func TestInvalidateWithNoTagsMakesNoCall(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()

	n := New(server.URL, "s3cr3t")
	if err := n.InvalidateContext(context.Background()); err != nil {
		t.Fatalf("InvalidateContext: %v", err)
	}
	if calls, _, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("calls = %d, want 0 for an empty tag list", calls)
	}
}

// A storefront that refuses must be reported, not swallowed silently -- but it
// is reported as an error to the caller's log, never as a failure of the write
// that triggered it. Invalidate (the fire-and-forget form) is what the handlers
// use, and it returns nothing.
func TestInvalidateReportsARefusingStorefront(t *testing.T) {
	rec := &recorder{status: http.StatusUnauthorized}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()

	n := New(server.URL, "wrong-secret")
	err := n.InvalidateContext(context.Background(), TagStorefront)
	if err == nil {
		t.Fatal("a 401 from the storefront was reported as success")
	}
}

// An unreachable storefront must not hang or panic. The closed server's port
// refuses immediately.
func TestInvalidateHandlesAnUnreachableStorefront(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	n := New(url, "s3cr3t")
	if err := n.InvalidateContext(context.Background(), TagStorefront); err == nil {
		t.Fatal("an unreachable storefront was reported as success")
	}
}
