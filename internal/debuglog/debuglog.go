// Package debuglog gates high-volume, per-request debug logging behind an
// explicit on/off switch instead of it running unconditionally in every
// environment -- unconditional logging on every request (e.g. one line per
// successful authentication) inflates log volume/cost in production and
// roughly doubles the token cost of reading the busiest handlers for anyone,
// human or LLM, working in this codebase later.
//
// SetEnabled is called once at process startup (see cmd/api/main.go); before
// that, or if it is never called at all, Printf is a no-op. That default
// matters for the one-off cmd/ tools, which never call SetEnabled and have
// no need for this per-request noise regardless.
package debuglog

import (
	"fmt"
	"sync/atomic"
)

var enabled atomic.Bool

// SetEnabled turns debug logging on or off for the process.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// Printf writes like fmt.Printf, but only when debug logging is enabled.
func Printf(format string, args ...any) {
	if enabled.Load() {
		fmt.Printf(format, args...)
	}
}
