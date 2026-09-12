// internal/handlers/admin_account_handler.go
package handlers

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Account is a light representation for admin listing
type Account struct {
	ID    primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	Name  string             `json:"name" bson:"name"`
	Email string             `json:"email" bson:"email"`
	Role  string             `json:"role" bson:"role"`
}

type AdminAccountHandler struct {
	DB *database.DBClient
}

// GetAllAccounts lists user accounts, paginated (admin only).
//
// Previously fetched every user in the database and returned a bare JSON
// array with no envelope -- fine for a handful of accounts, not at scale,
// and inconsistent with every other admin list endpoint. Optional query
// params: page, limit (defaults 1/20, matching GetProducts/GetAllOrders) and
// q (free-text, matches name or email).
func (h *AdminAccountHandler) GetAllAccounts(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	page, err := strconv.Atoi(c.Query("page", "1"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(c.Query("limit", "20"))
	if err != nil || limit < 1 {
		limit = 20
	}

	filter := bson.M{}
	if search := c.Query("q"); search != "" {
		pattern := regexp.QuoteMeta(search)
		filter["$or"] = bson.A{
			bson.M{"name": bson.M{"$regex": pattern, "$options": "i"}},
			bson.M{"email": bson.M{"$regex": pattern, "$options": "i"}},
		}
	}

	collection := h.DB.MongoDB.Collection("users")

	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to count accounts",
		})
	}
	meta := fiber.Map{
		"page":  page,
		"limit": limit,
		"total": total,
		"pages": (total + int64(limit) - 1) / int64(limit),
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetSkip(int64((page - 1) * limit)).
		SetLimit(int64(limit))
	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch accounts",
		})
	}
	defer cursor.Close(ctx)

	accounts := []Account{}
	if err := cursor.All(ctx, &accounts); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to parse accounts",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    accounts,
		"meta":    meta,
	})
}

// DeleteAccount removes a user and (best-effort) all associated data across collections.
// NOTE: If there are regulatory/audit requirements for retaining orders, you may
// want to anonymize instead of deleting those. For now we fully delete.
func (h *AdminAccountHandler) DeleteAccount(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rawID := c.Params("id")
	if rawID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "User ID is required",
		})
	}
	userID, err := primitive.ObjectIDFromHex(rawID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid user ID format",
			"error":   err.Error(),
		})
	}

	// First ensure user exists
	var existing Account
	if err := h.DB.MongoDB.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&existing); err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "User not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to lookup user",
			"error":   err.Error(),
		})
	}

	// Build deletion tasks (collection pointer, filter description)
	// Each uses {user_id: userID} except users collection which uses _id.
	deletions := []struct {
		name       string
		collection string
		filter     bson.M
	}{
		{"user", "users", bson.M{"_id": userID}},
		{"profile", "user_profiles", bson.M{"user_id": userID}},
		{"preferences", "user_preferences", bson.M{"user_id": userID}},
		{"addresses", "user_addresses", bson.M{"user_id": userID}},
		{"cart items", "cart_items", bson.M{"user_id": userID}},
		{"orders", "orders", bson.M{"user_id": userID}},
		{"inventories", "inventories", bson.M{"user_id": userID}},
		{"reviews", "reviews", bson.M{"user_id": userID}},
		{"wishlists", "wishlists", bson.M{"user_id": userID}},
		{"chat conversations", "chat_conversations", bson.M{"user_id": userID}},
		{"chat messages", "chat_messages", bson.M{"user_id": userID}},
		{"notifications", "notifications", bson.M{"user_id": userID}},
		{"recommendations", "recommendations", bson.M{"user_id": userID}},
		{"recommendation feedbacks", "recommendation_feedbacks", bson.M{"user_id": userID}},
	}

	summary := fiber.Map{}
	for _, d := range deletions {
		coll := h.DB.MongoDB.Collection(d.collection)
		res, derr := coll.DeleteMany(ctx, d.filter)
		if derr != nil {
			summary[d.name] = fmt.Sprintf("error: %v", derr)
			continue
		}
		summary[d.name] = res.DeletedCount
	}

	// Best-effort cache invalidation of known per-user keys.
	// (If adding new user-scoped caches, append here.)
	_ = h.DB.CacheDel(ctx,
		fmt.Sprintf("recommendations:%s", userID.Hex()),
		fmt.Sprintf("wishlist:%s", userID.Hex()),
		fmt.Sprintf("profile:%s", userID.Hex()),
	)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "User and related data deleted",
		"data": fiber.Map{
			"userId":  userID.Hex(),
			"summary": summary,
		},
	})
}
