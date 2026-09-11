package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-redis/redis/v8"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DBClient represents our database client with both MongoDB and Redis connections
type DBClient struct {
	MongoDB *mongo.Database
	Redis   *redis.Client
}

// NewDBClient creates a new database client wrapper
func NewDBClient(mongoClient *mongo.Client, dbName string, redisClient *redis.Client) *DBClient {
	return &DBClient{
		MongoDB: mongoClient.Database(dbName),
		Redis:   redisClient,
	}
}

// Collections returns MongoDB collections
func (db *DBClient) Collections() struct {
	Users             *mongo.Collection
	Products          *mongo.Collection
	Categories        *mongo.Collection
	CartItems         *mongo.Collection
	Orders            *mongo.Collection
	UserProfiles      *mongo.Collection
	UserPreferences   *mongo.Collection
	UserAddresses     *mongo.Collection
	Inventories       *mongo.Collection
	Reviews           *mongo.Collection
	Wishlists         *mongo.Collection
	ChatConversations *mongo.Collection
	ChatMessages      *mongo.Collection
	Notifications     *mongo.Collection
	Recommendations   *mongo.Collection
	RecFeedbacks      *mongo.Collection
	Subscribers       *mongo.Collection
	AbandonedCarts    *mongo.Collection
} {
	return struct {
		Users             *mongo.Collection
		Products          *mongo.Collection
		Categories        *mongo.Collection
		CartItems         *mongo.Collection
		Orders            *mongo.Collection
		UserProfiles      *mongo.Collection
		UserPreferences   *mongo.Collection
		UserAddresses     *mongo.Collection
		Inventories       *mongo.Collection
		Reviews           *mongo.Collection
		Wishlists         *mongo.Collection
		ChatConversations *mongo.Collection
		ChatMessages      *mongo.Collection
		Notifications     *mongo.Collection
		Recommendations   *mongo.Collection
		RecFeedbacks      *mongo.Collection
		Subscribers       *mongo.Collection
		AbandonedCarts    *mongo.Collection
	}{
		Users:             db.MongoDB.Collection("users"),
		Products:          db.MongoDB.Collection("products"),
		Categories:        db.MongoDB.Collection("categories"),
		CartItems:         db.MongoDB.Collection("cart_items"),
		Orders:            db.MongoDB.Collection("orders"),
		UserProfiles:      db.MongoDB.Collection("user_profiles"),
		UserPreferences:   db.MongoDB.Collection("user_preferences"),
		UserAddresses:     db.MongoDB.Collection("user_addresses"),
		Inventories:       db.MongoDB.Collection("inventories"),
		Reviews:           db.MongoDB.Collection("reviews"),
		Wishlists:         db.MongoDB.Collection("wishlists"),
		ChatConversations: db.MongoDB.Collection("chat_conversations"),
		ChatMessages:      db.MongoDB.Collection("chat_messages"),
		Notifications:     db.MongoDB.Collection("notifications"),
		Recommendations:   db.MongoDB.Collection("recommendations"),
		RecFeedbacks:      db.MongoDB.Collection("recommendation_feedbacks"),
		Subscribers:       db.MongoDB.Collection("subscribers"),
		AbandonedCarts:    db.MongoDB.Collection("abandoned_carts"),
	}
}

// CacheGet retrieves data from Redis cache
func (db *DBClient) CacheGet(ctx context.Context, key string, dest interface{}) error {
	// Check if Redis is available
	if db.Redis == nil {
		return errors.New("redis not available")
	}
	
	val, err := db.Redis.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return errors.New("key not found in cache")
		}
		return err
	}

	return json.Unmarshal([]byte(val), dest)
}

// CacheSet stores data in Redis cache
func (db *DBClient) CacheSet(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	// Check if Redis is available
	if db.Redis == nil {
		return nil // Silently skip if Redis is not available
	}
	
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return db.Redis.Set(ctx, key, data, expiration).Err()
}

// CacheVersion returns the current value of a named versioned-cache counter
// (0 if it has never been bumped, or if Redis is unavailable). Callers build
// cache keys that embed this value -- e.g. fmt.Sprintf("products:v%d:...",
// version) -- so that BumpCacheVersion invalidates every such key at once
// without having to know or enumerate them.
//
// This exists because a paginated, filtered, sorted list (like the product
// listing) can be cached under effectively unbounded key combinations
// (every filter × sort × page), so a write path can never reliably guess
// and delete "the" key to invalidate -- see the history of a real bug here:
// writes were calling CacheDel with a key that never matched the real
// cached key format, silently leaving stale list pages cached for up to
// their full TTL after every create/update/delete.
func (db *DBClient) CacheVersion(ctx context.Context, name string) int64 {
	if db.Redis == nil {
		return 0
	}
	v, err := db.Redis.Get(ctx, "cache_version:"+name).Int64()
	if err != nil {
		return 0
	}
	return v
}

// BumpCacheVersion increments a named versioned-cache counter (see
// CacheVersion), orphaning every key built from its previous value. Orphaned
// keys are never actively deleted -- they simply stop being read, and expire
// on their own existing TTL like any other cache entry.
func (db *DBClient) BumpCacheVersion(ctx context.Context, name string) {
	if db.Redis == nil {
		return
	}
	db.Redis.Incr(ctx, "cache_version:"+name)
}

// CacheDel deletes data from Redis cache
func (db *DBClient) CacheDel(ctx context.Context, keys ...string) error {
	// Check if Redis is available
	if db.Redis == nil {
		return nil // Silently skip if Redis is not available
	}
	
	return db.Redis.Del(ctx, keys...).Err()
}

// FindByID is a generic function to find a document by ID
func (db *DBClient) FindByID(ctx context.Context, collection *mongo.Collection, id string, result interface{}) error {
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return err
	}

	filter := bson.M{"_id": objectID}
	err = collection.FindOne(ctx, filter).Decode(result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return errors.New("document not found")
		}
		return err
	}

	return nil
}

// FindOne is a generic function to find a document by filter
func (db *DBClient) FindOne(ctx context.Context, collection *mongo.Collection, filter bson.M, result interface{}) error {
	err := collection.FindOne(ctx, filter).Decode(result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return errors.New("document not found")
		}
		return err
	}

	return nil
}

// Find is a generic function to find documents by filter with pagination
func (db *DBClient) Find(ctx context.Context, collection *mongo.Collection, filter bson.M, results interface{}, opts ...*options.FindOptions) error {
	cursor, err := collection.Find(ctx, filter, opts...)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	if err = cursor.All(ctx, results); err != nil {
		return err
	}

	return nil
}

// InsertOne inserts a document into the specified collection
func (db *DBClient) InsertOne(ctx context.Context, collection *mongo.Collection, document interface{}) (primitive.ObjectID, error) {
	result, err := collection.InsertOne(ctx, document)
	if err != nil {
		return primitive.NilObjectID, err
	}

	id, ok := result.InsertedID.(primitive.ObjectID)
	if !ok {
		return primitive.NilObjectID, errors.New("failed to get inserted ID")
	}

	return id, nil
}

// UpdateByID updates a document by ID
func (db *DBClient) UpdateByID(ctx context.Context, collection *mongo.Collection, id string, update bson.M) error {
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return err
	}

	result, err := collection.UpdateOne(ctx, bson.M{"_id": objectID}, bson.M{"$set": update})
	if err != nil {
		return err
	}

	// Check if document was found
	if result.MatchedCount == 0 {
		return errors.New("document not found")
	}

	return nil
}

// DeleteByID deletes a document by ID
func (db *DBClient) DeleteByID(ctx context.Context, collection *mongo.Collection, id string) error {
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return err
	}

	result, err := collection.DeleteOne(ctx, bson.M{"_id": objectID})
	if err != nil {
		return err
	}

	// Check if document was found
	if result.DeletedCount == 0 {
		return errors.New("document not found")
	}

	return nil
}

// DeleteMany deletes documents by filter
func (db *DBClient) DeleteMany(ctx context.Context, collection *mongo.Collection, filter bson.M) (int64, error) {
	result, err := collection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}

	return result.DeletedCount, nil
}
