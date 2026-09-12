package shiprocket

import "strings"

// splitName divides a single full name into the given name and the surname,
// the two fields Shiprocket asks for separately.
//
// MAK Watches stores one name per order, because that is what a customer
// types. Shiprocket wants it in halves, and validates that both keys are
// present. Splitting is therefore a wire-format concern of this adapter, not
// something the neutral shipping layer or the order model should know about.
//
// The last whitespace-separated token is taken as the surname and everything
// before it as the given name, so no part of the name is discarded:
//
//	"Shivam Mishra"        -> "Shivam",       "Mishra"
//	"Shivam Kumar Mishra"  -> "Shivam Kumar", "Mishra"
//	"  Shivam   Mishra  "  -> "Shivam",       "Mishra"
//	"Shivam"               -> "Shivam",       ""
//	""                     -> "",             ""
//
// A single-word name yields an empty surname rather than an invented one. See
// the note on fallbacks in CreateShipment: a fabricated surname would be
// printed on a real shipping label and read out to a real courier.
func splitName(full string) (first, last string) {
	// strings.Fields splits on any run of whitespace, which collapses the
	// double spaces and tabs that arrive from pasted input.
	parts := strings.Fields(full)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return strings.Join(parts[:len(parts)-1], " "), parts[len(parts)-1]
	}
}
