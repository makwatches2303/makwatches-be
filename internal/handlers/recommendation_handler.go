package handlers

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageurl"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// RecommendationHandler handles product recommendations
type RecommendationHandler struct {
	DB     *database.DBClient
	Config *config.Config
}

// NewRecommendationHandler creates a new instance of RecommendationHandler
func NewRecommendationHandler(db *database.DBClient, cfg *config.Config) *RecommendationHandler {
	return &RecommendationHandler{
		DB:     db,
		Config: cfg,
	}
}

// GetRecommendations returns product recommendations for the current user
func (h *RecommendationHandler) GetRecommendations(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get user info from token
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	// Try to get recommendations from cache
	cacheKey := fmt.Sprintf("recommendations:%s", user.UserID.Hex())
	var cachedRecommendations []fiber.Map
	err := h.DB.CacheGet(ctx, cacheKey, &cachedRecommendations)
	if err == nil && len(cachedRecommendations) > 0 {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Recommendations retrieved from cache",
			"data":    cachedRecommendations,
			"source":  "cache",
		})
	}

	// Get user preferences
	var userPrefs models.UserPreferences
	prefsCollection := h.DB.Collections().UserPreferences
	err = prefsCollection.FindOne(ctx, bson.M{"user_id": user.UserID}).Decode(&userPrefs)

	// Query parameters
	limit := 10
	if c.Query("limit") != "" {
		fmt.Sscanf(c.Query("limit"), "%d", &limit)
		if limit <= 0 || limit > 50 {
			limit = 10
		}
	}

	// Set up recommendation query
	productCollection := h.DB.Collections().Products
	findOptions := options.Find().SetLimit(int64(limit))

	// Base query - get products with sufficient stock
	query := bson.M{"stock": bson.M{"$gt": 0}}

	// Add preference-based filters if available
	if err == nil {
		// If we have user preferences, use them to filter recommendations

		// Filter by categories if user has favorite categories
		if len(userPrefs.FavoriteCategories) > 0 {
			query["category"] = bson.M{"$in": userPrefs.FavoriteCategories}
		}

		// Filter by price range if set
		if len(userPrefs.PriceRange) == 2 {
			query["price"] = bson.M{
				"$gte": userPrefs.PriceRange[0],
				"$lte": userPrefs.PriceRange[1],
			}
		}

		// Set sort order - newest products first, but give priority to favorite brands if available
		if len(userPrefs.FavoriteBrands) > 0 {
			// Add score field for sorting
			pipeline := []bson.M{
				{
					"$addFields": bson.M{
						"brandScore": bson.M{
							"$cond": bson.M{
								"if":   bson.M{"$in": []interface{}{"$brand", userPrefs.FavoriteBrands}},
								"then": 1,
								"else": 0,
							},
						},
					},
				},
				{"$match": query},
				{"$sort": bson.D{
					{Key: "brandScore", Value: -1},
					{Key: "created_at", Value: -1},
				}},
				{"$limit": limit},
			}

			// Execute aggregation pipeline
			cursor, err := productCollection.Aggregate(ctx, pipeline)
			if err != nil {
				// Fall back to regular query if aggregation fails
				findOptions.SetSort(bson.D{{Key: "created_at", Value: -1}})
			} else {
				defer cursor.Close(ctx)

				// Decode results
				var products []models.Product
				if err := cursor.All(ctx, &products); err != nil {
					return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
						"success": false,
						"message": "Failed to decode recommendations",
						"error":   err.Error(),
					})
				}

				// Build response
				recommendations := buildRecommendationsResponse(products, h.Config.FirebaseBucketName)

				// Cache the results
				h.DB.CacheSet(ctx, cacheKey, recommendations, 30*60) // 30 minutes

				return c.Status(fiber.StatusOK).JSON(fiber.Map{
					"success": true,
					"message": "Personalized recommendations retrieved successfully",
					"data":    recommendations,
					"source":  "personalized",
				})
			}
		}
	} else {
		// If we don't have user preferences, just sort by popularity and creation date
		findOptions.SetSort(bson.D{{Key: "created_at", Value: -1}})
	}

	// Execute query
	cursor, err := productCollection.Find(ctx, query, findOptions)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve recommendations",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	// Decode results
	var products []models.Product
	if err := cursor.All(ctx, &products); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode recommendations",
			"error":   err.Error(),
		})
	}

	// If no products found based on preferences, get popular products
	if len(products) == 0 {
		cursor, err = productCollection.Find(
			ctx,
			bson.M{"stock": bson.M{"$gt": 0}},
			options.Find().SetLimit(int64(limit)).SetSort(bson.D{{Key: "created_at", Value: -1}}),
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to retrieve popular products",
				"error":   err.Error(),
			})
		}
		defer cursor.Close(ctx)

		if err := cursor.All(ctx, &products); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to decode popular products",
				"error":   err.Error(),
			})
		}
	}

	// Build response
	recommendations := buildRecommendationsResponse(products, h.Config.FirebaseBucketName)

	// Cache the results
	h.DB.CacheSet(ctx, cacheKey, recommendations, 30*60) // 30 minutes

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Recommendations retrieved successfully",
		"data":    recommendations,
		"source":  "general",
	})
}

// SubmitFeedback records user feedback for recommendations
func (h *RecommendationHandler) SubmitFeedback(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get user info from token
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	// Parse request body
	var req struct {
		ProductID string `json:"productId" validate:"required"`
		Rating    int    `json:"rating" validate:"required,min=1,max=5"`
		Action    string `json:"action" validate:"required,oneof=click view add_to_cart purchase dismiss"`
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Convert string ID to ObjectID
	productID, err := primitive.ObjectIDFromHex(req.ProductID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID",
		})
	}

	// Check if product exists
	productCollection := h.DB.Collections().Products
	var product models.Product
	err = productCollection.FindOne(ctx, bson.M{"_id": productID}).Decode(&product)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Product not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to check product",
			"error":   err.Error(),
		})
	}

	// Record feedback
	now := time.Now()
	feedback := models.RecommendationFeedback{
		ID:        primitive.NewObjectID(),
		UserID:    user.UserID,
		ProductID: productID,
		Action:    req.Action,
		CreatedAt: now,
	}

	_, err = h.DB.Collections().RecFeedbacks.InsertOne(ctx, feedback)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to save feedback",
			"error":   err.Error(),
		})
	}

	// Update recommendation model (just clear the cache for now, could be more sophisticated)
	cacheKey := fmt.Sprintf("recommendations:%s", user.UserID.Hex())
	h.DB.CacheDel(ctx, cacheKey)

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "Feedback recorded successfully",
	})
}

// Helper function to build recommendation response
func buildRecommendationsResponse(products []models.Product, bucket string) []fiber.Map {
	recommendations := make([]fiber.Map, 0, len(products))
	for _, product := range products {
		image := imageurl.Resolve(product.ImageURL, bucket)
		if image == "" && len(product.Images) > 0 {
			image = imageurl.Resolve(product.Images[0], bucket)
		}
		recommendations = append(recommendations, fiber.Map{
			"id":          product.ID,
			"name":        product.Name,
			"price":       product.Price,
			"image":       image,
			"description": product.Description,
			"category":    product.Category,
			"inStock":     product.Stock > 0,
		})
	}
	return recommendations
}
