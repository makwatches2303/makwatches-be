package models

import (
	"regexp"
	"strings"
)

var (
	slugNonAlnum   = regexp.MustCompile(`[^a-z0-9]+`)
	slugEdgeHyphen = regexp.MustCompile(`^-+|-+$`)
)

// Slugify converts a product name into a URL-safe slug.
//
// It is intentionally conservative: lowercase ASCII alphanumerics and single
// hyphens only. Non-ASCII characters are dropped rather than transliterated, so
// a name that slugifies to nothing returns "" and the caller must fall back to
// an id-based path instead of writing an empty slug.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = slugEdgeHyphen.ReplaceAllString(s, "")
	return s
}

// SlugWithSuffix appends a short disambiguating suffix to a base slug, used
// when two products share a name. An empty base yields just the suffix so the
// result is always a usable path segment.
func SlugWithSuffix(base, suffix string) string {
	base = Slugify(base)
	suffix = Slugify(suffix)
	switch {
	case base == "" && suffix == "":
		return ""
	case base == "":
		return suffix
	case suffix == "":
		return base
	default:
		return base + "-" + suffix
	}
}
