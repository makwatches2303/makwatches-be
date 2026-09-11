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
