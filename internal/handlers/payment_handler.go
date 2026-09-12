package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// PaymentHandler provides endpoints for initiating payments (Razorpay order creation)
type PaymentHandler struct {
	DB  *database.DBClient
	Cfg *config.Config
	// Shipping resolves the selected delivery quote so the payment intent is
	// raised for the same total checkout will compute.
	Shipping *shipping.Service
}

func NewPaymentHandler(db *database.DBClient, cfg *config.Config, shippingSvc *shipping.Service) *PaymentHandler {
	return &PaymentHandler{DB: db, Cfg: cfg, Shipping: shippingSvc}
}

// NOTE: a second cart-pricing implementation (cartTotalINR) lived here and
// has been removed. It priced items checkout refuses -- entries found only in
// hero_slides or home_collection_features -- so the payment intent could be
// raised for a cart checkout would then reject. With the captured amount now
// verified against the order total, two pricing paths is a correctness bug,
// not untidiness: both sides go through cartComposition.

// CreateRazorpayOrder creates a Razorpay order from cart total
func (h *PaymentHandler) CreateRazorpayOrder(c *fiber.Ctx) error {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "message": "Unauthorized"})
	}

	if h.Cfg.RazorpayKey == "" || h.Cfg.RazorpaySecret == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "message": "Payment gateway not configured"})
	}

	// The intent amount must equal what checkout will compute, because checkout
	// now verifies the captured amount against its own total. Both sides price
	// the cart through cartComposition and add the same validated delivery
	// charge; if they disagreed, every legitimate payment would be refused.
	ctx := c.UserContext()
	lines, subtotal, err := cartComposition(ctx, h.DB, user.UserID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": err.Error()})
	}
	if len(lines) == 0 || subtotal <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Cart empty"})
	}

	var reqBody struct {
		CouponCode    string `json:"couponCode"`
		ShippingQuote string `json:"shippingQuote"`
		Pincode       string `json:"pincode"`
		COD           bool   `json:"cod"`
	}
	_ = c.BodyParser(&reqBody)

	total := subtotal
	if strings.TrimSpace(reqBody.CouponCode) != "" {
		code := strings.ToUpper(strings.TrimSpace(reqBody.CouponCode))
		var coupon models.Coupon
		if err := h.DB.Collections().Coupons.FindOne(c.Context(), bson.M{"code": code}).Decode(&coupon); err == nil {
			if disc, err := coupon.CalculateDiscount(subtotal); err == nil {
				total = math.Max(0, total-disc)
			}
		}
	}

	if token := strings.TrimSpace(reqBody.ShippingQuote); token != "" && h.Shipping != nil {
		pkg := h.Shipping.DefaultPackage()
		verified, qErr := h.Shipping.Quoter().Verify(token, shipping.QuoteBinding{
			UserID:      user.UserID.Hex(),
			CartHash:    shipping.CartFingerprint(lines),
			Pincode:     strings.TrimSpace(reqBody.Pincode),
			WeightGrams: pkg.WeightGrams,
			COD:         reqBody.COD,
		}, time.Now())
		if qErr != nil {
			se := shipping.AsError(qErr)
			log.Printf("[PAYMENT] rejected shipping quote for user %s: %s", user.UserID.Hex(), se.Detail)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"code":    string(se.Code),
				"message": "Your delivery option is no longer valid. Please choose it again.",
			})
		}
		total += verified.Charge
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
