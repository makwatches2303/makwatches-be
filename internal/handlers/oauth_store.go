package handlers

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// oauthTempDoc backs the short-lived OAuth CSRF state and post-login
// exchange code. This used to live in Redis; it's Mongo now so the OAuth
// flow works with no Redis configured at all -- Mongo is already a hard
// requirement for this app to run, so this trades an optional dependency for
// one that was never actually optional. A TTL index (see
// ensureOAuthTempIndex) does the expiry, matching Redis's own EXPIRE
// semantics; FindOneAndDelete makes the single-use consume atomic, which the
// previous Get-then-Del pair over Redis wasn't quite.
type oauthTempDoc struct {
	ID        string    `bson:"_id"`
	Token     string    `bson:"token,omitempty"`
	ExpiresAt time.Time `bson:"expires_at"`
}

const oauthTempCollection = "oauth_tmp"

// ensureOAuthTempIndex creates the TTL index if it doesn't already exist.
// Best-effort: a failure here (e.g. no index-creation permission) degrades
// to entries simply outliving their TTL rather than the app failing to
// start, since oauthTempConsume also checks ExpiresAt itself.
func ensureOAuthTempIndex(ctx context.Context, db *mongo.Database) {
	_, err := db.Collection(oauthTempCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	})
	if err != nil {
		log.Printf("[AUTH] WARNING: failed to ensure oauth_tmp TTL index: %v", err)
	}
}

// oauthTempSet stores a single-use entry under key, expiring after ttl.
// token is the payload for the post-login exchange code; the CSRF state
// entry passes "" since it only needs to exist.
func oauthTempSet(ctx context.Context, db *mongo.Database, key, token string, ttl time.Duration) error {
	opts := options.Replace().SetUpsert(true)
	_, err := db.Collection(oauthTempCollection).ReplaceOne(ctx, bson.M{"_id": key}, oauthTempDoc{
		ID:        key,
		Token:     token,
		ExpiresAt: time.Now().Add(ttl),
	}, opts)
	return err
}

// oauthTempConsume looks up key and deletes it atomically (single-use),
// returning the stored token and whether a live (non-expired) entry was
// found. A caller that only needs to know the key existed (the CSRF state
// check) ignores the returned token.
func oauthTempConsume(ctx context.Context, db *mongo.Database, key string) (token string, ok bool) {
	var doc oauthTempDoc
	err := db.Collection(oauthTempCollection).FindOneAndDelete(ctx, bson.M{"_id": key}).Decode(&doc)
	if err != nil {
		return "", false
	}
	if time.Now().After(doc.ExpiresAt) {
		// The TTL index usually beats us to it, but don't trust the race.
		return "", false
	}
	return doc.Token, true
}
