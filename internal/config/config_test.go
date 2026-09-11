package config

import (
	"strings"
	"testing"
)

// TestRedactURIRemovesCredentials guards the startup log: it used to print
// MONGO_URI verbatim, putting the database password into the application log.
func TestRedactURIRemovesCredentials(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		// mustNotContain is the secret that has to disappear.
		mustNotContain string
		// mustContain is the diagnostic detail worth keeping.
		mustContain string
	}{
		{
			name:           "atlas srv with credentials",
			uri:            "mongodb+srv://appuser:s3cr3tPassw0rd@cluster0.example.mongodb.net/makwatches?retryWrites=true",
			mustNotContain: "s3cr3tPassw0rd",
			mustContain:    "cluster0.example.mongodb.net",
		},
		{
			name:           "standard mongodb with credentials",
			uri:            "mongodb://root:hunter2@db.internal:27017/makwatches",
			mustNotContain: "hunter2",
			mustContain:    "db.internal:27017",
		},
		{
			name:           "password containing an at sign",
			uri:            "mongodb://user:p%40ss@host:27017/db",
			mustNotContain: "p%40ss",
			mustContain:    "host:27017",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURI(tc.uri)
			if strings.Contains(got, tc.mustNotContain) {
				t.Fatalf("redacted URI still contains the password: %q", got)
			}
			if !strings.Contains(got, tc.mustContain) {
				t.Errorf("redacted URI lost the host: %q", got)
			}
			if !strings.Contains(got, "***") {
				t.Errorf("redacted URI does not mark the removal: %q", got)
			}
		})
	}
}

func TestRedactURILeavesCredentiallessURIAlone(t *testing.T) {
	for _, uri := range []string{
		"mongodb://localhost:27017",
		"mongodb://localhost:27017/makwatches",
		"",
	} {
		if got := RedactURI(uri); got != uri {
			t.Errorf("RedactURI(%q) = %q, want it unchanged", uri, got)
		}
	}
}

// TestRedactURIKeepsTheUsername: the username is useful when diagnosing an
// authentication failure and is not itself a secret.
func TestRedactURIKeepsTheUsername(t *testing.T) {
	got := RedactURI("mongodb+srv://appuser:pw@host/db")
	if !strings.Contains(got, "appuser") {
		t.Errorf("username was dropped: %q", got)
	}
}
