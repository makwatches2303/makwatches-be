package handlers

import (
	"context"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
)

const WhatsAppSettingsCollection = "whatsapp_settings"

// WhatsAppSettingsHandler handles admin flow presetting and FlowSell linking
type WhatsAppSettingsHandler struct {
	DB       *database.DBClient
	Config   *config.Config
	WhatsApp *whatsapp.Client
}

// NewWhatsAppSettingsHandler creates a new instance of WhatsAppSettingsHandler
func NewWhatsAppSettingsHandler(db *database.DBClient, cfg *config.Config, wa *whatsapp.Client) *WhatsAppSettingsHandler {
	return &WhatsAppSettingsHandler{
		DB:       db,
		Config:   cfg,
		WhatsApp: wa,
	}
}

// GetSettings fetches the current flow presets
func (h *WhatsAppSettingsHandler) GetSettings(c *fiber.Ctx) error {
	ctx := c.Context()
	collection := h.DB.MongoDB.Collection(WhatsAppSettingsCollection)

	var settings models.WhatsAppFlowSettings
	err := collection.FindOne(ctx, bson.M{}).Decode(&settings)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			defaults := models.DefaultWhatsAppFlowSettings()
			if h.Config.WhatsAppPhoneNumberID != "" {
				defaults.PhoneNumberID = h.Config.WhatsAppPhoneNumberID
			}
			if h.Config.WhatsAppWelcomeTemplate != "" {
				defaults.WelcomeTemplate = h.Config.WhatsAppWelcomeTemplate
			}
			if h.Config.WhatsAppCartTemplate != "" {
				defaults.AbandonedCartTemplate = h.Config.WhatsAppCartTemplate
			}
			if h.Config.WhatsAppWelcomeImageURL != "" {
				defaults.WelcomeImageURL = h.Config.WhatsAppWelcomeImageURL
			}
			return c.JSON(fiber.Map{
				"success": true,
				"data":    defaults,
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve WhatsApp flow settings",
			"error":   err.Error(),
		})
	}

	// Always sync phone ID and dashboard URL from config if missing in document
	if settings.PhoneNumberID == "" && h.Config.WhatsAppPhoneNumberID != "" {
		settings.PhoneNumberID = h.Config.WhatsAppPhoneNumberID
	}
	if settings.FlowSellDashboardURL == "" {
		settings.FlowSellDashboardURL = "https://connect.flowsell.in"
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    settings,
	})
}

// UpdateSettings updates the flow presets in MongoDB
func (h *WhatsAppSettingsHandler) UpdateSettings(c *fiber.Ctx) error {
	var req models.UpdateWhatsAppFlowSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	setDoc := bson.M{
		"updated_at": time.Now(),
	}

	if req.WelcomeEnabled != nil {
		setDoc["welcome_enabled"] = *req.WelcomeEnabled
	}
	if req.WelcomeTemplate != nil {
		setDoc["welcome_template"] = *req.WelcomeTemplate
	}
	if req.WelcomeImageURL != nil {
		setDoc["welcome_image_url"] = *req.WelcomeImageURL
	}
	if req.OrderConfirmationEnabled != nil {
		setDoc["order_confirmation_enabled"] = *req.OrderConfirmationEnabled
	}
	if req.OrderConfirmationTemplate != nil {
		setDoc["order_confirmation_template"] = *req.OrderConfirmationTemplate
	}
	if req.DeliveryUpdatesEnabled != nil {
		setDoc["delivery_updates_enabled"] = *req.DeliveryUpdatesEnabled
	}
	if req.DeliveryUpdatesTemplate != nil {
		setDoc["delivery_updates_template"] = *req.DeliveryUpdatesTemplate
	}
	if req.AbandonedCartEnabled != nil {
		setDoc["abandoned_cart_enabled"] = *req.AbandonedCartEnabled
	}
	if req.AbandonedCartTemplate != nil {
		setDoc["abandoned_cart_template"] = *req.AbandonedCartTemplate
	}

	collection := h.DB.MongoDB.Collection(WhatsAppSettingsCollection)
	opts := options.Update().SetUpsert(true)
	_, err := collection.UpdateOne(c.Context(), bson.M{}, bson.M{"$set": setDoc}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to save WhatsApp flow settings",
			"error":   err.Error(),
		})
	}

	// Update in-memory WhatsApp client
	welcomeTpl := ""
	if req.WelcomeTemplate != nil {
		welcomeTpl = *req.WelcomeTemplate
	}
	cartTpl := ""
	if req.AbandonedCartTemplate != nil {
		cartTpl = *req.AbandonedCartTemplate
	}
	welcomeImg := ""
	if req.WelcomeImageURL != nil {
		welcomeImg = *req.WelcomeImageURL
	}
	h.WhatsApp.UpdatePresets(welcomeTpl, cartTpl, welcomeImg)

	log.Printf("[WHATSAPP_SETTINGS] Settings updated successfully by admin")
	return h.GetSettings(c)
}

// SendTest sends a test WhatsApp message to the specified number
func (h *WhatsAppSettingsHandler) SendTest(c *fiber.Ctx) error {
	var req models.WhatsAppTestRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	if req.Phone == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Phone number is required",
		})
	}

	normalizedPhone, err := whatsapp.NormalizePhoneNumber(req.Phone)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid phone number: " + err.Error(),
		})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
	defer cancel()

	flow := req.Flow
	if flow == "" {
		flow = "welcome"
	}

	log.Printf("[WHATSAPP_TEST] Sending test message for flow '%s' to %s", flow, normalizedPhone)

	switch flow {
	case "cart":
		err = h.WhatsApp.SendAbandonedCartTemplate(ctx, normalizedPhone, "Admin Tester", "Rolex Submariner Date")
	case "order":
		msg := "MAK Watches: Test Order Confirmation ✅\n\nOrder MAK-TEST-001 has been confirmed! Total: ₹4,249. We are preparing your shipment with utmost care."
		err = h.WhatsApp.SendTextMessage(ctx, normalizedPhone, msg)
	case "delivery":
		msg := "MAK Watches: Test Delivery Update 🚚\n\nYour order MAK-TEST-001 is out for delivery with Delhivery. Expected arrival today!"
		err = h.WhatsApp.SendTextMessage(ctx, normalizedPhone, msg)
	case "welcome":
		fallthrough
	default:
		err = h.WhatsApp.SendWelcomeTemplate(ctx, normalizedPhone, "Admin Tester")
	}

	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success": false,
			"message": "Failed to send test message: " + err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Test message dispatched successfully to " + normalizedPhone,
	})
}
