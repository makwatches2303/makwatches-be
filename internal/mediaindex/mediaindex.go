// Package mediaindex answers one question cheaply: does this object actually
// exist in the storage bucket?
//
// # Why this exists
//
// Product records reference image objects by name. A reference can outlive the
// object it points at -- the catalog was imported with image names that were
// never uploaded to this bucket. Serving those references anyway is not
// harmless: Google Cloud Storage answers an anonymous request for a missing
// object with 403 rather than 404 (it refuses to confirm whether an object
// exists, to prevent enumeration), so the URL looks like a permissions failure.
// Next.js then logs "upstream image response failed ... 403" for every such
// image on every render and burns an optimizer request on each one.
//
// Dropping unresolvable references at the API boundary means the storefront
// receives no URL at all and renders its placeholder directly, which is both
// the honest representation and the cheap one.
//
// Design
//
//   - The bucket inventory is listed once and cached, not checked per image.
//     One listing answers thousands of references.
//   - The shared firebase.Provider supplies the client, so no additional
//     Firebase initialization happens here.
//   - It FAILS OPEN. If the inventory cannot be built -- no credentials, a
//     network fault, a permissions change -- every reference is treated as
//     present and passes through untouched. A storage outage must never blank
//     the catalog's imagery.
package mediaindex

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/iterator"

	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
)

// DefaultTTL is how long an inventory is trusted before being rebuilt.
//
// Long enough that listing cost is negligible, short enough that an image
// uploaded through the admin panel becomes visible without a restart.
const DefaultTTL = 10 * time.Minute

// Index is a cached view of the object names present in the bucket.
//
// The zero value is not usable; construct with New. A nil *Index is valid and
// reports every object as present, so callers can hold one unconditionally.
type Index struct {
	provider *firebase.Provider
	ttl      time.Duration

	mu        sync.RWMutex
	names     map[string]struct{}
	loadedAt  time.Time
	available bool

	// refreshing serializes rebuilds so a burst of concurrent requests after
	// expiry triggers one listing rather than one per request.
	refreshing sync.Mutex
}

// New creates an index backed by the shared Firebase provider.
func New(provider *firebase.Provider, ttl time.Duration) *Index {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Index{provider: provider, ttl: ttl}
}

// Has reports whether an object is present in the bucket.
//
// Returns true when the inventory is unavailable, so an indexing failure never
// removes imagery. Also returns true for an empty name, which the caller's own
// emptiness checks are responsible for.
func (i *Index) Has(ctx context.Context, object string) bool {
	if i == nil || object == "" {
		return true
	}

	i.ensureFresh(ctx)

	i.mu.RLock()
	defer i.mu.RUnlock()

	if !i.available {
		return true
	}
	_, ok := i.names[object]
	return ok
}

// Note records objects this process has just written to the bucket, so they
// are servable immediately instead of waiting out the TTL.
//
// Without this, an image uploaded through the admin panel is invisible to
// every read until the inventory is next rebuilt: the object is in the
// bucket and its URL is in the product document, but Has reports it absent
// and resolveProductImages drops it. The admin uploads a photo, saves the
// product, and the product comes back with no imagery -- for up to DefaultTTL.
//
// Accepts references in any form ObjectName understands, so callers can pass
// the URL they just handed the client. A no-op when the inventory is
// unavailable, since that case already passes every reference through.
func (i *Index) Note(refs ...string) {
	if i == nil || len(refs) == 0 {
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.available || i.names == nil {
		return
	}
	for _, ref := range refs {
		if name := ObjectName(ref); name != "" {
			i.names[name] = struct{}{}
		}
	}
}

// Available reports whether a usable inventory has been built. Intended for
// diagnostics and logging, not for gating callers.
func (i *Index) Available() bool {
	if i == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.available
}

// Size returns the number of indexed objects, or 0 when unavailable.
func (i *Index) Size() int {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.names)
}

// ensureFresh rebuilds the inventory when it is missing or past its TTL.
func (i *Index) ensureFresh(ctx context.Context) {
	i.mu.RLock()
	fresh := i.loadedAt.After(time.Now().Add(-i.ttl))
	i.mu.RUnlock()
	if fresh {
		return
	}

	// Only one goroutine rebuilds; the rest proceed on the previous inventory
	// (or on fail-open) rather than blocking behind the listing.
	if !i.refreshing.TryLock() {
		return
	}
	defer i.refreshing.Unlock()

	// Re-check: another goroutine may have refreshed while we waited.
	i.mu.RLock()
	fresh = i.loadedAt.After(time.Now().Add(-i.ttl))
	i.mu.RUnlock()
	if fresh {
		return
	}

	i.rebuild(ctx)
}

func (i *Index) rebuild(ctx context.Context) {
	// Detached from the request context: a client disconnecting mid-listing
	// must not poison the cache for everyone else.
	listCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	client, err := i.provider.Client(listCtx)
	if err != nil {
		i.markUnavailable("firebase client unavailable: " + err.Error())
		return
	}

	names := make(map[string]struct{}, 512)
	it := client.StorageClient.Bucket(client.BucketName).Objects(listCtx, nil)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			i.markUnavailable("listing bucket: " + err.Error())
			return
		}
		names[attrs.Name] = struct{}{}
	}

	i.mu.Lock()
	i.names = names
	i.loadedAt = time.Now()
	first := !i.available
	i.available = true
	i.mu.Unlock()

	if first {
		log.Printf("[MEDIAINDEX] indexed %d objects in %s", len(names), client.BucketName)
	}
}

// markUnavailable records a failed rebuild and starts the TTL clock, so a
// persistent failure is retried periodically rather than on every request.
func (i *Index) markUnavailable(reason string) {
	i.mu.Lock()
	wasAvailable := i.available
	i.available = false
	i.names = nil
	i.loadedAt = time.Now()
	i.mu.Unlock()

	if wasAvailable || i.loadedAt.IsZero() {
		log.Printf("[MEDIAINDEX] unavailable, passing all references through: %s", reason)
	}
}

// ObjectName extracts the bucket object name from a stored image reference.
//
// Handles the forms the catalog actually contains: a bare object name, a
// canonical GCS URL, a Firebase download URL, and the legacy "/uploads/<name>"
// path. Query strings and fragments are stripped, and percent-encoding is left
// intact because bucket object names are compared as stored.
func ObjectName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	// Drop query and fragment (download tokens, cache busters).
	if idx := strings.IndexAny(ref, "?#"); idx >= 0 {
		ref = ref[:idx]
	}

	// Firebase download URL: /v0/b/<bucket>/o/<object>, object percent-encoded.
	if idx := strings.Index(ref, "/o/"); idx >= 0 && strings.Contains(ref, "/v0/b/") {
		return decodePath(ref[idx+len("/o/"):])
	}

	// Legacy local upload path.
	if idx := strings.Index(ref, "/uploads/"); idx >= 0 {
		return strings.TrimPrefix(ref[idx+len("/uploads/"):], "/")
	}

	// Canonical GCS URL, gs:// URI, or any other path: the object is whatever
	// follows the bucket segment. Falling back to the last path element covers
	// the flat namespace this bucket uses.
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}

	return ref
}

// decodePath reverses the percent-encoding Firebase applies to object names.
func decodePath(s string) string {
	// Only %2F (the path separator) is meaningful for these object names; the
	// bucket namespace is otherwise flat and unencoded.
	return strings.ReplaceAll(strings.ReplaceAll(s, "%2F", "/"), "%2f", "/")
}
