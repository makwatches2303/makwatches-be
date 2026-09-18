package mediaindex

import (
	"context"
	"errors"
	"sync"
	"time"

	"cloud.google.com/go/storage"
)

// probeTTL is how long a probe's answer is trusted. Short for a miss -- the
// worker may be storing that very object right now -- and effectively
// permanent for a hit, since objects are never renamed.
const (
	probeMissTTL = 20 * time.Second
	probeTimeout = 3 * time.Second
)

type probeResult struct {
	present bool
	at      time.Time
}

// probes caches per-object answers from the bucket for names the listing does
// not know.
//
// The listing is rebuilt every DefaultTTL, so for up to ten minutes an object
// that has just been written by another process -- the worker copying an
// import, a second API instance's upload -- is absent from it and the read
// path would drop the image. Before this the fix was Note, which only helps
// the process that did the writing; it does nothing for the others, and with
// storage moving to the worker the API never writes the object at all.
//
// So a miss is no longer final. The bucket is asked once, the answer is
// remembered, and a genuinely missing object still costs one call rather than
// one per request.
var probes = struct {
	sync.Mutex
	m map[string]probeResult
}{m: map[string]probeResult{}}

// probe asks the bucket directly whether an object exists.
//
// Bounded by a short timeout: this runs inside a product read, and a slow
// answer from storage must not turn into a slow catalogue. On timeout or
// error the object is reported present -- the same fail-open choice the
// listing itself makes, since a broken image is better than a vanished one.
func (i *Index) probe(ctx context.Context, object string) bool {
	// No provider means no bucket to ask. Absent from the listing is then the
	// only answer there is -- the contract Has had before probing existed, and
	// the one an index built without storage (tests, a misconfigured
	// deployment) still has to honour.
	if i.provider == nil {
		return false
	}

	probes.Lock()
	if cached, ok := probes.m[object]; ok {
		if cached.present || time.Since(cached.at) < probeMissTTL {
			probes.Unlock()
			return cached.present
		}
	}
	probes.Unlock()

	client, err := i.provider.Client(ctx)
	if err != nil {
		return true
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, err = client.StorageClient.Bucket(client.BucketName).Object(object).Attrs(probeCtx)
	present := true
	if errors.Is(err, storage.ErrObjectNotExist) {
		present = false
	} else if err != nil {
		// Unknown -- report present rather than hide an image over a
		// transient error, and do not cache a guess.
		return true
	}

	probes.Lock()
	probes.m[object] = probeResult{present: present, at: time.Now()}
	// The cache is per process and per object, never large, but it should
	// not grow without bound across a long-lived instance either.
	if len(probes.m) > 5000 {
		probes.m = map[string]probeResult{object: probes.m[object]}
	}
	probes.Unlock()

	if present {
		i.Note(object)
	}
	return present
}
