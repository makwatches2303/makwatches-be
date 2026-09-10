package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// PaymentHandler provides endpoints for initiating payments (Razorpay order creation)
type PaymentHandler struct {
	DB  *database.DBClient
	Cfg *config.Config
}

func NewPaymentHandler(db *database.DBClient, cfg *config.Config) *PaymentHandler {
	return &PaymentHandler{DB: db, Cfg: cfg}
}

// cartTotalINR computes the current cart total for a user
func (h *PaymentHandler) cartTotalINR(userID any) (float64, error) {
	ctx := context.Background()
	cartCol := h.DB.Collections().CartItems
	prodCol := h.DB.Collections().Products
	heroCol := h.DB.MongoDB.Collection("hero_slides")
	collectionCol := h.DB.MongoDB.Collection("home_collection_features")

	cursor, err := cartCol.Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	type itemRow struct {
		ProductID interface{} `bson:"product_id"`
		Quantity  int         `bson:"quantity"`
	}
	var rows []itemRow
	if err := cursor.All(ctx, &rows); err != nil {
		return 0, err
	}
	total := 0.0
	for _, r := range rows {
		// Try regular products first
		var p models.Product
		err := prodCol.FindOne(ctx, bson.M{"_id": r.ProductID}).Decode(&p)
		if err == nil {
			// Found in regular products
			if p.Stock < r.Quantity {
				return 0, fmt.Errorf("insufficient stock for product: %s", p.Name)
			}
			// Use discounted final price if active
			unit := p.GetFinalPrice()
			total += unit * float64(r.Quantity)
		} else {
			// Try to find in home content by productId
			var heroSlide models.HeroSlide
			err = heroCol.FindOne(ctx, bson.M{"productId": r.ProductID}).Decode(&heroSlide)
			if err == nil {
				// Found in hero slides - parse price
				priceFloat := 0.0
				fmt.Sscanf(heroSlide.Price, "₹%f", &priceFloat)
				if priceFloat == 0 {
					fmt.Sscanf(heroSlide.Price, "%f", &priceFloat)
				}
				total += priceFloat * float64(r.Quantity)
			} else {
				// Try collection features
				var collectionFeature models.HomeCollectionFeature
				err = collectionCol.FindOne(ctx, bson.M{"productId": r.ProductID}).Decode(&collectionFeature)
				if err == nil {
					// Found in collection features - parse price
					priceFloat := 0.0
					if collectionFeature.Price != "" {
						fmt.Sscanf(collectionFeature.Price, "₹%f", &priceFloat)
						if priceFloat == 0 {
							fmt.Sscanf(collectionFeature.Price, "%f", &priceFloat)
						}
					}
					total += priceFloat * float64(r.Quantity)
				} else {
					// Product not found in any collection
					return 0, fmt.Errorf("product not found in cart")
				}
			}
		}
	}
	return total, nil
}

// CreateRazorpayOrder creates a Razorpay order from cart total
func (h *PaymentHandler) CreateRazorpayOrder(c *fiber.Ctx) error {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "message": "Unauthorized"})
	}

	if h.Cfg.RazorpayKey == "" || h.Cfg.RazorpaySecret == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "message": "Payment gateway not configured"})
	}
	total, err := h.cartTotalINR(user.UserID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": err.Error()})
	}
	if total <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Cart empty"})
	}

	amountPaise := int64(math.Round(total * 100))
	rnd := make([]byte, 6)
	rand.Read(rnd)
	receipt := fmt.Sprintf("rcpt_%s", hex.EncodeToString(rnd))

	payload := map[string]any{"amount": amountPaise, "currency": "INR", "receipt": receipt, "payment_capture": 1}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.razorpay.com/v1/orders", bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(h.Cfg.RazorpayKey, h.Cfg.RazorpaySecret)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"success": false, "message": "Failed to create payment order", "error": err.Error()})
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return c.Status(resp.StatusCode).JSON(fiber.Map{"success": false, "message": "Gateway error", "raw": string(body)})
	}

	var rzpOrder map[string]any
	_ = json.Unmarshal(body, &rzpOrder)
	orderID, _ := rzpOrder["id"].(string)

	return c.JSON(fiber.Map{
		"success":  true,
		"key":      h.Cfg.RazorpayKey,
		"amount":   amountPaise,
		"currency": "INR",
		"orderId":  orderID,
		"data": fiber.Map{
			"key":      h.Cfg.RazorpayKey,
			"amount":   amountPaise,
			"currency": "INR",
			"orderId":  orderID,
			"id":       orderID,
			"order":    rzpOrder,
		},
	})
}

// RazorpayWebhook validates webhook signatures from Razorpay
// Set the endpoint URL in Razorpay dashboard and use Cfg.RazorpayWebhookSecret
func (h *PaymentHandler) RazorpayWebhook(c *fiber.Ctx) error {
	if h.Cfg.RazorpayWebhookSecret == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "message": "Webhook secret not configured"})
	}

	sig := c.Get("X-Razorpay-Signature")
	if sig == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Missing signature"})
	}

	body := c.Body()
	mac := hmac.New(sha256.New, []byte(h.Cfg.RazorpayWebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Invalid webhook signature"})
	}

	// Parse event (optional minimal handling)
	var evt map[string]any
	if err := json.Unmarshal(body, &evt); err == nil {
		// You can extend: update order/payment status based on event
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true})
}
