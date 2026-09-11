// Command seed-catalog reads a curated JSON product batch, downloads each
// product's source images, re-uploads them to this project's own Firebase
// Storage bucket, and inserts the resulting product documents into Mongo.
//
// Re-running the same batch file is safe: a product already present (same
// name + brand) is skipped, not duplicated. Meant to be run in small,
// reviewed batches -- see cmd/seed-catalog/data/ for the batch file format.
//
// Usage:
//
//	go run ./cmd/seed-catalog -file cmd/seed-catalog/data/2026-09-mass-market-batch1.json
//	go run ./cmd/seed-catalog -file <path> -dry-run   # preview only, no writes
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imagefetch"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

type batchProduct struct {
	Name               string   `json:"name"`
	Brand              string   `json:"brand"`
	Category           string   `json:"category"`
	MainCategory       string   `json:"mainCategory"`
	Subcategory        string   `json:"subcategory"`
	Price              float64  `json:"price"`
	DiscountPercentage float64  `json:"discountPercentage"`
	Description        string   `json:"description"`
	Stock              int      `json:"stock"`
	Images             []string `json:"images"`
}

type batchFile struct {
	Products []batchProduct `json:"products"`
}

func main() {
	filePath := flag.String("file", "", "path to a batch JSON file (see cmd/seed-catalog/data/)")
	dryRun := flag.Bool("dry-run", false, "preview what would be created without writing anything")
	flag.Parse()

	if *filePath == "" {
		log.Fatal("-file is required")
	}

	raw, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("reading batch file: %v", err)
	}
	var batch batchFile
	if err := json.Unmarshal(raw, &batch); err != nil {
		log.Fatalf("parsing batch file: %v", err)
	}
	log.Printf("Loaded %d product(s) from %s", len(batch.Products), *filePath)

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	created, skipped, failed := 0, 0, 0
	for i, p := range batch.Products {
		log.Printf("[%d/%d] %s — %s", i+1, len(batch.Products), p.Brand, p.Name)

		exists, err := productExists(ctx, products, p.Name, p.Brand)
		if err != nil {
			log.Printf("  ERROR checking for duplicate: %v", err)
			failed++
			continue
		}
		if exists {
			log.Printf("  SKIP: already exists")
			skipped++
			continue
		}

		if *dryRun {
			log.Printf("  DRY RUN: would upload %d image(s) and insert with price ₹%.0f, discount %.0f%%",
				len(p.Images), p.Price, p.DiscountPercentage)
			continue
		}

		uploadedURLs, err := uploadImages(ctx, fb, p.Images, p.Brand, p.Name)
		if err != nil {
			log.Printf("  ERROR uploading images: %v", err)
			failed++
			continue
		}
		if len(uploadedURLs) == 0 {
			log.Printf("  ERROR: no images uploaded successfully, skipping product")
			failed++
			continue
		}

		now := time.Now()
		discount := p.DiscountPercentage
		doc := models.Product{
			Name:               p.Name,
			Brand:              p.Brand,
			Description:        p.Description,
			Price:              p.Price,
			Category:           p.Category,
			MainCategory:       p.MainCategory,
			Subcategory:        p.Subcategory,
			ImageURL:           uploadedURLs[0],
			Images:             uploadedURLs,
			Stock:              p.Stock,
			DiscountPercentage: &discount,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if _, err := products.InsertOne(ctx, doc); err != nil {
			log.Printf("  ERROR inserting product: %v", err)
			failed++
			continue
		}
		log.Printf("  OK: inserted with %d image(s)", len(uploadedURLs))
		created++
	}

	log.Printf("Done. created=%d skipped=%d failed=%d", created, skipped, failed)
}

var skuSuffixRe = regexp.MustCompile(`(?i)[-–—]\s*([A-Za-z0-9]{5,15})\s*$`)

func productExists(ctx context.Context, coll *mongo.Collection, name, brand string) (bool, error) {
	// 1. Exact case-insensitive match on name + brand
	count, err := coll.CountDocuments(ctx, bson.M{
		"brand": bson.M{"$regex": "^" + regexp.QuoteMeta(strings.TrimSpace(brand)) + "$", "$options": "i"},
		"name":  bson.M{"$regex": "^" + regexp.QuoteMeta(strings.TrimSpace(name)) + "$", "$options": "i"},
	}, options.Count().SetLimit(1))
	if err != nil || count > 0 {
		return count > 0, err
	}

	// 2. Trailing SKU match for that brand (e.g. "... - 6296SM01")
	if m := skuSuffixRe.FindStringSubmatch(strings.TrimSpace(name)); len(m) > 1 {
		sku := m[1]
		skuCount, err := coll.CountDocuments(ctx, bson.M{
			"brand": bson.M{"$regex": "^" + regexp.QuoteMeta(strings.TrimSpace(brand)) + "$", "$options": "i"},
			"name":  bson.M{"$regex": regexp.QuoteMeta(sku), "$options": "i"},
		}, options.Count().SetLimit(1))
		if err != nil || skuCount > 0 {
			return skuCount > 0, err
		}
	}

	return false, nil
}

// uploadImages downloads and validates each source URL (see
// internal/imagefetch -- rejects thumbnails below its minimum resolution)
// and re-uploads it to Firebase Storage. A single rejected/failed image is
// logged and skipped rather than failing the whole product -- a product
// with 2 of 4 images is still worth having.
func uploadImages(ctx context.Context, fb *firebase.FirebaseClient, urls []string, brand, name string) ([]string, error) {
	var uploaded []string
	for i, url := range urls {
		res, err := imagefetch.Fetch(ctx, url)
		if err != nil {
			log.Printf("    image %d rejected: %v", i+1, err)
			continue
		}
		filename := fmt.Sprintf("%s-%s-%d%s", imagefetch.Slugify(brand), imagefetch.Slugify(name), i+1, res.Ext)
		publicURL, err := fb.UploadFile(ctx, bytes.NewReader(res.Body), filename)
		if err != nil {
			log.Printf("    image %d: firebase upload failed: %v", i+1, err)
			continue
		}
		uploaded = append(uploaded, publicURL)
	}
	return uploaded, nil
}
