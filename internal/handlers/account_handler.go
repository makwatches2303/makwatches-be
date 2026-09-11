package handlers

import (
	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// AccountHandler handles user account operations
type AccountHandler struct {
	DB     *database.DBClient
	Config *config.Config
}

// NewAccountHandler creates a new instance of AccountHandler
func NewAccountHandler(db *database.DBClient, cfg *config.Config) *AccountHandler {
	return &AccountHandler{
		DB:     db,
		Config: cfg,
	}
}

// GetAccountOverview returns an overview of the user's account
func (h *AccountHandler) GetAccountOverview(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get user info from token
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	// Get base user data
	var userData models.User
	userCollection := h.DB.Collections().Users
	err := userCollection.FindOne(ctx, bson.M{"_id": user.UserID}).Decode(&userData)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "User not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve user data",
			"error":   err.Error(),
		})
	}

	// Get profile data
	var profile models.UserProfile
	profileCollection := h.DB.Collections().UserProfiles
	err = profileCollection.FindOne(ctx, bson.M{"user_id": user.UserID}).Decode(&profile)
	// It's okay if profile doesn't exist yet

	// Get wishlist count
	wishlistCollection := h.DB.Collections().Wishlists
	wishlistCount, _ := wishlistCollection.CountDocuments(ctx, bson.M{"user_id": user.UserID})

	// Get order count
	orderCollection := h.DB.Collections().Orders
	orderCount, _ := orderCollection.CountDocuments(ctx, bson.M{"user_id": user.UserID})

	// Get review count
	reviewCollection := h.DB.Collections().Reviews
	reviewCount, _ := reviewCollection.CountDocuments(ctx, bson.M{"user_id": user.UserID})

	// Build response
	response := fiber.Map{
		"profile": fiber.Map{
			"id":        userData.ID,
			"name":      userData.Name,
			"email":     userData.Email,
			"role":      userData.Role,
			"createdAt": userData.CreatedAt,
		},
		"counts": fiber.Map{
			"wishlist": wishlistCount,
			"orders":   orderCount,
			"reviews":  reviewCount,
		},
	}

	if err != mongo.ErrNoDocuments {
		response["profile"].(fiber.Map)["dateOfBirth"] = profile.DateOfBirth
		response["profile"].(fiber.Map)["gender"] = profile.Gender
		response["profile"].(fiber.Map)["phone"] = profile.Phone
		response["profile"].(fiber.Map)["avatarUrl"] = profile.AvatarURL
		response["profile"].(fiber.Map)["bio"] = profile.Bio
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Account overview retrieved successfully",
		"data":    response,
	})
}

// GetAccountReviews retrieves all reviews by the current user
func (h *AccountHandler) GetAccountReviews(c *fiber.Ctx) error {
	// We can reuse the existing ReviewHandler's GetUserReviews method
	reviewHandler := NewReviewHandler(h.DB, h.Config)
	return reviewHandler.GetUserReviews(c)
}

// DeleteAccountReview deletes a review by the current user
func (h *AccountHandler) DeleteAccountReview(c *fiber.Ctx) error {
	// We can reuse the existing ReviewHandler's DeleteReview method
	reviewHandler := NewReviewHandler(h.DB, h.Config)
	return reviewHandler.DeleteReview(c)
}

// GetAccountWishlist retrieves the wishlist for the current user
func (h *AccountHandler) GetAccountWishlist(c *fiber.Ctx) error {
	// We can reuse the existing WishlistHandler's GetWishlist method
	wishlistHandler := NewWishlistHandler(h.DB, h.Config)
	return wishlistHandler.GetWishlist(c)
}

// RemoveAccountWishlistItem removes an item from the current user's wishlist
func (h *AccountHandler) RemoveAccountWishlistItem(c *fiber.Ctx) error {
	// We can reuse the existing WishlistHandler's RemoveFromWishlist method
	wishlistHandler := NewWishlistHandler(h.DB, h.Config)
	return wishlistHandler.RemoveFromWishlist(c)
}

// GetAccountOrders retrieves all orders for the current user
func (h *AccountHandler) GetAccountOrders(c *fiber.Ctx) error {

	// Get user info from token
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	// Set the userID param for the handler (Fiber doesn't allow setting Params directly, but you can provide a default value)
	// This will make c.Params("userID") return user.UserID.Hex() if not set
	c.Params("userID", user.UserID.Hex())

	// Use the existing OrderHandler's GetOrders method. No shipping service is
	// passed: this is a read path and never books, cancels or tracks anything.
	orderHandler := NewOrderHandler(h.DB, h.Config, nil)
	return orderHandler.GetOrders(c)
}

// GetAccountOrder retrieves a specific order for the current user
func (h *AccountHandler) GetAccountOrder(c *fiber.Ctx) error {
	// We can reuse the existing OrderHandler's GetOrder method
	// It already checks if the user is authorized to view the order.
	// Read-only, so no shipping service is required.
	orderHandler := NewOrderHandler(h.DB, h.Config, nil)
	return orderHandler.GetOrder(c)
}

// UpdateAccountProfile updates the current user's profile
func (h *AccountHandler) UpdateAccountProfile(c *fiber.Ctx) error {
	// We can reuse the existing UserProfileHandler's UpdateProfile method
	profileHandler := NewUserProfileHandler(h.DB, h.Config)
	return profileHandler.UpdateProfile(c)
}
