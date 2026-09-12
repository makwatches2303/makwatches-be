package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SettingsHandler handles settings related operations
type SettingsHandler struct {
	DB       *mongo.Database
	Firebase *firebase.Provider
}

// NewSettingsHandler creates a new settings handler
func NewSettingsHandler(db *mongo.Database, fb *firebase.Provider) *SettingsHandler {
	return &SettingsHandler{
		DB:       db,
		Firebase: fb,
	}
}

// GetSettings retrieves the current system settings
func (h *SettingsHandler) GetSettings() fiber.Handler {
	return func(c *fiber.Ctx) error {
		collection := h.DB.Collection("settings")
		ctx := c.Context()

		// We'll always have just one settings document with a known ID
		// If it doesn't exist yet, we'll return default settings
		var settings models.Settings

		// Try to find existing settings
		err := collection.FindOne(ctx, bson.M{}).Decode(&settings)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				// Return default settings if none exist
				defaultSettings := models.Settings{
					StoreName:          "Makwatches",
					StoreDescription:   "Your fashion destination",
					Currency:           "INR",
					TaxRate:            18.0, // Default GST in India
					EnableRegistration: true,
					MaintenanceMode:    false,
					CreatedAt:          time.Now(),
					UpdatedAt:          time.Now(),
				}
				return c.Status(fiber.StatusOK).JSON(fiber.Map{
					"success": true,
					"data":    defaultSettings,
				})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Error retrieving settings",
				"error":   err.Error(),
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"data":    settings,
		})
	}
}

// UpdateSettings updates the system settings
func (h *SettingsHandler) UpdateSettings() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Parse the update request
		var updateRequest models.UpdateSettingsRequest
		if err := c.BodyParser(&updateRequest); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "Invalid request data",
				"error":   err.Error(),
			})
		}

		collection := h.DB.Collection("settings")
		ctx := c.Context()

		// Create an update document with the fields to update
		update := bson.M{
			"$set": bson.M{
				"updated_at": time.Now(),
			},
		}

		// Add fields to update only if they're provided
		updateSet := update["$set"].(bson.M)

		if updateRequest.StoreName != nil {
			updateSet["store_name"] = *updateRequest.StoreName
		}
		if updateRequest.StoreDescription != nil {
			updateSet["store_description"] = *updateRequest.StoreDescription
		}
		if updateRequest.ContactEmail != nil {
			updateSet["contact_email"] = *updateRequest.ContactEmail
		}
		if updateRequest.ContactPhone != nil {
			updateSet["contact_phone"] = *updateRequest.ContactPhone
		}
		if updateRequest.Address != nil {
			updateSet["address"] = *updateRequest.Address
		}
		if updateRequest.Currency != nil {
			updateSet["currency"] = *updateRequest.Currency
		}
		if updateRequest.TaxRate != nil {
			updateSet["tax_rate"] = *updateRequest.TaxRate
		}
		if len(updateRequest.ShippingMethods) > 0 {
			updateSet["shipping_methods"] = updateRequest.ShippingMethods
		}
		if len(updateRequest.PaymentGateways) > 0 {
			updateSet["payment_gateways"] = updateRequest.PaymentGateways
		}
		if updateRequest.SocialMedia != nil {
			updateSet["social_media"] = updateRequest.SocialMedia
		}
		if updateRequest.PrivacyPolicy != nil {
			updateSet["privacy_policy"] = *updateRequest.PrivacyPolicy
		}
		if updateRequest.TermsOfService != nil {
			updateSet["terms_of_service"] = *updateRequest.TermsOfService
		}
		if updateRequest.RefundPolicy != nil {
			updateSet["refund_policy"] = *updateRequest.RefundPolicy
		}
		if updateRequest.EnableRegistration != nil {
			updateSet["enable_registration"] = *updateRequest.EnableRegistration
		}
		if updateRequest.MaintenanceMode != nil {
			updateSet["maintenance_mode"] = *updateRequest.MaintenanceMode
		}

		// Find one and update (or insert if not exists)
		opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
		var updatedSettings models.Settings
		err := collection.FindOneAndUpdate(
			ctx,
			bson.M{}, // Empty filter to match any document
			update,
			opts,
		).Decode(&updatedSettings)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Error updating settings",
				"error":   err.Error(),
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Settings updated successfully",
			"data":    updatedSettings,
		})
	}
}

// UploadLogo handles logo image uploads
func (h *SettingsHandler) UploadLogo() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Get the file from form
		file, err := c.FormFile("logo")
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "No logo file provided",
				"error":   err.Error(),
			})
		}

		// Check file type
		contentType := file.Header.Get("Content-Type")
		if contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/webp" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "Invalid file type. Only JPEG, PNG or WEBP allowed",
			})
		}

		ctx := c.Context()

		fbClient, err := h.Firebase.Client(ctx)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to init Firebase client",
				"error":   err.Error(),
			})
		}

		opened, err := file.Open()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Error reading logo file",
				"error":   err.Error(),
			})
		}
		logoURL, err := fbClient.UploadFile(ctx, opened, file.Filename)
		opened.Close()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Error uploading logo to Firebase",
				"error":   err.Error(),
			})
		}

		// Update the settings with the new logo URL
		collection := h.DB.Collection("settings")

		update := bson.M{
			"$set": bson.M{
				"logo":       logoURL,
				"updated_at": time.Now(),
			},
		}

		opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
		var updatedSettings models.Settings
		err = collection.FindOneAndUpdate(
			ctx,
			bson.M{}, // Empty filter to match any document
			update,
			opts,
		).Decode(&updatedSettings)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Error updating logo in settings",
				"error":   err.Error(),
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Logo uploaded successfully",
			"data": fiber.Map{
				"logo": logoURL,
			},
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Shipping Tier Config
// ─────────────────────────────────────────────────────────────────────────────

// GetShippingTierConfig returns the current delivery surcharge tiers.
// GET /admin/settings/shipping-tiers
func (h *SettingsHandler) GetShippingTierConfig() fiber.Handler {
	return func(c *fiber.Ctx) error {
		col := h.DB.Collection(models.ShippingTierConfigCollection)
		var cfg models.ShippingTierConfig
		err := col.FindOne(c.Context(), bson.M{}).Decode(&cfg)
		if err == mongo.ErrNoDocuments {
			cfg = models.DefaultShippingTierConfig()
		} else if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false, "message": "Could not load shipping tier config",
			})
		}
		return c.JSON(fiber.Map{"success": true, "data": cfg})
	}
}

// UpdateShippingTierConfig saves new delivery surcharge tiers.
// PUT /admin/settings/shipping-tiers
func (h *SettingsHandler) UpdateShippingTierConfig() fiber.Handler {
	return func(c *fiber.Ctx) error {
		var body struct {
			AirCharge     *float64 `json:"airCharge"`
			ExpressCharge *float64 `json:"expressCharge"`
			SurfaceCharge *float64 `json:"surfaceCharge"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false, "message": "Invalid request body",
			})
		}

		set := bson.M{"updated_at": time.Now()}
		if body.AirCharge != nil {
			set["air_charge"] = *body.AirCharge
		}
		if body.ExpressCharge != nil {
			set["express_charge"] = *body.ExpressCharge
		}
		if body.SurfaceCharge != nil {
			set["surface_charge"] = *body.SurfaceCharge
		}

		col := h.DB.Collection(models.ShippingTierConfigCollection)
		opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
		var updated models.ShippingTierConfig
		if err := col.FindOneAndUpdate(
			c.Context(), bson.M{}, bson.M{"$set": set}, opts,
		).Decode(&updated); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false, "message": "Could not save shipping tier config",
			})
		}

		return c.JSON(fiber.Map{
			"success": true,
			"message": "Shipping tier charges updated",
			"data":    updated,
		})
	}
}
