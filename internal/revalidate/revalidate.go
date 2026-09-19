// Package revalidate pushes cache invalidations to the storefront.
//
// # Why this exists
//
// Admin-managed presentation content is read by the Next.js storefront through
// `fetch(..., { next: { revalidate, tags } })`. That response lands in Next's
// Data Cache, and the rendered route lands in its Full Route Cache. Clearing
// the API's own Redis entry -- which the storefront and home-content handlers
// already do on every write -- therefore only fixes the *first* of three
// layers: the browser keeps being served the storefront's cached copy until
// its TTL lapses, which is exactly the "I saved it and the site still shows
// the old heading" report.
//
// Next.js exposes on-demand invalidation for precisely this, but only from
// inside its own process. So the API tells it: one authenticated POST naming
// the cache tags that just became stale, and the storefront purges those tags.
//
// Everything here is best effort and fails open. A storefront that cannot be
// reached must never fail, slow down or roll back an admin's save -- the write
// is already committed and the content will still refresh on its ordinary TTL.
// Unconfigured is a no-op, so an environment without a storefront URL behaves
// exactly as it did before.
package revalidate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Cache tags. These are the contract with the storefront: the same strings
// appear in its fetch options and in its /api/revalidate allow-list, so a tag
// added on one side is inert until it is added on the other.
const (
	// TagStorefront covers the admin-managed presentation document -- the
	// navigation, the footer, every section's copy, the product rails.
	TagStorefront = "storefront"
	// TagHomeContent covers the homepage CMS: hero slides, category cards,
	// collection features, the tech showcase and the gallery.
	TagHomeContent = "home-content"
)

// requestTimeout bounds one invalidation call. Short on purpose: this runs
// detached from the admin's request, and a storefront that is not answering
// within a few seconds is better abandoned than left holding a goroutine.
const requestTimeout = 6 * time.Second

// Notifier posts cache invalidations to the storefront's revalidation
// endpoint. The zero value, and a nil *Notifier, are safe no-ops.
type Notifier struct {
	endpoint string
	secret   string
	client   *http.Client
}

// New builds a notifier.
//
// Both the endpoint and the secret are required: an endpoint without a secret
// would mean posting to an unauthenticated purge route, which is a denial-of-
// service lever, so that combination is refused rather than half-configured.
// Either one empty yields a nil notifier, and every method on it is a no-op.
func New(endpoint, secret string) *Notifier {
	endpoint = strings.TrimSpace(endpoint)
	secret = strings.TrimSpace(secret)

	switch {
	case endpoint == "" && secret == "":
		log.Printf("[REVALIDATE] disabled: STOREFRONT_REVALIDATE_URL is not set. " +
			"Admin content changes will appear on the storefront when its own cache TTL lapses.")
		return nil
	case endpoint == "":
		log.Printf("[REVALIDATE] disabled: a secret is configured but STOREFRONT_REVALIDATE_URL is not.")
		return nil
	case secret == "":
		log.Printf("[REVALIDATE] disabled: STOREFRONT_REVALIDATE_URL is set but STOREFRONT_REVALIDATE_SECRET is not. " +
			"Refusing to call an unauthenticated purge endpoint.")
		return nil
	}

	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		log.Printf("[REVALIDATE] disabled: STOREFRONT_REVALIDATE_URL must be an absolute http(s) URL.")
		return nil
	}

	log.Printf("[REVALIDATE] enabled -> %s", redactURL(endpoint))
	return &Notifier{
		endpoint: endpoint,
		secret:   secret,
		client:   &http.Client{Timeout: requestTimeout},
	}
}

// Enabled reports whether invalidations will actually be sent.
func (n *Notifier) Enabled() bool { return n != nil && n.endpoint != "" }

// Invalidate purges the given cache tags on the storefront, in the background.
//
// It returns immediately. Callers are HTTP handlers answering an admin, and
// the save they are reporting on has already been committed -- making that
// response wait on a second service would trade a correctness problem for a
// latency one.
func (n *Notifier) Invalidate(tags ...string) {
	if !n.Enabled() || len(tags) == 0 {
		return
	}
	// A detached context: the caller's request context is cancelled the moment
	// its response is written, which would abort this call every time.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		if err := n.InvalidateContext(ctx, tags...); err != nil {
			log.Printf("[REVALIDATE] tags=%v failed: %v", tags, err)
		}
	}()
}

// InvalidateContext performs the call synchronously. Exported for tests and
// for callers that genuinely need to know the outcome.
func (n *Notifier) InvalidateContext(ctx context.Context, tags ...string) error {
	if !n.Enabled() || len(tags) == 0 {
		return nil
	}

	body, err := json.Marshal(map[string]any{"tags": tags})
	if err != nil {
		return fmt.Errorf("revalidate: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("revalidate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A shared secret in a header, never in the URL: a query string is logged
	// by proxies and CDNs by default and would leak the credential into
	// somebody else's access log.
	req.Header.Set("X-Revalidate-Secret", n.secret)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("revalidate: post: %w", err)
	}
	defer resp.Body.Close()

	// Drain a little of the body so the connection can be reused, and so a
	// failure can say what the storefront actually answered.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("revalidate: storefront answered %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	log.Printf("[REVALIDATE] tags=%v purged on the storefront", tags)
	return nil
}

// redactURL keeps any credential or token out of the startup log.
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i] + "?…"
	}
	return raw
}
