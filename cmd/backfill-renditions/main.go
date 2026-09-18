// Command backfill-renditions gives the images already in the bucket the
// smaller sizes that new uploads get automatically.
//
// Renditions are what let a listing page draw a 200px square without
// downloading a 1500px photograph, which matters now that image optimization
// at the edge is switched off (the Vercel quota is exhausted; see the
// storefront's next.config.ts). Uploads and imports have produced renditions
// since internal/imageproc landed, but the catalogue that existed before then
// -- several thousand images -- has none, so every grid still ships originals.
//
// Safe to re-run: an image whose rendition already exists in the bucket is
// skipped, so a run that is interrupted picks up where it left off, and
// running it twice costs one listing and nothing else. Nothing in Mongo is
// touched at all -- renditions are found by name on read, so the backfill only
// ever adds objects to the bucket.
//
// Usage:
//
//	go run ./cmd/backfill-renditions -dry-run          # count what is missing
//	go run ./cmd/backfill-renditions                   # do it
//	go run ./cmd/backfill-renditions -limit 200        # a first slice
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/api/iterator"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageproc"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageurl"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report what is missing without writing anything")
	cleanOrphans := flag.Bool("clean-orphans", false, "delete renditions stored under a generated name instead of the derived one (a first run of this tool wrote some), then exit")
	limit := flag.Int("limit", 0, "stop after this many images (0 means all of them)")
	workers := flag.Int("workers", 6, "how many images to process at once")
	flag.Parse()

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	mongoClient, mongoDB, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatalf("connecting to MongoDB: %v", err)
	}
	defer mongoClient.Disconnect(ctx)

	provider := firebase.NewProvider(cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
	client, err := provider.Client(ctx)
	if err != nil {
		log.Fatalf("connecting to Firebase Storage: %v", err)
	}

	if *cleanOrphans {
		cleanOrphanRenditions(ctx, client, cfg.FirebaseBucketName, *dryRun)
		return
	}

	// The same inventory the API uses, so "does this rendition already exist"
	// is answered from one bucket listing rather than an object-exists call
	// per image.
	index := mediaindex.New(provider, mediaindex.DefaultTTL)

	cursor, err := mongoDB.Collection("products").Find(ctx, bson.M{}, nil)
	if err != nil {
		log.Fatalf("reading products: %v", err)
	}
	defer cursor.Close(ctx)

	// Collected first so the work can be counted before any of it is done, and
	// so one image shared by several products is handled once.
	type work struct{ object, url string }
	var queue []work
	seen := map[string]bool{}

	for cursor.Next(ctx) {
		var doc struct {
			ImageURL string   `bson:"image_url"`
			Images   []string `bson:"images"`
		}
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		for _, ref := range append([]string{doc.ImageURL}, doc.Images...) {
			resolved := imageurl.Resolve(ref, cfg.FirebaseBucketName)
			if resolved == "" {
				continue
			}
			object := mediaindex.ObjectName(resolved)
			if object == "" || seen[object] {
				continue
			}
			seen[object] = true

			// Renditions are themselves objects in the bucket; a run that
			// treated them as sources would make renditions of renditions.
			if isRendition(object) {
				continue
			}
			thumb := imageproc.RenditionName(object, fmt.Sprintf("-%dw", imageproc.ThumbWidth), "image/jpeg")
			if index.Has(ctx, thumb) {
				continue
			}
			queue = append(queue, work{object: object, url: resolved})
		}
	}

	if *limit > 0 && len(queue) > *limit {
		queue = queue[:*limit]
	}

	log.Printf("%d image(s) without renditions", len(queue))
	if *dryRun || len(queue) == 0 {
		return
	}

	var done, failed, skipped atomic.Int64
	var wg sync.WaitGroup
	tokens := make(chan struct{}, *workers)
	httpClient := &http.Client{Timeout: 60 * time.Second}

	for _, item := range queue {
		wg.Add(1)
		go func(item work) {
			defer wg.Done()
			tokens <- struct{}{}
			defer func() { <-tokens }()

			body, err := download(ctx, httpClient, item.url)
			if err != nil {
				log.Printf("  %s: %v", item.object, err)
				failed.Add(1)
				return
			}

			stored := imageproc.Store(ctx, client, body, item.object)
			if len(stored) == 0 {
				// Already small enough, or already compressed harder than we
				// would re-encode it. Neither is a failure.
				skipped.Add(1)
				return
			}

			index.Note(mapValues(stored)...)
			if n := done.Add(1); n%50 == 0 {
				log.Printf("  %d/%d done", n, len(queue))
			}
		}(item)
	}
	wg.Wait()

	log.Printf("finished: %d resized, %d already fine, %d failed", done.Load(), skipped.Load(), failed.Load())
}

// orphanRendition matches a rendition stored under a generated name rather
// than the derived one: two timestamps, because an early version of this tool
// uploaded through the path that prefixes a fresh timestamp. Such an object is
// unreachable -- no reader can derive its name -- so it is pure waste.
var orphanRendition = regexp.MustCompile(`^\d{16,20}-\d{16,20}-.*-(?:400|800|1200)w\.(?:jpe?g|png)$`)

// cleanOrphanRenditions deletes those objects.
//
// Narrow on purpose, and doubly guarded: only names matching the two-timestamp
// shape, and only objects written in the last day, so nothing that predates
// this tool can be caught by a pattern that happens to match.
func cleanOrphanRenditions(ctx context.Context, client *firebase.FirebaseClient, bucket string, dryRun bool) {
	cutoff := time.Now().Add(-24 * time.Hour)
	it := client.StorageClient.Bucket(bucket).Objects(ctx, nil)

	var found, deleted int
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Fatalf("listing the bucket: %v", err)
		}
		if !orphanRendition.MatchString(attrs.Name) || attrs.Created.Before(cutoff) {
			continue
		}
		found++
		if dryRun {
			log.Printf("  would delete %s", attrs.Name)
			continue
		}
		if err := client.StorageClient.Bucket(bucket).Object(attrs.Name).Delete(ctx); err != nil {
			log.Printf("  could not delete %s: %v", attrs.Name, err)
			continue
		}
		deleted++
	}
	log.Printf("orphan renditions: %d found, %d deleted", found, deleted)
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the bucket answered %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 20<<20))
}

// isRendition reports whether an object is itself one of our smaller sizes,
// by the "-400w" style suffix the names carry.
func isRendition(object string) bool {
	for _, width := range []int{imageproc.ThumbWidth, imageproc.MediumWidth, imageproc.LargeWidth} {
		suffix := fmt.Sprintf("-%dw", width)
		base := object
		if dot := lastDot(object); dot > 0 {
			base = object[:dot]
		}
		if len(base) > len(suffix) && base[len(base)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
