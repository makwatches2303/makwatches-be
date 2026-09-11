package handlers

import (
	"context"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
)

// CartTrackerHandler handles cart activity recording and abandoned cart reminders
type CartTrackerHandler struct {
	DB       *database.DBClient
	Config   *config.Config
	WhatsApp *whatsapp.Client
}

// NewCartTrackerHandler creates a new CartTrackerHandler
func NewCartTrackerHandler(db *database.DBClient, cfg *config.Config, wa *whatsapp.Client) *CartTrackerHandler {
	return &CartTrackerHandler{
		DB:       db,
		Config:   cfg,
		WhatsApp: wa,
	}
}

// TrackCart receives cart updates with customer phone to monitor for cart abandonment
func (h *CartTrackerHandler) TrackCart(c *fiber.Ctx) error {
	var req models.TrackCartRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
		})
	}

	if req.Phone == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Phone number is required for tracking",
		})
	}

	normalizedPhone, err := whatsapp.NormalizePhoneNumber(req.Phone)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid phone format: " + err.Error(),
		})
	}

	if len(req.Items) == 0 {
		// Empty cart, mark any existing active cart as recovered/cleared
		_, _ = h.DB.Collections().AbandonedCarts.UpdateMany(
			c.Context(),
			bson.M{"phone": normalizedPhone, "status": "active"},
			bson.M{"$set": bson.M{"status": "cleared", "updated_at": time.Now()}},
		)
		return c.JSON(fiber.Map{"success": true, "message": "Cart cleared"})
	}

	now := time.Now()
	collection := h.DB.Collections().AbandonedCarts

	update := bson.M{
		"$set": bson.M{
			"phone":            normalizedPhone,
			"customer_name":    req.CustomerName,
			"cart_token":       req.CartToken,
			"items":            req.Items,
			"total":            req.Total,
			"status":           "active",
			"reminder_sent":    false,
			"last_activity_at": now,
			"updated_at":       now,
		},
		"$setOnInsert": bson.M{
			"created_at": now,
		},
	}

	opts := options.Update().SetUpsert(true)
	_, err = collection.UpdateOne(c.Context(), bson.M{"phone": normalizedPhone, "status": "active"}, update, opts)
	if err != nil {
		log.Printf("[CART_TRACKER] Error upserting tracked cart for %s: %v", normalizedPhone, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to track cart",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Cart activity recorded successfully",
	})
}

// CheckAndSendAbandonedCartReminders queries carts that have been inactive and dispatches WhatsApp reminders
func (h *CartTrackerHandler) CheckAndSendAbandonedCartReminders(c *fiber.Ctx) error {
	count, err := h.ProcessAbandonedCarts(c.Context(), 15*time.Minute)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to process abandoned carts: " + err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success":        true,
		"reminders_sent": count,
	})
}

// ProcessAbandonedCarts scans for inactive carts and sends reminders
func (h *CartTrackerHandler) ProcessAbandonedCarts(ctx context.Context, inactivityThreshold time.Duration) (int, error) {
	collection := h.DB.Collections().AbandonedCarts
	cutoffTime := time.Now().Add(-inactivityThreshold)
	maxAge := time.Now().Add(-48 * time.Hour) // Don't remind carts older than 48 hours

	filter := bson.M{
		"status":           "active",
		"reminder_sent":    false,
		"last_activity_at": bson.M{"$lte": cutoffTime, "$gte": maxAge},
	}

	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)

	var abandonedCarts []models.AbandonedCart
	if err := cursor.All(ctx, &abandonedCarts); err != nil {
		return 0, err
	}

	sentCount := 0
	for _, cart := range abandonedCarts {
		if len(cart.Items) == 0 {
			continue
		}

		productName := cart.Items[0].Name
		if len(cart.Items) > 1 {
			productName = productName + " and more"
		}

		sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := h.WhatsApp.SendAbandonedCartTemplate(sendCtx, cart.Phone, cart.CustomerName, productName)
		cancel()

		now := time.Now()
		if err != nil {
			log.Printf("[ABANDONED_CART] Failed to send reminder to %s: %v", cart.Phone, err)
		} else {
			sentCount++
			_, _ = collection.UpdateOne(
				ctx,
				bson.M{"_id": cart.ID},
				bson.M{
					"$set": bson.M{
						"status":           "reminded",
						"reminder_sent":    true,
						"reminder_sent_at": now,
						"updated_at":       now,
					},
				},
			)
			log.Printf("[ABANDONED_CART] Sent cart recovery reminder to %s for product '%s'", cart.Phone, productName)
		}
	}

	return sentCount, nil
}

// StartAbandonedCartWorker starts a background goroutine that checks for abandoned carts every interval
func (h *CartTrackerHandler) StartAbandonedCartWorker(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		log.Printf("[ABANDONED_CART] Background worker started, checking every %v", interval)
		for {
			select {
			case <-ctx.Done():
				log.Println("[ABANDONED_CART] Background worker stopped")
				return
			case <-ticker.C:
				count, err := h.ProcessAbandonedCarts(context.Background(), 15*time.Minute)
				if err != nil {
					log.Printf("[ABANDONED_CART] Worker scan error: %v", err)
				} else if count > 0 {
					log.Printf("[ABANDONED_CART] Worker sent %d reminder(s)", count)
				}
			}
		}
	}()
}
