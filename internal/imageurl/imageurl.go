// Package imageurl resolves stored product image references to canonical
// Firebase Storage URLs.
//
// Historically the upload handlers persisted absolute URLs built from the
// incoming request host (c.BaseURL() + "/uploads/<object>"). That coupled image
// URLs to whichever domain served the API at upload time, so records written on
// the production host are still pointing at https://api.makwatches.in/uploads/...
// even when the catalog is served from somewhere else.
//
// Images themselves live in Firebase Storage, keyed by the same
// "<unixnano>-<filename>" object name that the legacy path already carries, so
// the legacy reference can be rewritten to the canonical bucket URL without any
// per-file mapping.
package imageurl

import "strings"

const gcsHost = "https://storage.googleapis.com"

// Resolve maps a stored image reference to a canonical Firebase Storage URL.
//
// Already-canonical Firebase/GCS URLs and unrelated absolute URLs are returned
// unchanged; legacy "/uploads/<object>" references (relative, or absolute
// against any host) are rewritten onto the configured bucket.
func Resolve(ref, bucket string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || bucket == "" {
		return ref
	}

	// Canonical Firebase Storage URLs pass through untouched.
	if strings.HasPrefix(ref, gcsHost+"/") ||
		strings.HasPrefix(ref, "https://firebasestorage.googleapis.com/") {
		return ref
	}

	// Legacy "/uploads/<object>" reference, relative or against any host.
	if i := strings.Index(ref, "/uploads/"); i >= 0 {
		if object := strings.TrimPrefix(ref[i+len("/uploads/"):], "/"); object != "" {
			return gcsHost + "/" + bucket + "/" + object
		}
	}

	// Any other absolute URL (CDN, external asset) is left as-is.
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}

	// A bare object name is assumed to live in the bucket root.
	if !strings.HasPrefix(ref, "/") {
		return gcsHost + "/" + bucket + "/" + ref
	}

	return ref
}

// ResolveAll applies Resolve to every reference in a slice, dropping entries
// that resolve to an empty string. It always returns a non-nil slice so the
// JSON response carries [] rather than null.
func ResolveAll(refs []string, bucket string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if resolved := Resolve(r, bucket); resolved != "" {
			out = append(out, resolved)
		}
	}
	return out
}
