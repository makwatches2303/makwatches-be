package mediaindex

import (
	"context"
	"testing"
	"time"
)

// TestObjectName covers every reference shape the catalog actually stores, so
// the existence check compares the same string the bucket is keyed by.
func TestObjectName(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "bare object name",
			ref:  "1775623685403438254-tita_zoop.jpg",
			want: "1775623685403438254-tita_zoop.jpg",
		},
		{
			name: "canonical GCS url",
			ref:  "https://storage.googleapis.com/mak-watches.firebasestorage.app/1775623685403438254-tita_zoop.jpg",
			want: "1775623685403438254-tita_zoop.jpg",
		},
		{
			name: "firebase download url with token",
			ref:  "https://firebasestorage.googleapis.com/v0/b/mak-watches.firebasestorage.app/o/photo.jpg?alt=media&token=abc-123",
			want: "photo.jpg",
		},
		{
			name: "firebase download url with encoded path",
			ref:  "https://firebasestorage.googleapis.com/v0/b/bucket/o/products%2Fphoto.jpg?alt=media",
			want: "products/photo.jpg",
		},
		{
			name: "legacy uploads path, relative",
			ref:  "/uploads/1755004208409218400-hero.png",
			want: "1755004208409218400-hero.png",
		},
		{
			name: "legacy uploads path, absolute against old host",
			ref:  "https://api.makwatches.in/uploads/1755004208409218400-hero.png",
			want: "1755004208409218400-hero.png",
		},
		{
			name: "gs uri",
			ref:  "gs://mak-watches.firebasestorage.app/photo.jpg",
			want: "photo.jpg",
		},
		{
			name: "query string stripped",
			ref:  "https://storage.googleapis.com/bucket/photo.jpg?v=2",
			want: "photo.jpg",
		},
		{
			name: "fragment stripped",
			ref:  "https://storage.googleapis.com/bucket/photo.jpg#top",
			want: "photo.jpg",
		},
		{
			name: "whitespace trimmed",
			ref:  "  photo.jpg  ",
			want: "photo.jpg",
		},
		{
			name: "empty",
			ref:  "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ObjectName(tc.ref); got != tc.want {
				t.Errorf("ObjectName(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// TestNilIndexFailsOpen pins the most important safety property: a caller that
// holds no index must still see every reference as present, so the absence of
// an index can never blank the catalog's imagery.
func TestNilIndexFailsOpen(t *testing.T) {
	var index *Index

	if !index.Has(context.Background(), "anything.jpg") {
		t.Error("nil index reported an object as missing; it must fail open")
	}
	if index.Available() {
		t.Error("nil index reported itself available")
	}
	if index.Size() != 0 {
		t.Error("nil index reported a non-zero size")
	}
}

// TestUnavailableIndexFailsOpen covers the runtime failure path: credentials
// missing, listing denied, network down. Every reference must pass through.
func TestUnavailableIndexFailsOpen(t *testing.T) {
	// A provider with empty credentials fails to build a client, which is the
	// same path a permissions or network failure takes.
	index := New(nil, DefaultTTL)
	index.markUnavailable("test")

	if !index.Has(context.Background(), "anything.jpg") {
		t.Error("unavailable index reported an object as missing; it must fail open")
	}
	if index.Available() {
		t.Error("index marked unavailable still reports available")
	}
}

// TestEmptyObjectIsPresent guards the caller contract: emptiness is the
// caller's concern, and the index must not claim an empty name is missing.
func TestEmptyObjectIsPresent(t *testing.T) {
	index := New(nil, DefaultTTL)
	if !index.Has(context.Background(), "") {
		t.Error("empty object name reported as missing")
	}
}

// TestPopulatedIndexFiltersCorrectly exercises the real decision once an
// inventory exists.
func TestPopulatedIndexFiltersCorrectly(t *testing.T) {
	index := New(nil, DefaultTTL)

	// Simulate a completed rebuild.
	index.mu.Lock()
	index.names = map[string]struct{}{"present.jpg": {}}
	index.available = true
	index.loadedAt = time.Now()
	index.mu.Unlock()

	ctx := context.Background()
	if !index.Has(ctx, "present.jpg") {
		t.Error("indexed object reported missing")
	}
	if index.Has(ctx, "absent.jpg") {
		t.Error("unindexed object reported present; the 403-producing reference must be dropped")
	}
	if index.Size() != 1 {
		t.Errorf("Size() = %d, want 1", index.Size())
	}
}
