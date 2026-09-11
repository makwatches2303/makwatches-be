// Command dump-brand-products pulls _id + name (+ current image_url) for
// every product of a given brand into a JSON worklist file, in one Mongo
// round-trip. Used as the first step of a per-product 1:1 image backfill --
// the worklist is then walked externally (e.g. via a browser tool) to find
// each product's real source-site SKU and images, without repeated DB
// round-trips.
//
// Usage:
//
//	go run ./cmd/dump-brand-products -brand Fastrack -out cmd/apply-product-images/data/fastrack-worklist.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
)

type workItem struct {
	ID       primitive.ObjectID `json:"id"`
	Name     string             `json:"name"`
	Category string             `json:"category"`
	ImageURL string             `json:"imageUrl"`
}

func main() {
	brand := flag.String("brand", "", "exact brand value to filter on (e.g. Fastrack)")
	out := flag.String("out", "", "path to write the worklist JSON to")
	flag.Parse()

	if *brand == "" || *out == "" {
		log.Fatal("-brand and -out are required")
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mongoClient, mongoDB, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatalf("connecting to MongoDB: %v", err)
	}
	defer mongoClient.Disconnect(context.Background())
	products := mongoDB.Collection("products")

	cursor, err := products.Find(ctx, bson.M{"brand": *brand})
	if err != nil {
		log.Fatalf("querying Mongo: %v", err)
	}
	var docs []struct {
		ID       primitive.ObjectID `bson:"_id"`
		Name     string             `bson:"name"`
		Category string             `bson:"category"`
		ImageURL string             `bson:"image_url"`
	}
	if err := cursor.All(ctx, &docs); err != nil {
		log.Fatalf("reading matches: %v", err)
	}

	items := make([]workItem, 0, len(docs))
	for _, d := range docs {
		items = append(items, workItem{ID: d.ID, Name: d.Name, Category: d.Category, ImageURL: d.ImageURL})
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("creating output file: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(items); err != nil {
		log.Fatalf("writing JSON: %v", err)
	}

	log.Printf("Wrote %d %s product(s) to %s", len(items), *brand, *out)
}
