// Command backfill-images repairs products whose image_url/images still point
// at the dead VPS path (https://api.makwatches.in/uploads/...) by pulling
// replacement photos from a curated "bucket pool" JSON file -- images
// harvested from the brand's own official site, grouped by (brand, category)
// -- downloading them, re-uploading to this project's Firebase Storage
// bucket, and updating the matching Mongo documents.
//
// A bucket pool file looks like:
//
//	{"buckets": [
//	  {"brand": "Titan", "category": "Men — metal watch", "images": ["https://...", ...]},
//	  ...
//	]}
//
// For each bucket, every product matching that exact brand + category whose
// image_url still starts with the dead-VPS prefix is updated with up to
// -max-images images cycled from the bucket's pool (product N gets pool
// images [N, N+1, N+2, ...] wrapping around) -- many products legitimately
// end up sharing photos, which is expected given 1:1 unique sourcing isn't
// tractable at this scale. Products whose image_url is already anything else
// (storage.googleapis.com, or otherwise not the dead path) are left alone.
//
// A source image is downloaded and uploaded to Firebase at most once per run
// -- repeats within or across buckets reuse the already-uploaded URL.
//
// Usage:
//
//	go run ./cmd/backfill-images -file cmd/backfill-images/data/titan-family.json
//	go run ./cmd/backfill-images -file <path> -dry-run   # preview only, no writes
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
)

// brokenImagePrefix is the signature of the dead VPS's upload path. Only
// products whose image_url still starts with this are touched -- anything
// else (storage.googleapis.com from this project's Firebase bucket, or any
// other path) is already fine and must be left alone.
const brokenImagePrefix = "https://api.makwatches.in/uploads/"

type imageBucket struct {
	Brand    string   `json:"brand"`
	Category string   `json:"category"`
	Images   []string `json:"images"`
	// ExcludeNameContains, when set, skips any product whose name contains
	// this substring (case-insensitive) even if it matches brand+category --
	// e.g. a handful of "Casio"-branded rows in Mongo are actually mislabeled
	// London Fog products, and must not be given Casio-logo photos.
	ExcludeNameContains string `json:"excludeNameContains,omitempty"`
}

type bucketFile struct {
	Buckets []imageBucket `json:"buckets"`
}

func main() {
	filePath := flag.String("file", "", "path to a bucket pool JSON file (see cmd/backfill-images/data/)")
	dryRun := flag.Bool("dry-run", false, "preview what would be updated without writing anything")
	maxImages := flag.Int("max-images", 3, "max images to attach per product, cycled from the bucket's pool")
	flag.Parse()

	if *filePath == "" {
		log.Fatal("-file is required")
	}
	if *maxImages < 1 {
		log.Fatal("-max-images must be at least 1")
	}

	raw, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("reading bucket file: %v", err)
	}
	var bf bucketFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		log.Fatalf("parsing bucket file: %v", err)
	}
	log.Printf("Loaded %d bucket(s) from %s", len(bf.Buckets), *filePath)

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	mongoClient, mongoDB, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatalf("connecting to MongoDB: %v", err)
	}
	defer mongoClient.Disconnect(context.Background())
	products := mongoDB.Collection("products")

	var fb *firebase.FirebaseClient
	if !*dryRun {
		fb, err = firebase.NewFirebaseClient(ctx, cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
		if err != nil {
			log.Fatalf("connecting to Firebase Storage: %v", err)
		}
	}

	// Source image URL -> already-uploaded Firebase public URL. Shared across
	// every bucket in this run so the same harvested photo isn't re-downloaded
	// and re-uploaded every time it's cycled to a new product.
	uploadCache := map[string]string{}

	totalUpdated, totalFailed, totalSkippedNoMatch := 0, 0, 0

	for bi, b := range bf.Buckets {
		log.Printf("=== [%d/%d] bucket %s / %s (%d source image(s)) ===", bi+1, len(bf.Buckets), b.Brand, b.Category, len(b.Images))
		if len(b.Images) == 0 {
			log.Printf("  SKIP: bucket has no source images")
			continue
		}

		filter := bson.M{
			"brand":    b.Brand,
			"category": b.Category,
			"image_url": bson.M{
				"$regex": primitive.Regex{Pattern: "^" + regexp.QuoteMeta(brokenImagePrefix)},
			},
		}
		if b.ExcludeNameContains != "" {
			filter["name"] = bson.M{
				"$not": primitive.Regex{Pattern: regexp.QuoteMeta(b.ExcludeNameContains), Options: "i"},
			}
			log.Printf("  excluding names containing %q", b.ExcludeNameContains)
		}
		cursor, err := products.Find(ctx, filter)
		if err != nil {
			log.Printf("  ERROR querying Mongo: %v", err)
			continue
		}
		var docs []bson.M
		if err := cursor.All(ctx, &docs); err != nil {
			log.Printf("  ERROR reading matches: %v", err)
			continue
		}
		if len(docs) == 0 {
			log.Printf("  no matching broken products for this bucket")
			totalSkippedNoMatch++
			continue
		}
		log.Printf("  %d matching broken product(s)", len(docs))

		n := *maxImages
		if n > len(b.Images) {
			n = len(b.Images)
		}

		for i, doc := range docs {
			id := doc["_id"]
			name, _ := doc["name"].(string)

			var uploaded []string
			for k := 0; k < n; k++ {
				srcURL := b.Images[(i+k)%len(b.Images)]

				if *dryRun {
					uploaded = append(uploaded, srcURL)
					continue
				}

				fbURL, ok := uploadCache[srcURL]
				if !ok {
					fbURL, err = downloadAndUpload(ctx, fb, srcURL, b.Brand, name, k+1)
					if err != nil {
						log.Printf("  [%s] image %d upload failed (%s): %v", name, k+1, srcURL, err)
						continue
					}
					uploadCache[srcURL] = fbURL
				}
				uploaded = append(uploaded, fbURL)
			}

			if len(uploaded) == 0 {
				log.Printf("  [%s] ERROR: no images available, skipping product", name)
				totalFailed++
				continue
			}

			if *dryRun {
				log.Printf("  [%d/%d] DRY RUN: %s -> would set %d image(s)", i+1, len(docs), name, len(uploaded))
				totalUpdated++
				continue
			}

			update := bson.M{"$set": bson.M{
				"image_url":  uploaded[0],
				"images":     uploaded,
				"updated_at": time.Now(),
			}}
			if _, err := products.UpdateOne(ctx, bson.M{"_id": id}, update); err != nil {
				log.Printf("  [%s] ERROR updating Mongo: %v", name, err)
				totalFailed++
				continue
			}
			if (i+1)%25 == 0 || i+1 == len(docs) {
				log.Printf("  [%d/%d] updated: %s (%d image(s))", i+1, len(docs), name, len(uploaded))
			}
			totalUpdated++
		}
	}

	log.Printf("Done. updated=%d failed=%d buckets_with_no_matches=%d unique_images_uploaded=%d",
		totalUpdated, totalFailed, totalSkippedNoMatch, len(uploadCache))
}

// downloadAndUpload fetches a single source image and re-uploads it to
// Firebase Storage, returning the resulting public URL. Mirrors
// cmd/seed-catalog's uploadImages helper (same User-Agent workaround for
// retail-site WAFs, same content-type-from-extension logic).
//
// A srcURL of the form "file:///abs/path/to/image.jpg" is read from local
// disk instead of downloaded -- an escape hatch for CDNs that block plain
// HTTP clients on TLS/bot-detection grounds even with a browser User-Agent
// (img.fossil.in does this); for those, fetch the bytes through the Browser
// MCP tool as base64 first, write them to a local file, and point the
// bucket pool's "images" entry at "file://" + that path.
func downloadAndUpload(ctx context.Context, fb *firebase.FirebaseClient, srcURL, brand, name string, index int) (string, error) {
	var body []byte

	if localPath, ok := strings.CutPrefix(srcURL, "file://"); ok {
		var err error
		body, err = os.ReadFile(localPath)
		if err != nil {
			return "", fmt.Errorf("reading local file failed: %w", err)
		}
	} else {
		client := &http.Client{Timeout: 30 * time.Second}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
		if err != nil {
			return "", fmt.Errorf("bad request: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("download failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download returned %s", resp.Status)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("reading body failed: %w", err)
		}
	}

	ext := extFromURL(srcURL)
	filename := fmt.Sprintf("%s-%s-%d%s", slugify(brand), slugify(name), index, ext)
	publicURL, err := fb.UploadFile(ctx, bytes.NewReader(body), filename)
	if err != nil {
		return "", fmt.Errorf("firebase upload failed: %w", err)
	}
	return publicURL, nil
}

func extFromURL(url string) string {
	u := strings.SplitN(url, "?", 2)[0]
	switch {
	case strings.HasSuffix(strings.ToLower(u), ".png"):
		return ".png"
	case strings.HasSuffix(strings.ToLower(u), ".webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
