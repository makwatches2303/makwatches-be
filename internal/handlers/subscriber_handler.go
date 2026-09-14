package handlers

import (
	"context"
	"fmt"
	"log"
	"net/mail"
	"strings"
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

// SubscriberHandler handles WhatsApp lead capture and marketing subscriptions
type SubscriberHandler struct {
	DB       *database.DBClient
	Config   *config.Config
	WhatsApp *whatsapp.Client
}

// NewSubscriberHandler creates a new SubscriberHandler instance
func NewSubscriberHandler(db *database.DBClient, cfg *config.Config, wa *whatsapp.Client) *SubscriberHandler {
	return &SubscriberHandler{
		DB:       db,
		Config:   cfg,
		WhatsApp: wa,
	}
}

// SubscribeEmail records an email address from the newsletter block.
//
// Deliberately separate from SubscribeWhatsApp, and deliberately silent: it
// stores the address and returns. No WhatsApp template is dispatched, because
// the visitor gave an email and never consented to a message on their phone --
// and in the ordinary case there is no phone number on the record to send one
// to.
//
// The upsert is keyed on the normalized email, so a visitor who has already
// subscribed by phone keeps that number, and re-submitting the same address is
// idempotent rather than creating duplicates.
//
// The response is intentionally identical whether or not the address was
// already on the list. Reporting "you are already subscribed" turns a public,
// unauthenticated endpoint into an oracle that confirms whether a given address
// is a customer.
func (h *SubscriberHandler) SubscribeEmail(c *fiber.Ctx) error {
	var req models.SubscribeEmailRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
		})
	}

	email, err := normalizeEmail(req.Email)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Please enter a valid email address.",
		})
	}

	source := req.Source
	if source == "" {
		source = "newsletter"
	}

	now := time.Now()
	update := bson.M{
		"$set": bson.M{
			"email":      email,
			"source":     source,
			"updated_at": now,
		},
		"$setOnInsert": bson.M{
			"created_at":   now,
			"welcome_sent": false,
		},
	}
	// Name only when one was given: an empty value would wipe a name captured
	// earlier through the phone popup.
	if strings.TrimSpace(req.Name) != "" {
		update["$set"].(bson.M)["name"] = strings.TrimSpace(req.Name)
	}

	_, err = h.DB.Collections().Subscribers.UpdateOne(
		c.Context(),
		bson.M{"email": email},
		update,
		options.Update().SetUpsert(true),
	)
	if err != nil {
		// The address is not logged: it is personal data, and a failure here is
		// actionable without it.
		log.Printf("[SUBSCRIBER] Error saving email subscriber: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Could not save your subscription. Please try again.",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "You are on the list.",
	})
}

// normalizeEmail validates an address and lowercases it for use as a key.
//
// mail.ParseAddress also accepts display-name forms ("A <a@b.c>"), which must
// not become a subscriber key, so the parsed address is required to match the
// trimmed input.
func normalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("email is required")
	}

	parsed, err := mail.ParseAddress(trimmed)
	if err != nil || !strings.EqualFold(parsed.Address, trimmed) {
		return "", fmt.Errorf("invalid email address")
	}
	if !strings.Contains(parsed.Address, ".") {
		return "", fmt.Errorf("invalid email address")
	}

	return strings.ToLower(parsed.Address), nil
}

// SubscribeWhatsApp handles user subscribing their phone number from the website popup
func (h *SubscriberHandler) SubscribeWhatsApp(c *fiber.Ctx) error {
	var req models.SubscribeRequest
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

	source := req.Source
	if source == "" {
		source = "popup"
	}

	now := time.Now()
	collection := h.DB.Collections().Subscribers

	// Check if this subscriber already exists
	var existing models.Subscriber
	err = collection.FindOne(c.Context(), bson.M{"phone": normalizedPhone}).Decode(&existing)
	isNew := err == mongo.ErrNoDocuments

	update := bson.M{
		"$set": bson.M{
			"phone":      normalizedPhone,
			"name":       req.Name,
			"source":     source,
			"updated_at": now,
		},
		"$setOnInsert": bson.M{
			"created_at":   now,
			"welcome_sent": false,
		},
	}

	opts := options.Update().SetUpsert(true)
	_, err = collection.UpdateOne(c.Context(), bson.M{"phone": normalizedPhone}, update, opts)
	if err != nil {
		log.Printf("[SUBSCRIBER] Error saving subscriber %s: %v", normalizedPhone, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to save subscription",
		})
	}

	// If subscriber is new or has not received the welcome template yet, send it
	if isNew || !existing.WelcomeSent {
		go func(phone, name string) {
			sendCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// Check if welcome flow is enabled and load active preset
			var ws models.WhatsAppFlowSettings
			if err := h.DB.MongoDB.Collection(WhatsAppSettingsCollection).FindOne(sendCtx, bson.M{}).Decode(&ws); err == nil {
				if !ws.WelcomeEnabled {
					log.Printf("[SUBSCRIBER] Welcome flow is disabled in settings; skipping message to %s", phone)
					return
				}
				if ws.WelcomeTemplate != "" || ws.WelcomeImageURL != "" {
					h.WhatsApp.UpdatePresets(ws.WelcomeTemplate, ws.AbandonedCartTemplate, ws.WelcomeImageURL)
				}
			}

			if err := h.WhatsApp.SendWelcomeTemplate(sendCtx, phone, name); err != nil {
				log.Printf("[SUBSCRIBER] Warning: Failed to send WhatsApp welcome to %s: %v", phone, err)
			} else {
				// Mark welcome as sent
				_, _ = collection.UpdateOne(
					context.Background(),
					bson.M{"phone": phone},
					bson.M{"$set": bson.M{"welcome_sent": true, "updated_at": time.Now()}},
				)
				log.Printf("[SUBSCRIBER] Welcome template successfully delivered and marked sent for %s", phone)
			}
		}(normalizedPhone, req.Name)
	}

	return c.Status(fiber.StatusOK).JSON(models.SubscribeResponse{
		Success: true,
		Message: "Thank you for subscribing to MAK Watches VIP circle! Check your WhatsApp for a welcome message.",
		Phone:   normalizedPhone,
	})
}

// ListSubscribers handles admin listing of captured subscribers
func (h *SubscriberHandler) ListSubscribers(c *fiber.Ctx) error {
	collection := h.DB.Collections().Subscribers
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100)

	cursor, err := collection.Find(c.Context(), bson.M{}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch subscribers",
		})
	}
	defer cursor.Close(c.Context())

	var subscribers []models.Subscriber
	if err := cursor.All(c.Context(), &subscribers); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode subscribers",
		})
	}

	if subscribers == nil {
		subscribers = []models.Subscriber{}
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    subscribers,
		"count":   len(subscribers),
	})
}
