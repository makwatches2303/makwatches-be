package mediaindex

import (
	"context"
	"testing"
	"time"
)

// freshIndex is an index holding a usable inventory that will not expire
// during the test, so Has answers from the cache and never needs a provider.
func freshIndex(objects ...string) *Index {
	names := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		names[object] = struct{}{}
	}
	return &Index{
		ttl:       time.Hour,
		names:     names,
		loadedAt:  time.Now(),
		available: true,
	}
}

// TestNoteMakesAFreshUploadVisible is the regression test for images
// disappearing from a product the moment it was saved.
//
// The inventory is listed once and cached for DefaultTTL. An image uploaded
// through the admin panel lands in the bucket and its URL is written to the
// product document, but the cached inventory predates it, so Has reported it
// absent and resolveProductImages dropped it from every response. The admin
// uploaded photos, saw them in the form, saved, and got back a product with
// no imagery -- for up to ten minutes, with nothing wrong in the database.
func TestNoteMakesAFreshUploadVisible(t *testing.T) {
	ctx := context.Background()
	index := freshIndex("1775623685403438254-old.jpg")

	const uploaded = "https://storage.googleapis.com/mak-watches.firebasestorage.app/1775623999999999999-new.jpg"

	if index.Has(ctx, ObjectName(uploaded)) {
		t.Fatal("precondition: the new object should be absent from a stale inventory")
	}

	index.Note(uploaded)

	if !index.Has(ctx, ObjectName(uploaded)) {
		t.Error("an object uploaded through this process must be servable at once")
	}
	if !index.Has(ctx, "1775623685403438254-old.jpg") {
		t.Error("Note must not disturb the existing inventory")
	}
}

func TestNoteAcceptsEveryReferenceShape(t *testing.T) {
	ctx := context.Background()

	refs := []string{
		"bare-object.jpg",
		"https://storage.googleapis.com/bucket/canonical.jpg",
		"https://firebasestorage.googleapis.com/v0/b/bucket/o/download.jpg?alt=media&token=abc",
		"/uploads/legacy.png",
	}
	want := []string{"bare-object.jpg", "canonical.jpg", "download.jpg", "legacy.png"}

	index := freshIndex()
	index.Note(refs...)

	for _, object := range want {
		if !index.Has(ctx, object) {
			t.Errorf("Note(%v): object %q not recorded", refs, object)
		}
	}
}

// An unavailable inventory already passes every reference through, so Note
// has nothing to record and must not resurrect a map that fail-open cleared.
func TestNoteIsANoOpWhenTheInventoryIsUnavailable(t *testing.T) {
	index := &Index{ttl: time.Hour, loadedAt: time.Now()}
	index.markUnavailable("test")

	index.Note("https://storage.googleapis.com/bucket/anything.jpg")

	if index.Available() {
		t.Error("Note must not mark an unavailable inventory available")
	}
	if index.Size() != 0 {
		t.Errorf("Note populated an unavailable inventory: size %d", index.Size())
	}
	if !index.Has(context.Background(), "anything.jpg") {
		t.Error("an unavailable inventory must still fail open")
	}
}

func TestNoteToleratesNilAndEmpty(t *testing.T) {
	var nilIndex *Index
	nilIndex.Note("https://storage.googleapis.com/bucket/x.jpg") // must not panic

	index := freshIndex()
	index.Note()
	index.Note("", "   ")
	if index.Size() != 0 {
		t.Errorf("empty references were recorded: size %d", index.Size())
	}
}
