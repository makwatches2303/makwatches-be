// Command backfill-slugs assigns a URL slug to products that do not have one.
//
// The tool is strictly additive and non-destructive:
//
//   - It only ever writes the `slug` field. No other field is read for writing,
//     updated, or removed.
//   - A product that already has a non-empty slug is skipped. An existing slug
//     is never overwritten, because it may already be a live, indexed URL.
//   - Collisions are resolved by appending a short suffix from the product's
//     own ObjectID, so two products with the same name both get a stable,
//     unique slug.
//   - A product whose name slugifies to nothing (for example, a name written
//     entirely in a non-ASCII script) is left alone rather than given a
//     meaningless slug; those keep resolving through the id route.
//
// It runs in dry-run mode by default and prints what it would do. Pass -apply
// to write.
//
// Usage:
//
//	go run ./cmd/backfill-slugs            # report only, writes nothing
//	go run ./cmd/backfill-slugs -apply     # perform the backfill
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

func main() {
	apply := flag.Bool("apply", false, "write the computed slugs; without this flag the tool only reports")
	flag.Parse()

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	client, db, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatalf("connect mongo: %v", err)
	}
	defer func() {
		if err := client.Disconnect(context.Background()); err != nil {
			log.Printf("disconnect: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	products := db.Collection("products")

	// Only consider records with no usable slug.
	missing := bson.M{"$or": []bson.M{
		{"slug": bson.M{"$exists": false}},
		{"slug": ""},
		{"slug": nil},
	}}

	cursor, err := products.Find(ctx, missing)
	if err != nil {
		log.Fatalf("find products: %v", err)
	}
	defer cursor.Close(ctx)

	var rows []struct {
		ID   primitive.ObjectID `bson:"_id"`
		Name string             `bson:"name"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		log.Fatalf("decode products: %v", err)
	}

	if len(rows) == 0 {
		log.Println("every product already has a slug; nothing to do")
		return
	}

	if !*apply {
		log.Printf("DRY RUN -- %d product(s) missing a slug. Re-run with -apply to write.", len(rows))
	}

	var written, skipped int
	for _, r := range rows {
		base := models.Slugify(r.Name)
		if base == "" {
			log.Printf("skip %s: name %q produces an empty slug", r.ID.Hex(), r.Name)
			skipped++
			continue
		}

		slug, err := uniqueSlug(ctx, products, base, r.ID)
		if err != nil {
			log.Printf("skip %s: %v", r.ID.Hex(), err)
			skipped++
			continue
		}

		if !*apply {
			fmt.Printf("  %s  %q -> %s\n", r.ID.Hex(), r.Name, slug)
			continue
		}

		// Guarded update: the filter re-asserts that the slug is still absent,
		// so a concurrent writer cannot be clobbered between read and write.
		res, err := products.UpdateOne(ctx,
			bson.M{"_id": r.ID, "$or": []bson.M{
				{"slug": bson.M{"$exists": false}},
				{"slug": ""},
				{"slug": nil},
			}},
			bson.M{"$set": bson.M{"slug": slug}},
		)
		if err != nil {
			log.Printf("skip %s: update failed: %v", r.ID.Hex(), err)
			skipped++
			continue
		}
		if res.ModifiedCount == 0 {
			log.Printf("skip %s: slug was set concurrently", r.ID.Hex())
			skipped++
			continue
		}

		written++
	}

	if *apply {
		log.Printf("backfill complete: %d written, %d skipped", written, skipped)
		return
	}
	log.Printf("dry run complete: %d would be written, %d skipped", len(rows)-skipped, skipped)
}

// uniqueSlug returns a slug not already taken by another product.
//
// The plain slug is preferred. If it is taken, a short suffix from the
// product's own ObjectID is appended -- deterministic, so re-running the tool
// produces the same slug for the same product rather than a new one each time.
func uniqueSlug(ctx context.Context, products *mongo.Collection, base string, id primitive.ObjectID) (string, error) {
	taken, err := slugTaken(ctx, products, base, id)
	if err != nil {
		return "", err
	}
	if !taken {
		return base, nil
	}

	// Last 6 hex characters of the id: short enough to stay readable, wide
	// enough (16.7M values) that a collision within one product name is
	// vanishingly unlikely.
	hex := id.Hex()
	candidate := models.SlugWithSuffix(base, hex[len(hex)-6:])

	taken, err = slugTaken(ctx, products, candidate, id)
	if err != nil {
		return "", err
	}
	if taken {
		return "", fmt.Errorf("slug %q and %q are both taken", base, candidate)
	}
	return candidate, nil
}

// slugTaken reports whether any product other than self already uses slug.
func slugTaken(ctx context.Context, products *mongo.Collection, slug string, self primitive.ObjectID) (bool, error) {
	n, err := products.CountDocuments(ctx, bson.M{
		"slug": slug,
		"_id":  bson.M{"$ne": self},
	})
	if err != nil {
		return false, fmt.Errorf("count slug %q: %w", slug, err)
	}
	return n > 0, nil
}
