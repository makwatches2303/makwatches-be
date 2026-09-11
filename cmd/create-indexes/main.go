// Command create-indexes adds the indexes the products/orders collections
// were missing (both had nothing but the default _id index -- every admin
// list request, sort, category filter, and the new free-text search was
// doing a full collection scan). Indexes persist in MongoDB once created;
// this only needs to run once, not on every deploy.
//
// Usage:
//
//	go run ./cmd/create-indexes
package main

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
)

func main() {
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
	productIndexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "category", Value: 1}}},
		{Keys: bson.D{{Key: "brand", Value: 1}}},
		{Keys: bson.D{{Key: "price", Value: 1}}},
		{Keys: bson.D{{Key: "name", Value: 1}}},
		{Keys: bson.D{{Key: "stock", Value: 1}}},
	}
	names, err := products.Indexes().CreateMany(ctx, productIndexes)
	if err != nil {
		log.Fatalf("creating product indexes: %v", err)
	}
	log.Printf("products: created/confirmed indexes %v", names)

	orders := mongoDB.Collection("orders")
	orderIndexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "user_id", Value: 1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}},
	}
	names, err = orders.Indexes().CreateMany(ctx, orderIndexes)
	if err != nil {
		log.Fatalf("creating order indexes: %v", err)
	}
	log.Printf("orders: created/confirmed indexes %v", names)
}
