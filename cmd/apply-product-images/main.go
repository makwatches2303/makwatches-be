// Command apply-product-images gives individual products their own genuine
// images, keyed by exact Mongo _id -- the per-product counterpart to
// cmd/backfill-images' category-bucket pooling.
//
// Input is a JSON array of harvested per-product results, produced externally
// (e.g. by driving a browser to each product's real source-site page and
// pulling its own image URLs):
//
//	[
//	  {"product_id": "<hex ObjectID>", "product_name": "...", "images": ["https://...", ...]},
//	  ...
//	]
//
// For each entry, every image URL is downloaded and re-uploaded to this
// project's Firebase Storage bucket, then the matching product is updated by
// its exact _id with image_url = the first uploaded image and images = all of
// them -- never touching any other product, unlike the old bucket-pooling
// tool which matched many products per (brand, category).
//
// A worker pool processes entries concurrently (-workers, default 8) since
// this step is pure network + Firebase I/O with no browser involved and is
// safe to parallelize.
//
// Usage:
//
//	go run ./cmd/apply-product-images -file cmd/apply-product-images/data/batch-001.json
//	go run ./cmd/apply-product-images -file <path> -dry-run   # preview only, no writes
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
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
)

type harvestItem struct {
	ProductID   string   `json:"product_id"`
	ProductName string   `json:"product_name,omitempty"`
	Images      []string `json:"images"`
}

type result struct {
	name    string
	updated bool
	err     error
}

func main() {
	filePath := flag.String("file", "", "path to a harvested batch JSON file (array of {product_id, product_name, images})")
	dryRun := flag.Bool("dry-run", false, "preview what would be updated without writing anything")
	workers := flag.Int("workers", 8, "number of concurrent workers for download+upload+update")
	flag.Parse()

	if *filePath == "" {
		log.Fatal("-file is required")
	}
	if *workers < 1 {
		*workers = 1
	}

	raw, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("reading batch file: %v", err)
	}
	var items []harvestItem
	if err := json.Unmarshal(raw, &items); err != nil {
		log.Fatalf("parsing batch file: %v", err)
	}
	log.Printf("Loaded %d item(s) from %s", len(items), *filePath)

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

	jobs := make(chan harvestItem)
	results := make(chan result)

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				results <- processItem(ctx, products, fb, item, *dryRun)
			}
		}()
	}

	go func() {
		for _, item := range items {
			jobs <- item
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	updated, failed := 0, 0
	done := 0
	for r := range results {
		done++
		if r.err != nil {
			log.Printf("[%d/%d] FAILED: %s: %v", done, len(items), r.name, r.err)
			failed++
			continue
		}
		if r.updated {
			updated++
		}
		if done%25 == 0 || done == len(items) {
			log.Printf("[%d/%d] progress: updated=%d failed=%d", done, len(items), updated, failed)
		}
	}

	log.Printf("Done. updated=%d failed=%d total=%d", updated, failed, len(items))
}

// processItem downloads+re-uploads every image for one harvested product and
// (unless dryRun) updates that exact product document by _id.
func processItem(ctx context.Context, products *mongo.Collection, fb *firebase.FirebaseClient, item harvestItem, dryRun bool) result {
	name := item.ProductName
	if name == "" {
		name = item.ProductID
	}

	objID, err := primitive.ObjectIDFromHex(item.ProductID)
	if err != nil {
		return result{name: name, err: fmt.Errorf("invalid product_id %q: %w", item.ProductID, err)}
	}
	if len(item.Images) == 0 {
		return result{name: name, err: fmt.Errorf("no images provided")}
	}

	var uploaded []string
	for k, srcURL := range item.Images {
		if dryRun {
			uploaded = append(uploaded, srcURL)
			continue
		}
		fbURL, err := downloadAndUpload(ctx, fb, srcURL, "fastrack", name, k+1)
		if err != nil {
			log.Printf("  [%s] image %d upload failed (%s): %v", name, k+1, srcURL, err)
			continue
		}
		uploaded = append(uploaded, fbURL)
	}

	if len(uploaded) == 0 {
		return result{name: name, err: fmt.Errorf("all %d image upload(s) failed", len(item.Images))}
	}

	if dryRun {
		log.Printf("  DRY RUN: %s (%s) -> would set %d image(s)", name, item.ProductID, len(uploaded))
		return result{name: name, updated: true}
	}

	update := bson.M{"$set": bson.M{
		"image_url":  uploaded[0],
		"images":     uploaded,
		"updated_at": time.Now(),
	}}
	if _, err := products.UpdateOne(ctx, bson.M{"_id": objID}, update); err != nil {
		return result{name: name, err: fmt.Errorf("updating Mongo: %w", err)}
	}
	return result{name: name, updated: true}
}

// downloadAndUpload fetches a single source image and re-uploads it to
// Firebase Storage, returning the resulting public URL. Mirrors
// cmd/backfill-images' helper of the same name (same User-Agent workaround
// for retail-site WAFs, same content-type-from-extension logic).
//
// A srcURL of the form "file:///abs/path/to/image.jpg" is read from local
// disk instead of downloaded -- an escape hatch for CDNs that block plain
// HTTP clients on TLS/bot-detection grounds even with a browser User-Agent.
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
