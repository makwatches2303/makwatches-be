package handlers

import (
	"context"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"

	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// Backs the admin dashboard's Security settings tab, which previously said
// "Coming soon" for exactly these two things: changing your own password,
// and seeing recent admin sign-ins.
//
// adminLoginAuditCollection is its own small collection, not part of
// database.DBClient.Collections() -- it's used from exactly one handler
// (this one), the same way admin_account_handler.go reaches
// h.DB.MongoDB.Collection("users") directly rather than growing the shared
// accessor for a single caller.
const adminLoginAuditCollection = "admin_login_audit"

// adminLoginAudit is one row of the security tab's "recent sign-ins" list.
type adminLoginAudit struct {
	ID        primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	UserID    primitive.ObjectID `json:"userId" bson:"user_id"`
	Email     string             `json:"email" bson:"email"`
	IP        string             `json:"ip" bson:"ip"`
	UserAgent string             `json:"userAgent" bson:"user_agent"`
	CreatedAt time.Time          `json:"createdAt" bson:"created_at"`
}

// recordAdminLoginAudit inserts one audit row for a successful admin login.
// Best-effort and fire-and-forget from the caller's perspective: a login
// that already succeeded must not fail because this write did.
func recordAdminLoginAudit(ctx context.Context, db *database.DBClient, userID primitive.ObjectID, email string, c *fiber.Ctx) {
	_, _ = db.MongoDB.Collection(adminLoginAuditCollection).InsertOne(ctx, adminLoginAudit{
		ID:        primitive.NewObjectID(),
		UserID:    userID,
		Email:     email,
		IP:        c.IP(),
		UserAgent: c.Get("User-Agent"),
		CreatedAt: time.Now(),
	})
}

// ChangePassword lets the signed-in admin change their own password.
// PUT /admin/security/password
// { "currentPassword": "...", "newPassword": "..." }
func (h *AuthHandler) ChangePassword(c *fiber.Ctx) error {
	ctx := c.Context()

	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized",
		})
	}

	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}
	if len(req.NewPassword) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "New password must be at least 6 characters",
		})
	}

	userCollection := h.DB.Collections().Users
	var user models.User
	if err := userCollection.FindOne(ctx, bson.M{"_id": tokenUser.UserID}).Decode(&user); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Account not found",
		})
	}

	if user.AuthProvider == "google" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "This account signs in with Google -- there's no password to change here.",
		})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.CurrentPassword)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Current password is incorrect",
		})
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to hash new password",
			"error":   err.Error(),
		})
	}

	_, err = userCollection.UpdateOne(ctx, bson.M{"_id": tokenUser.UserID}, bson.M{"$set": bson.M{
		"password":   string(hashed),
		"updated_at": time.Now(),
	}})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to update password",
			"error":   err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Password updated successfully",
	})
}

// GetLoginActivity returns recent admin sign-ins for the security tab.
// GET /admin/security/login-activity
func (h *AuthHandler) GetLoginActivity(c *fiber.Ctx) error {
	ctx := c.Context()

	limit, err := strconv.Atoi(c.Query("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		limit = 20
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(int64(limit))
	cursor, err := h.DB.MongoDB.Collection(adminLoginAuditCollection).Find(ctx, bson.M{}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch login activity",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	entries := []adminLoginAudit{}
	if err := cursor.All(ctx, &entries); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode login activity",
			"error":   err.Error(),
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"data":    entries,
	})
}
