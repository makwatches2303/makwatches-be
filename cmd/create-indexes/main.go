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
		// Every product page with siblings looks its group up by this field
		// (see catalog.ListVariants), uncached, once per render. Without an
		// index that is a full collection scan on the hot path.
		{Keys: bson.D{{Key: "variant_group_id", Value: 1}}},

		// Keyset pagination sort keys.
		//
		// Listings sort by (field, _id) so the order is total -- see
		// catalog/cursor.go for why the tie-break is what makes progressive
		// loading correct. Each of these lets Mongo seek straight to a cursor
		// position and stream from there in sort order, with no blocking sort
		// and no skip.
		//
		// One index per sort the storefront offers, not one per direction:
		// Mongo walks an index backwards when every key reverses, so
		// {price:1,_id:1} serves price ascending and descending both.
		//
		// There is deliberately no {category, created_at, _id} for the Men and
		// Women scopes. Those match `category` with a prefix regex, which is a
		// range predicate, and a compound index cannot supply sort order after
		// a range key -- it would force an in-memory sort of the whole scope.
		// Walking created_at in order and filtering as it goes is both correct
		// and streaming.
		//
		// These make the single-field created_at, price and name indexes above
		// redundant prefixes. Dropping those is a separate, deliberate call.
		{Keys: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}},
		{Keys: bson.D{{Key: "price", Value: 1}, {Key: "_id", Value: 1}}},
		{Keys: bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}},
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
