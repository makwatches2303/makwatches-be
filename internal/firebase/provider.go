package firebase

import (
	"context"
	"sync"
)

// Provider hands out a single shared FirebaseClient.
//
// The upload path used to call config.LoadConfig and NewFirebaseClient on every
// request, which re-read the environment, built a fresh GCS client and made a
// bucket-attrs round trip per upload. Construction is now done once, on first
// use, and the result is reused for the process lifetime.
//
// Initialization stays lazy rather than happening at boot: the API must still
// start when Firebase credentials are absent (local development without media
// uploads), and fail loudly only when an upload is actually attempted.
type Provider struct {
	credentialsJSON string
	bucketName      string

	once   sync.Once
	client *FirebaseClient
	err    error
}

// NewProvider returns a Provider bound to the given credentials and bucket. It
// performs no I/O; the client is built on the first Client call.
func NewProvider(credentialsJSON, bucketName string) *Provider {
	return &Provider{
		credentialsJSON: credentialsJSON,
		bucketName:      bucketName,
	}
}

// Client returns the shared client, constructing it on first call.
//
// A failed initialization is cached alongside the client, so a misconfigured
// deployment returns the same error cheaply instead of retrying a doomed
// network call on every request.
func (p *Provider) Client(ctx context.Context) (*FirebaseClient, error) {
	p.once.Do(func() {
		p.client, p.err = NewFirebaseClient(ctx, p.credentialsJSON, p.bucketName)
	})
	return p.client, p.err
}

// BucketName reports the configured bucket without forcing initialization.
// Callers that only need to build a public URL should use this rather than
// reaching through Client.
func (p *Provider) BucketName() string {
	return p.bucketName
}
