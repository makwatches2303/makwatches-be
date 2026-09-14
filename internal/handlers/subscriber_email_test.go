package handlers

import "testing"

// normalizeEmail is the only thing standing between a public, unauthenticated
// endpoint and whatever a client decides to post, so it is worth pinning down.
func TestNormalizeEmailAcceptsOrdinaryAddresses(t *testing.T) {
	cases := map[string]string{
		"a@b.com":                      "a@b.com",
		"  spaced@example.com  ":       "spaced@example.com",
		"MiXeD@Example.COM":            "mixed@example.com",
		"first.last+tag@sub.domain.in": "first.last+tag@sub.domain.in",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got, err := normalizeEmail(input)
			if err != nil {
				t.Fatalf("normalizeEmail(%q) returned error: %v", input, err)
			}
			if got != want {
				t.Errorf("normalizeEmail(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

// Lowercasing matters because the address is the upsert key: without it,
// "A@x.com" and "a@x.com" would become two subscribers.
func TestNormalizeEmailIsCaseInsensitiveKey(t *testing.T) {
	upper, err := normalizeEmail("Person@Example.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lower, err := normalizeEmail("person@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upper != lower {
		t.Errorf("same address normalized to %q and %q; they must key the same record", upper, lower)
	}
}

func TestNormalizeEmailRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"no at sign":       "not-an-email",
		"no domain":        "person@",
		"no local part":    "@example.com",
		"no dot in domain": "person@localhost",
		"spaces inside":    "per son@example.com",
		// A display-name form must not become a subscriber key.
		"display name": "Person <person@example.com>",
		"angle only":   "<person@example.com>",
		// Header injection via a newline in the address.
		"newline": "person@example.com\nBcc: victim@example.com",
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := normalizeEmail(input); err == nil {
				t.Errorf("normalizeEmail(%q) = %q, want an error", input, got)
			}
		})
	}
}
