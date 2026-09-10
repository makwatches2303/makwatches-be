package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/queue"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipment"
)

// OrderHandler handles order related requests
type OrderHandler struct {
	DB               *database.DBClient
	Config           *config.Config
	DelhiveryService *services.DelhiveryService
	// ShipmentQueue is nil under the non-Lambda entrypoint (cmd/api) and in
	// local dev -- Checkout falls back to a synchronous Delhivery call.
	ShipmentQueue *queue.ShipmentQueue
}

// NewOrderHandler creates a new instance of OrderHandler
func NewOrderHandler(db *database.DBClient, cfg *config.Config, shipmentQueue *queue.ShipmentQueue) *OrderHandler {
	log.Printf("[ORDER_HANDLER] Initializing OrderHandler with Delhivery config...")
	log.Printf("[ORDER_HANDLER] Delhivery API Token: %s (length: %d)", maskToken(cfg.DelhiveryAPIToken), len(cfg.DelhiveryAPIToken))
	log.Printf("[ORDER_HANDLER] Delhivery Base URL: %s", cfg.DelhiveryBaseURL)
	log.Printf("[ORDER_HANDLER] Delhivery Pickup Location: %s", cfg.DelhiveryPickupLocation)

	// Initialize Delhivery service
	delhiveryConfig := services.DelhiveryConfig{
		APIToken:       cfg.DelhiveryAPIToken,
		BaseURL:        cfg.DelhiveryBaseURL,
		PickupLocation: cfg.DelhiveryPickupLocation,
		SellerName:     cfg.DelhiverySellerName,
		SellerPhone:    cfg.DelhiverySellerPhone,
		SellerAddress:  cfg.DelhiverySellerAddress,
		SellerCity:     cfg.DelhiverySellerCity,
		SellerState:    cfg.DelhiverySellerState,
		SellerPincode:  cfg.DelhiverySellerPincode,
		ReturnAddress:  cfg.DelhiveryReturnAddress,
		ReturnCity:     cfg.DelhiveryReturnCity,
		ReturnState:    cfg.DelhiveryReturnState,
		ReturnPincode:  cfg.DelhiveryReturnPincode,
		ReturnPhone:    cfg.DelhiveryReturnPhone,
	}

	delhiveryService := services.NewDelhiveryService(delhiveryConfig)
	if delhiveryService != nil {
		log.Printf("[ORDER_HANDLER] ✅ DelhiveryService initialized successfully")
		log.Printf("[ORDER_HANDLER] 🏪 Pickup Config: Location='%s', Address='%s', City='%s'",
			cfg.DelhiveryPickupLocation, cfg.DelhiverySellerAddress, cfg.DelhiverySellerCity)
	} else {
		log.Printf("[ORDER_HANDLER] ⚠️ DelhiveryService is nil!")
	}

	return &OrderHandler{
		DB:               db,
		Config:           cfg,
		DelhiveryService: delhiveryService,
		ShipmentQueue:    shipmentQueue,
	}
}

// maskToken masks the API token for logging (shows first 4 and last 4 chars)
func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// generateOrderNumber generates a human-readable order number like MAK-20251214-A1B2
func (h *OrderHandler) generateOrderNumber(ctx context.Context) string {
	// Get today's date
	today := time.Now().Format("20060102")

	// Count orders created today to get sequence number
	startOfDay := time.Now().Truncate(24 * time.Hour)
	orderCollection := h.DB.Collections().Orders
	count, err := orderCollection.CountDocuments(ctx, bson.M{
		"created_at": bson.M{"$gte": startOfDay},
	})
	if err != nil {
		count = 0
	}

	// Generate order number: MAK-YYYYMMDD-XXX (XXX is sequence number)
	return fmt.Sprintf("MAK-%s-%03d", today, count+1)
}

// Checkout processes the checkout and creates an order
func (h *OrderHandler) Checkout(c *fiber.Ctx) error {
	log.Printf("[CHECKOUT] 🛒 Checkout endpoint called")
	ctx := c.Context()

	// Get user info from token
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		log.Printf("[CHECKOUT] ❌ Unauthorized - User data not found")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}
	log.Printf("[CHECKOUT] 👤 User authenticated: %s", user.UserID.Hex())

	// Parse request body
	var req models.CheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Validate request
	if req.ShippingAddress.Street == "" || req.ShippingAddress.City == "" ||
		req.ShippingAddress.State == "" || req.ShippingAddress.ZipCode == "" ||
		req.ShippingAddress.Country == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Complete shipping address is required",
		})
	}

	if req.PaymentInfo.Method == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Payment method is required",
		})
	}

	// Get the user's cart
	cartCollection := h.DB.Collections().CartItems
	cursor, err := cartCollection.Find(ctx, bson.M{"user_id": user.UserID})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve cart",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	// Parse cart items
	var cartItems []models.CartItem
	if err := cursor.All(ctx, &cartItems); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode cart items",
			"error":   err.Error(),
		})
	}

	// Check if cart is empty
	if len(cartItems) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Cart is empty",
		})
	}

	// Price the order from the catalogue, authoritatively. This pass performs
	// no writes: stock is only committed once the payment has been verified,
	// below. Decrementing here and validating afterwards -- as this handler
	// previously did -- burned inventory on every rejected signature and every
	// client/server total mismatch, and never restored it.
	var orderItems []models.OrderItem
	var total float64
	productsCollection := h.DB.Collections().Products

	for _, item := range cartItems {
		var product models.Product
		err := productsCollection.FindOne(ctx, bson.M{"_id": item.ProductID}).Decode(&product)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to retrieve product details",
				"error":   err.Error(),
			})
		}

		if product.Stock < item.Quantity {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("Not enough stock for product %s", product.Name),
			})
		}

		// Use discounted price if active
		finalPrice := product.GetFinalPrice()

		// Get first image if available
		productImage := ""
		if len(product.Images) > 0 {
			productImage = product.Images[0]
		}

		orderItem := models.OrderItem{
			ProductID:   product.ID,
			ProductName: product.Name,
			Brand:       product.Brand,
			Image:       productImage,
			Price:       finalPrice,
			Size:        item.Size,
			Quantity:    item.Quantity,
			Subtotal:    finalPrice * float64(item.Quantity),
		}

		orderItems = append(orderItems, orderItem)
		total += orderItem.Subtotal
	}

	// Verify Razorpay signature if method is razorpay
	if req.PaymentInfo.Method == "razorpay" {
		if req.PaymentInfo.RazorpayOrderID == "" || req.PaymentInfo.RazorpayPaymentID == "" || req.PaymentInfo.RazorpaySignature == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Missing Razorpay payment details"})
		}
		mac := hmac.New(sha256.New, []byte(h.Config.RazorpaySecret))
		mac.Write([]byte(req.PaymentInfo.RazorpayOrderID + "|" + req.PaymentInfo.RazorpayPaymentID))
		expected := hex.EncodeToString(mac.Sum(nil))
		if expected != req.PaymentInfo.RazorpaySignature {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Invalid payment signature"})
		}
	}

	// Defensive: If client supplied a clientTotal ensure it matches authoritative total
	if req.ClientTotal != nil {
		clientTotal := *req.ClientTotal
		// Allow small rounding difference (₹1)
		if clientTotal < total-1 || clientTotal > total+1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("Total mismatch. Client: %.2f Server: %.2f", clientTotal, total),
			})
		}
	}

	// Commit stock. The guard on the update means a unit sold between pricing
	// and here loses the race instead of overselling; anything already taken is
	// handed back before returning.
	committed := make([]models.OrderItem, 0, len(orderItems))
	restoreStock := func() {
		for _, taken := range committed {
			if _, err := productsCollection.UpdateOne(
				ctx,
				bson.M{"_id": taken.ProductID},
				bson.M{"$inc": bson.M{"stock": taken.Quantity}},
			); err != nil {
				// Nothing further can be done automatically; surface it loudly
				// so the discrepancy can be reconciled.
				log.Printf("[CHECKOUT] ⚠️ failed to restore %d unit(s) of product %s: %v",
					taken.Quantity, taken.ProductID.Hex(), err)
			}
			h.DB.CacheDel(ctx, fmt.Sprintf("product:%s", taken.ProductID.Hex()))
		}
	}

	for _, orderItem := range orderItems {
		result, err := productsCollection.UpdateOne(
			ctx,
			bson.M{"_id": orderItem.ProductID, "stock": bson.M{"$gte": orderItem.Quantity}},
			bson.M{"$inc": bson.M{"stock": -orderItem.Quantity}},
		)
		if err != nil {
			restoreStock()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to update product stock",
				"error":   err.Error(),
			})
		}
		if result.MatchedCount == 0 {
			restoreStock()
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("Not enough stock for product %s", orderItem.ProductName),
			})
		}

		committed = append(committed, orderItem)
		h.DB.CacheDel(ctx, fmt.Sprintf("product:%s", orderItem.ProductID.Hex()))
	}

	// Determine order and payment statuses
	orderStatus := "pending"  // pending -> processing -> shipped -> delivered/cancelled/returned
	paymentStatus := "unpaid" // unpaid | paid | refunded | failed
	switch req.PaymentInfo.Method {
	case "razorpay":
		// Signature already verified above, consider payment successful
		paymentStatus = "paid"
		orderStatus = "processing"
	case "cod":
		paymentStatus = "unpaid"
		orderStatus = "processing"
	}

	// Generate human-readable order number
	orderNumber := h.generateOrderNumber(ctx)

	// Prepare pickup details from configuration
	pickupDetails := &models.PickupDetails{
		LocationName: h.Config.DelhiveryPickupLocation,
		SellerName:   h.Config.DelhiverySellerName,
		Address:      h.Config.DelhiverySellerAddress,
		City:         h.Config.DelhiverySellerCity,
		State:        h.Config.DelhiverySellerState,
		Pincode:      h.Config.DelhiverySellerPincode,
		Phone:        h.Config.DelhiverySellerPhone,
		Country:      "India",
	}
	log.Printf("[CHECKOUT] 🏪 PickupDetails: Location='%s', Address='%s', City='%s'",
		pickupDetails.LocationName, pickupDetails.Address, pickupDetails.City)

	// Create the order
	now := time.Now()
	order := models.Order{
		ID:              primitive.NewObjectID(),
		OrderNumber:     orderNumber,
		UserID:          user.UserID,
		Items:           orderItems,
		Total:           total,
		Status:          orderStatus,
		PaymentStatus:   paymentStatus,
		ShippingAddress: req.ShippingAddress,
		PaymentInfo:     req.PaymentInfo,
		PickupDetails:   pickupDetails,
		CustomerPhone:   req.CustomerPhone,
		CustomerEmail:   req.CustomerEmail,
		CustomerName:    req.CustomerName,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Insert the order into the database
	orderCollection := h.DB.Collections().Orders
	_, err = orderCollection.InsertOne(ctx, order)
	if err != nil {
		// The stock was already taken; without this the units would be lost to
		// an order that does not exist.
		restoreStock()
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to create order",
			"error":   err.Error(),
		})
	}

	// Log order creation success
	log.Printf("[CHECKOUT] ✅ Order created: OrderNumber=%s, OrderID=%s", order.OrderNumber, order.ID.Hex())
	if order.PickupDetails != nil {
		log.Printf("[CHECKOUT] 🏪 PickupDetails saved: Location='%s'", order.PickupDetails.LocationName)
	} else {
		log.Printf("[CHECKOUT] ⚠️ PickupDetails is NIL!")
	}

	// Hand shipment creation off to SQS when configured (the Lambda/HTTP API
	// deploy); otherwise call it synchronously as before. A Delhivery failure
	// (or an enqueue failure, with synchronous fallback) is logged and
	// recorded on the order but never fails the checkout response, since the
	// order and payment are already committed at this point.
	switch {
	case h.ShipmentQueue != nil:
		log.Printf("[CHECKOUT] 📦 Enqueuing Delhivery shipment for OrderID=%s", order.ID.Hex())
		if err := h.ShipmentQueue.EnqueueShipment(ctx, order.ID.Hex()); err != nil {
			log.Printf("[CHECKOUT] ⚠️ Failed to enqueue shipment, falling back to synchronous call: %v", err)
			if h.DelhiveryService != nil {
				shipment.CreateDelhiveryShipment(ctx, h.Config, h.DelhiveryService, h.DB.MongoDB, &order)
			}
		}
	case h.DelhiveryService != nil:
		log.Printf("[CHECKOUT] 📦 Creating Delhivery shipment synchronously for OrderID=%s", order.ID.Hex())
		shipment.CreateDelhiveryShipment(ctx, h.Config, h.DelhiveryService, h.DB.MongoDB, &order)
	default:
		log.Printf("[CHECKOUT] ⚠️ DelhiveryService is nil - shipment will NOT be created for OrderID=%s", order.ID.Hex())
	}

	// Clear the user's cart
	_, err = cartCollection.DeleteMany(ctx, bson.M{"user_id": user.UserID})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to clear cart after order",
			"error":   err.Error(),
		})
	}

	// Invalidate cart cache
	cartCacheKey := fmt.Sprintf("cart:%s", user.UserID.Hex())
	h.DB.CacheDel(ctx, cartCacheKey)

	// Invalidate order cache
	ordersCacheKey := fmt.Sprintf("orders:%s", user.UserID.Hex())
	h.DB.CacheDel(ctx, ordersCacheKey)

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "Order placed successfully",
		"data":    order,
	})
}

// GetOrders retrieves order history for a user
func (h *OrderHandler) GetOrders(c *fiber.Ctx) error {
	ctx := c.Context()

	// Determine the target user ID from route params or the authenticated token
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	userIDParam := c.Params("userID")
	var userID primitive.ObjectID
	var err error
	if userIDParam == "" {
		// If no param provided (e.g., /account/orders), default to the authenticated user's ID
		userID = tokenUser.UserID
	} else {
		// Convert user ID from string to ObjectID
		userID, err = primitive.ObjectIDFromHex(userIDParam)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "Invalid user ID format",
				"error":   err.Error(),
			})
		}
	}

	// Authorization: user can view own orders; admin can view any user's orders
	if tokenUser.UserID != userID && tokenUser.Role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized to view these orders",
		})
	}

	// Define OrderResponse type for consistent API responses
	type OrderResponse struct {
		ID              string                `json:"id"`
		OrderNumber     string                `json:"orderNumber"`
		UserID          string                `json:"userId"`
		Items           []models.OrderItem    `json:"items"`
		Total           float64               `json:"total"`
		Status          string                `json:"status"`
		PaymentStatus   string                `json:"paymentStatus"`
		ShippingAddress models.Address        `json:"shippingAddress"`
		PaymentInfo     models.PaymentInfo    `json:"paymentInfo"`
		ShippingInfo    *models.ShippingInfo  `json:"shippingInfo,omitempty"`
		PickupDetails   *models.PickupDetails `json:"pickupDetails,omitempty"`
		CreatedAt       time.Time             `json:"createdAt"`
		UpdatedAt       time.Time             `json:"updatedAt"`
	}

	// Check if the orders are in Redis cache
	cacheKey := fmt.Sprintf("orders:%s", userID.Hex())
	var cachedOrders []OrderResponse
	err = h.DB.CacheGet(ctx, cacheKey, &cachedOrders)
	if err == nil {
		// Cache hit
		log.Printf("[GET_ORDERS] Cache hit for user %s (%d orders)", userID.Hex(), len(cachedOrders))
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Orders retrieved from cache",
			"data":    cachedOrders,
		})
	}

	var orders []models.Order

	// Find all orders for the user, sorted by creation date descending
	orderCollection := h.DB.Collections().Orders
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := orderCollection.Find(ctx, bson.M{"user_id": userID}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve orders",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	// Parse the results
	if err := cursor.All(ctx, &orders); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode orders",
			"error":   err.Error(),
		})
	}

	// Map orders to convert ObjectID to hex string for frontend
	// Initialised rather than declared nil: Go marshals a nil slice as JSON
	// `null`, and a client that reads the list would get null instead of an
	// empty array for a customer who has not ordered yet.
	respOrders := []OrderResponse{}
	for _, o := range orders {
		payStatus := o.PaymentStatus
		if payStatus == "" {
			if o.Status == "paid" || o.PaymentInfo.RazorpayPaymentID != "" {
				payStatus = "paid"
			} else if o.Status == "cancelled" {
				payStatus = "refunded"
			} else {
				payStatus = "unpaid"
			}
		}
		// Debug: Log pickup details from DB
		if o.PickupDetails != nil {
			log.Printf("[GET_ORDERS] 🏪 Order %s has PickupDetails: '%s'",
				o.OrderNumber, o.PickupDetails.LocationName)
		}
		respOrders = append(respOrders, OrderResponse{
			ID:              o.ID.Hex(),
			OrderNumber:     o.OrderNumber,
			UserID:          o.UserID.Hex(),
			Items:           o.Items,
			Total:           o.Total,
			Status:          o.Status,
			PaymentStatus:   payStatus,
			ShippingAddress: o.ShippingAddress,
			PaymentInfo:     o.PaymentInfo,
			ShippingInfo:    o.ShippingInfo,
			PickupDetails:   o.PickupDetails,
			CreatedAt:       o.CreatedAt,
			UpdatedAt:       o.UpdatedAt,
		})
	}

	// Cache the orders (expire after 15 minutes)
	h.DB.CacheSet(ctx, cacheKey, respOrders, 15*time.Minute)

	// Return the orders
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Orders retrieved successfully",
		"data":    respOrders,
	})
}

// GetOrder retrieves a specific order by ID
func (h *OrderHandler) GetOrder(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get order ID from URL parameter
	orderIDParam := c.Params("orderID")
	if orderIDParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Convert order ID from string to ObjectID
	orderID, err := primitive.ObjectIDFromHex(orderIDParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID format",
			"error":   err.Error(),
		})
	}

	// Check if the order is in Redis cache
	cacheKey := fmt.Sprintf("order:%s", orderID.Hex())
	var order models.Order
	err = h.DB.CacheGet(ctx, cacheKey, &order)
	if err == nil {
		// Cache hit
		// Check if the user is authorized to view this order
		tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
		if !ok || (order.UserID != tokenUser.UserID && tokenUser.Role != "admin") {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"success": false,
				"message": "Not authorized to view this order",
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Order retrieved from cache",
			"data":    order,
		})
	}

	// Find the order in the database
	orderCollection := h.DB.Collections().Orders
	err = orderCollection.FindOne(ctx, bson.M{"_id": orderID}).Decode(&order)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Order not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve order",
			"error":   err.Error(),
		})
	}

	// Check if the user is authorized to view this order
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || (order.UserID != tokenUser.UserID && tokenUser.Role != "admin") {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized to view this order",
		})
	}

	// Cache the order (expire after 15 minutes)
	h.DB.CacheSet(ctx, cacheKey, order, 15*time.Minute)

	// Return the order
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Order retrieved successfully",
		"data":    order,
	})
}

// UpdateOrderStatus updates the status of an order (admin only)
func (h *OrderHandler) UpdateOrderStatus(c *fiber.Ctx) error {
	ctx := c.Context()

	// Only admin can update order status
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || tokenUser.Role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Only admins can update order status",
		})
	}

	// Get order ID from URL parameter
	orderIDParam := c.Params("orderID")
	if orderIDParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Convert order ID from string to ObjectID
	orderID, err := primitive.ObjectIDFromHex(orderIDParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID format",
			"error":   err.Error(),
		})
	}

	// Parse request body
	type StatusUpdate struct {
		Status        string `json:"status"`
		PaymentStatus string `json:"paymentStatus,omitempty"`
	}
	var req StatusUpdate
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Validate statuses
	validStatuses := map[string]bool{
		"pending":    true,
		"processing": true,
		"shipped":    true,
		"delivered":  true,
		"cancelled":  true,
		"returned":   true,
	}

	if !validStatuses[req.Status] {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order status. Must be one of: pending, processing, shipped, delivered, cancelled, returned",
		})
	}

	validPaymentStatuses := map[string]bool{
		"unpaid":   true,
		"paid":     true,
		"failed":   true,
		"refunded": true,
	}
	if req.PaymentStatus != "" && !validPaymentStatuses[req.PaymentStatus] {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid payment status. Must be one of: unpaid, paid, failed, refunded",
		})
	}

	// Update the order status
	now := time.Now()
	orderCollection := h.DB.Collections().Orders
	setFields := bson.M{
		"status":     req.Status,
		"updated_at": now,
	}
	if req.PaymentStatus != "" {
		setFields["payment_status"] = req.PaymentStatus
	}
	result, err := orderCollection.UpdateOne(
		ctx,
		bson.M{"_id": orderID},
		bson.M{"$set": setFields},
	)

	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to update order status",
			"error":   err.Error(),
		})
	}

	if result.MatchedCount == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	// Get the updated order
	var updatedOrder models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": orderID}).Decode(&updatedOrder)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve updated order",
			"error":   err.Error(),
		})
	}

	// Invalidate order caches
	orderCacheKey := fmt.Sprintf("order:%s", orderID.Hex())
	userOrdersCacheKey := fmt.Sprintf("orders:%s", updatedOrder.UserID.Hex())
	h.DB.CacheDel(ctx, orderCacheKey)
	h.DB.CacheDel(ctx, userOrdersCacheKey)

	// Return the updated order
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Order status updated successfully",
		"data":    updatedOrder,
	})
}

// CancelOrder cancels an order if it's still in "pending" or "processing" status
func (h *OrderHandler) CancelOrder(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get order ID from URL parameter
	orderIDParam := c.Params("orderID")
	if orderIDParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Convert order ID from string to ObjectID
	orderID, err := primitive.ObjectIDFromHex(orderIDParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID format",
			"error":   err.Error(),
		})
	}

	// Get the order
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": orderID}).Decode(&order)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Order not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve order",
			"error":   err.Error(),
		})
	}

	// Check if the user is authorized to cancel this order
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || (order.UserID != tokenUser.UserID && tokenUser.Role != "admin") {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized to cancel this order",
		})
	}

	// Check if the order can be cancelled
	if order.Status != "pending" && order.Status != "processing" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Only pending or processing orders can be cancelled",
		})
	}

	log.Printf("[CANCEL_ORDER] 🚫 Cancelling order: %s (Status: %s)", order.OrderNumber, order.Status)

	// Try to cancel Delhivery shipment if it exists and hasn't been picked up yet
	if order.ShippingInfo != nil && order.ShippingInfo.Waybill != "" && h.DelhiveryService != nil {
		waybill := order.ShippingInfo.Waybill
		log.Printf("[CANCEL_ORDER] 📦 Attempting to cancel Delhivery shipment: %s", waybill)

		// Only attempt to cancel if not already picked up or delivered
		canCancelShipment := order.ShippingInfo.ShipmentStatus == "" ||
			order.ShippingInfo.ShipmentStatus == "manifested" ||
			order.ShippingInfo.ShipmentStatus == "pending"

		if canCancelShipment {
			cancelErr := h.DelhiveryService.CancelShipment(waybill)
			if cancelErr != nil {
				log.Printf("[CANCEL_ORDER] ⚠️ Failed to cancel Delhivery shipment %s: %v", waybill, cancelErr)
				log.Printf("[CANCEL_ORDER] ℹ️ Continuing with order cancellation despite shipment cancel failure")
				// Don't fail the entire cancellation if Delhivery cancel fails
			} else {
				log.Printf("[CANCEL_ORDER] ✅ Delhivery shipment %s cancelled successfully", waybill)
			}
		} else {
			log.Printf("[CANCEL_ORDER] ℹ️ Shipment already picked up (status: %s), cannot cancel with carrier",
				order.ShippingInfo.ShipmentStatus)
		}
	}

	// Update the order status to "cancelled" and set paymentStatus if prepaid
	now := time.Now()
	setCancel := bson.M{
		"status":     "cancelled",
		"updated_at": now,
	}
	if order.PaymentStatus == "paid" {
		// Business rule: mark as refunded; real refund should be processed via gateway
		setCancel["payment_status"] = "refunded"
		log.Printf("[CANCEL_ORDER] 💰 Order was prepaid, marking for refund")
	}
	_, err = orderCollection.UpdateOne(
		ctx,
		bson.M{"_id": orderID},
		bson.M{"$set": setCancel},
	)

	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to cancel order",
			"error":   err.Error(),
		})
	}

	// Return inventory to stock
	log.Printf("[CANCEL_ORDER] 📦 Restoring inventory for %d items", len(order.Items))
	productsCollection := h.DB.Collections().Products
	for _, item := range order.Items {
		_, err = productsCollection.UpdateOne(
			ctx,
			bson.M{"_id": item.ProductID},
			bson.M{"$inc": bson.M{"stock": item.Quantity}},
		)
		if err != nil {
			// Log error but continue processing
			log.Printf("[CANCEL_ORDER] ⚠️ Error restoring inventory for product %s: %v", item.ProductID.Hex(), err)
		} else {
			log.Printf("[CANCEL_ORDER] ✅ Restored %d units of product %s", item.Quantity, item.ProductName)
		}

		// Invalidate product cache
		productCacheKey := fmt.Sprintf("product:%s", item.ProductID.Hex())
		h.DB.CacheDel(ctx, productCacheKey)
	}

	// Invalidate order caches
	orderCacheKey := fmt.Sprintf("order:%s", orderID.Hex())
	userOrdersCacheKey := fmt.Sprintf("orders:%s", order.UserID.Hex())
	h.DB.CacheDel(ctx, orderCacheKey)
	h.DB.CacheDel(ctx, userOrdersCacheKey)

	log.Printf("[CANCEL_ORDER] ✅ Order %s cancelled successfully", order.OrderNumber)

	// Return success response
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Order cancelled successfully",
	})
}

// GetAllOrders returns all orders (admin only)
func (h *OrderHandler) GetAllOrders(c *fiber.Ctx) error {
	ctx := c.Context()
	// Only admin can access
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || tokenUser.Role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized",
		})
	}
	orderCollection := h.DB.Collections().Orders
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := orderCollection.Find(ctx, bson.M{}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve orders",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)
	var orders []models.Order
	if err := cursor.All(ctx, &orders); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode orders",
			"error":   err.Error(),
		})
	}
	// Map orders to frontend format if needed
	type OrderResponse struct {
		ID              string               `json:"id"`
		OrderNumber     string               `json:"orderNumber"`
		UserID          string               `json:"userId"`
		CustomerName    string               `json:"customerName"`
		Items           []models.OrderItem   `json:"items"`
		Total           float64              `json:"total"`
		Status          string               `json:"status"`
		PaymentStatus   string               `json:"paymentStatus"`
		ShippingAddress models.Address       `json:"shippingAddress"`
		PaymentInfo     models.PaymentInfo   `json:"paymentInfo"`
		ShippingInfo    *models.ShippingInfo `json:"shippingInfo,omitempty"`
		CreatedAt       time.Time            `json:"createdAt"`
		UpdatedAt       time.Time            `json:"updatedAt"`
	}
	userCollection := h.DB.Collections().Users
	// Cache userId to name to avoid duplicate DB calls
	userNameCache := make(map[string]string)
	// Initialised rather than declared nil: Go marshals a nil slice as JSON
	// `null`, and a client that reads the list would get null instead of an
	// empty array for a customer who has not ordered yet.
	respOrders := []OrderResponse{}
	for _, o := range orders {
		payStatus := o.PaymentStatus
		if payStatus == "" {
			if o.Status == "paid" || o.PaymentInfo.RazorpayPaymentID != "" {
				payStatus = "paid"
			} else if o.Status == "cancelled" {
				payStatus = "refunded"
			} else {
				payStatus = "unpaid"
			}
		}
		userIdStr := o.UserID.Hex()
		customerName := ""
		if cached, ok := userNameCache[userIdStr]; ok {
			customerName = cached
		} else {
			var user models.User
			err := userCollection.FindOne(ctx, bson.M{"_id": o.UserID}).Decode(&user)
			if err == nil {
				customerName = user.Name
			}
			userNameCache[userIdStr] = customerName
		}
		respOrders = append(respOrders, OrderResponse{
			ID:              o.ID.Hex(),
			OrderNumber:     o.OrderNumber,
			UserID:          userIdStr,
			CustomerName:    customerName,
			Items:           o.Items,
			Total:           o.Total,
			Status:          o.Status,
			PaymentStatus:   payStatus,
			ShippingAddress: o.ShippingAddress,
			PaymentInfo:     o.PaymentInfo,
			ShippingInfo:    o.ShippingInfo,
			CreatedAt:       o.CreatedAt,
			UpdatedAt:       o.UpdatedAt,
		})
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "All orders retrieved",
		"data":    respOrders,
	})
}
