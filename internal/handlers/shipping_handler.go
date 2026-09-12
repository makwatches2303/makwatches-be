package handlers

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
)

// ShippingHandler handles shipping related requests
type ShippingHandler struct {
	DB               *database.DBClient
	Config           *config.Config
	DelhiveryService *services.DelhiveryService
}

// NewShippingHandler creates a new instance of ShippingHandler
func NewShippingHandler(db *database.DBClient, cfg *config.Config) *ShippingHandler {
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

	return &ShippingHandler{
		DB:               db,
		Config:           cfg,
		DelhiveryService: services.NewDelhiveryService(delhiveryConfig),
	}
}

// CreateShipmentForOrder creates a Delhivery shipment for an order
func (h *ShippingHandler) CreateShipmentForOrder(order *models.Order) (*services.CreateShipmentResponse, error) {
	if h.Config.DelhiveryAPIToken == "" {
		return nil, fmt.Errorf("delhivery API token not configured")
	}

	// Build product description from order items
	var productNames []string
	totalQuantity := 0
	for _, item := range order.Items {
		productNames = append(productNames, item.ProductName)
		totalQuantity += item.Quantity
	}
	productDesc := strings.Join(productNames, ", ")
	if len(productDesc) > 200 {
		productDesc = productDesc[:197] + "..."
	}

	// Determine payment mode
	paymentMode := "Prepaid"
	codAmount := 0.0
	if order.PaymentInfo.Method == "cod" {
		paymentMode = "COD"
		codAmount = order.Total
	}

	// Get customer details
	customerName := order.CustomerName
	if customerName == "" {
		customerName = order.ShippingAddress.Name
	}
	if customerName == "" {
		customerName = "Customer"
	}

	customerPhone := order.CustomerPhone
	if customerPhone == "" {
		customerPhone = order.ShippingAddress.Phone
	}

	// Create shipment request
	req := services.CreateShipmentRequest{
		CustomerName:    customerName,
		CustomerPhone:   customerPhone,
		CustomerEmail:   order.CustomerEmail,
		CustomerAddress: order.ShippingAddress.Street,
		CustomerCity:    order.ShippingAddress.City,
		CustomerState:   order.ShippingAddress.State,
		CustomerPincode: order.ShippingAddress.ZipCode,
		CustomerCountry: order.ShippingAddress.Country,
		OrderID:         order.ID.Hex(),
		OrderDate:       order.CreatedAt.Format("2006-01-02"),
		TotalAmount:     order.Total,
		PaymentMode:     paymentMode,
		CODAmount:       codAmount,
		ProductQuantity: totalQuantity,
		ProductDesc:     productDesc,
		// Default package dimensions for watches (can be configured later)
		Weight:  500, // 500 grams
		Length:  15,  // 15 cm
		Breadth: 10,  // 10 cm
		Height:  8,   // 8 cm
	}

	// Create items list
	for _, item := range order.Items {
		req.Items = append(req.Items, services.ShipmentItem{
			Name:     item.ProductName,
			SKU:      item.ProductID.Hex(),
			Quantity: item.Quantity,
			Price:    item.Price,
		})
	}

	// Call Delhivery API
	return h.DelhiveryService.CreateShipment(req)
}

// TrackShipment returns tracking info for an order
func (h *ShippingHandler) TrackShipment(c *fiber.Ctx) error {
	orderID := c.Params("orderID")
	if orderID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Get user info
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized",
		})
	}

	// Parse order ID
	objID, err := primitive.ObjectIDFromHex(orderID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID",
		})
	}

	// Get the order
	ctx := c.Context()
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": objID}).Decode(&order)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	// Check authorization (user can view own orders, admin can view all)
	if user.UserID != order.UserID && user.Role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized to view this order",
		})
	}

	// Check if shipping info exists
	if order.ShippingInfo == nil || order.ShippingInfo.Waybill == "" {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "No tracking information available for this order",
		})
	}

	// Get tracking info from Delhivery
	tracking, err := h.DelhiveryService.TrackShipment(order.ShippingInfo.Waybill)
	if err != nil {
		log.Printf("Error tracking shipment %s: %v", order.ShippingInfo.Waybill, err)
		// Return cached info if available
		return c.JSON(fiber.Map{
			"success": true,
			"data": fiber.Map{
				"waybill":          order.ShippingInfo.Waybill,
				"trackingUrl":      order.ShippingInfo.TrackingURL,
				"status":           order.ShippingInfo.ShipmentStatus,
				"lastUpdate":       order.ShippingInfo.LastStatusUpdate,
				"expectedDelivery": order.ShippingInfo.ExpectedDelivery,
				"currentLocation":  order.ShippingInfo.CurrentLocation,
				"error":            "Unable to fetch live tracking, showing last known status",
			},
		})
	}

	// Update order with latest tracking info
	update := bson.M{
		"$set": bson.M{
			"shipping_info.shipment_status":    tracking.ShipmentStatus,
			"shipping_info.last_status_update": time.Now(),
			"shipping_info.expected_delivery":  tracking.ExpectedDelivery,
			"shipping_info.current_location":   tracking.StatusLocation,
			"updated_at":                       time.Now(),
		},
	}

	// Update order status based on shipping status
	if tracking.ShipmentStatus == "delivered" {
		update["$set"].(bson.M)["status"] = "delivered"
		update["$set"].(bson.M)["shipping_info.delivered_at"] = time.Now()
		// Mark COD as paid when delivered
		if order.PaymentInfo.Method == "cod" {
			update["$set"].(bson.M)["payment_status"] = "paid"
		}
	} else if tracking.ShipmentStatus == "picked_up" {
		update["$set"].(bson.M)["status"] = "shipped"
		update["$set"].(bson.M)["shipping_info.picked_up_at"] = time.Now()
	} else if tracking.ShipmentStatus == "returned" {
		update["$set"].(bson.M)["status"] = "returned"
	}

	orderCollection.UpdateOne(ctx, bson.M{"_id": objID}, update)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    tracking,
	})
}

// TrackByWaybill returns tracking info by waybill number (public endpoint)
func (h *ShippingHandler) TrackByWaybill(c *fiber.Ctx) error {
	waybill := c.Params("waybill")
	if waybill == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Waybill number is required",
		})
	}

	tracking, err := h.DelhiveryService.TrackShipment(waybill)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Unable to track shipment",
			"error":   err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    tracking,
	})
}

// CheckPincode checks if a pincode is serviceable
func (h *ShippingHandler) CheckPincode(c *fiber.Ctx) error {
	pincode := c.Params("pincode")
	if pincode == "" {
		pincode = c.Query("pincode")
	}
	if pincode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Pincode is required",
		})
	}

	serviceability, err := h.DelhiveryService.CheckPincodeServiceability(pincode)
	if err != nil {
		// "We could not check" is not "we do not deliver here". Collapsing the
		// two told customers at serviceable addresses that we do not ship to
		// them whenever our carrier credentials failed.
		if errors.Is(err, services.ErrCarrierUnavailable) {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"success": false,
				"message": "We could not check delivery for this pincode right now.",
				"error":   err.Error(),
			})
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success":     false,
			"serviceable": false,
			"message":     "Pincode not serviceable",
		})
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"serviceable": true,
		"data":        serviceability,
	})
}

// CheckoutShippingOption represents a normalized delivery option for checkout
type CheckoutShippingOption struct {
	ID                    string  `json:"id"`
	Provider              string  `json:"provider"`
	ProviderCourierID     string  `json:"providerCourierId"`
	CourierName           string  `json:"courierName"`
	Charge                float64 `json:"charge"`
	EstimatedDeliveryDays int     `json:"estimatedDeliveryDays,omitempty"`
	ETD                   string  `json:"etd,omitempty"`
	CODAvailable          bool    `json:"codAvailable"`
	Mode                  string  `json:"mode,omitempty"`
	Recommended           bool    `json:"recommended,omitempty"`
	Quote                 string  `json:"quote"`
}

// GetCheckoutShippingOptions returns delivery courier options for cart checkout
func (h *ShippingHandler) GetCheckoutShippingOptions(c *fiber.Ctx) error {
	var req struct {
		Pincode string `json:"pincode"`
		COD     bool   `json:"cod"`
	}
	_ = c.BodyParser(&req)

	pincode := strings.TrimSpace(req.Pincode)
	if pincode == "" {
		pincode = strings.TrimSpace(c.Query("pincode"))
	}
	if len(pincode) < 6 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "A valid 6-digit destination pincode is required",
		})
	}

	serviceability, err := h.DelhiveryService.CheckPincodeServiceability(pincode)
	if err != nil {
		if errors.Is(err, services.ErrPincodeNotServiceable) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Pincode is not serviceable by courier partners",
			})
		}
		// Carrier API unreachable or credentials issue: do not block customer checkout
		log.Printf("[SHIPPING-OPTIONS] ⚠️ Carrier check failed for %s (%v), using default express option", pincode, err)
		serviceability = &services.PincodeServiceability{
			Pincode: pincode,
			COD:     true,
			Prepaid: true,
		}
	}

	var options []CheckoutShippingOption
	// If customer explicitly requested COD and carrier does not support COD at this pincode
	if req.COD && !serviceability.COD {
		return c.JSON(fiber.Map{
			"success":  true,
			"pincode":  pincode,
			"provider": "delhivery",
			"cod":      false,
			"prepaid":  serviceability.Prepaid,
			"options":  options,
			"message":  "Cash on delivery is not available for this pincode. Please select Pay Online.",
		})
	}

	modeStr := "online"
	if req.COD {
		modeStr = "cod"
	}
	quote := fmt.Sprintf("quote_delhivery_%s_%s_%d", pincode, modeStr, time.Now().Unix())
	options = append(options, CheckoutShippingOption{
		ID:                    "delhivery-express",
		Provider:              "delhivery",
		ProviderCourierID:     "delhivery_surface",
		CourierName:           "Delhivery Express (Insured)",
		Charge:                0, // Free complimentary insured delivery for MAK Watches
		EstimatedDeliveryDays: 3,
		CODAvailable:          serviceability.COD,
		Mode:                  "Express",
		Recommended:           true,
		Quote:                 quote,
	})

	return c.JSON(fiber.Map{
		"success":  true,
		"pincode":  pincode,
		"provider": "delhivery",
		"cod":      serviceability.COD,
		"prepaid":  serviceability.Prepaid,
		"options":  options,
	})
}

// DelhiveryWebhook handles status updates from Delhivery.
//
// Unlike RazorpayWebhook, Delhivery does not sign its callbacks with an
// HMAC -- the mechanism it offers is a shared token you configure alongside
// the registered callback URL in the Delhivery One dashboard. This handler
// requires that same token back on every call, as a bearer token or a
// ?token= query param, and fails closed (like RazorpayWebhook) if none is
// configured: an unconfigured token must not silently mean "trust anyone."
func (h *ShippingHandler) DelhiveryWebhook(c *fiber.Ctx) error {
	if h.Config.DelhiveryWebhookToken == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"success": false,
			"message": "Webhook token not configured",
		})
	}

	provided := strings.TrimPrefix(c.Get("Authorization"), "Bearer ")
	if provided == "" {
		provided = c.Query("token")
	}
	if provided == "" || !hmac.Equal([]byte(provided), []byte(h.Config.DelhiveryWebhookToken)) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid or missing webhook token",
		})
	}

	body := c.Body()

	// Parse webhook payload
	payload, err := services.ParseWebhook(body)
	if err != nil {
		log.Printf("Failed to parse Delhivery webhook: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid webhook payload",
		})
	}

	log.Printf("Received Delhivery webhook: waybill=%s, status=%s, statusType=%s",
		payload.Waybill, payload.Status, payload.StatusType)

	// Find order by waybill
	ctx := c.Context()
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{
		"shipping_info.waybill": payload.Waybill,
	}).Decode(&order)

	if err != nil {
		log.Printf("Order not found for waybill %s: %v", payload.Waybill, err)
		// Return success anyway so Delhivery doesn't keep retrying
		return c.JSON(fiber.Map{
			"success": true,
			"message": "Webhook received",
		})
	}

	// Map Delhivery status to internal status
	shipmentStatus := services.GetOrderStatusFromWebhook(payload.StatusType)

	// Prepare update
	update := bson.M{
		"$set": bson.M{
			"shipping_info.shipment_status":    shipmentStatus,
			"shipping_info.last_status_update": time.Now(),
			"shipping_info.current_location":   payload.StatusLocation,
			"shipping_info.expected_delivery":  payload.ExpectedDate,
			"updated_at":                       time.Now(),
		},
	}

	// Update order status based on shipping status
	switch shipmentStatus {
	case "delivered":
		update["$set"].(bson.M)["status"] = "delivered"
		update["$set"].(bson.M)["shipping_info.delivered_at"] = time.Now()
		// Mark COD as paid when delivered
		if order.PaymentInfo.Method == "cod" {
			update["$set"].(bson.M)["payment_status"] = "paid"
		}
	case "picked_up":
		update["$set"].(bson.M)["status"] = "shipped"
		update["$set"].(bson.M)["shipping_info.picked_up_at"] = time.Now()
	case "out_for_delivery":
		update["$set"].(bson.M)["status"] = "out_for_delivery"
	case "returned":
		update["$set"].(bson.M)["status"] = "returned"
	case "undelivered":
		// Keep status as shipped but log the issue
		log.Printf("Order %s undelivered: %s", order.ID.Hex(), payload.Remarks)
	}

	// Update the order
	_, err = orderCollection.UpdateOne(ctx, bson.M{"_id": order.ID}, update)
	if err != nil {
		log.Printf("Failed to update order %s: %v", order.ID.Hex(), err)
	}

	// Invalidate order cache
	orderCacheKey := fmt.Sprintf("order:%s", order.ID.Hex())
	h.DB.CacheDel(ctx, orderCacheKey)
	ordersCacheKey := fmt.Sprintf("orders:%s", order.UserID.Hex())
	h.DB.CacheDel(ctx, ordersCacheKey)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Webhook processed successfully",
	})
}

// RetryShipment retries creating a shipment for an order (admin only)
func (h *ShippingHandler) RetryShipment(c *fiber.Ctx) error {
	orderID := c.Params("orderID")
	if orderID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Parse order ID
	objID, err := primitive.ObjectIDFromHex(orderID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID",
		})
	}

	// Get the order
	ctx := c.Context()
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": objID}).Decode(&order)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	// Check if shipment already exists
	if order.ShippingInfo != nil && order.ShippingInfo.Waybill != "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Shipment already created for this order",
			"waybill": order.ShippingInfo.Waybill,
		})
	}

	// Create shipment
	shipmentResp, err := h.CreateShipmentForOrder(&order)
	if err != nil {
		// Update order with error
		retryCount := 0
		if order.ShippingInfo != nil {
			retryCount = order.ShippingInfo.RetryCount
		}
		orderCollection.UpdateOne(ctx, bson.M{"_id": objID}, bson.M{
			"$set": bson.M{
				"shipping_info": models.ShippingInfo{
					Provider:      "delhivery",
					ShipmentError: err.Error(),
					RetryCount:    retryCount + 1,
				},
				"updated_at": time.Now(),
			},
		})
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to create shipment",
			"error":   err.Error(),
		})
	}

	// Update order with shipping info
	trackingURL := fmt.Sprintf("https://www.delhivery.com/track/package/%s", shipmentResp.Waybill)
	orderCollection.UpdateOne(ctx, bson.M{"_id": objID}, bson.M{
		"$set": bson.M{
			"shipping_info": models.ShippingInfo{
				Provider:          "delhivery",
				Waybill:           shipmentResp.Waybill,
				TrackingURL:       trackingURL,
				ShipmentStatus:    "manifested",
				ShipmentCreatedAt: time.Now(),
				LastStatusUpdate:  time.Now(),
			},
			"status":     "processing",
			"updated_at": time.Now(),
		},
	})

	return c.JSON(fiber.Map{
		"success":     true,
		"message":     "Shipment created successfully",
		"waybill":     shipmentResp.Waybill,
		"trackingUrl": trackingURL,
	})
}

// CancelShipment cancels a shipment (admin only)
func (h *ShippingHandler) CancelShipment(c *fiber.Ctx) error {
	orderID := c.Params("orderID")
	if orderID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Parse order ID
	objID, err := primitive.ObjectIDFromHex(orderID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID",
		})
	}

	// Get the order
	ctx := c.Context()
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": objID}).Decode(&order)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	// Check if shipment exists
	if order.ShippingInfo == nil || order.ShippingInfo.Waybill == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "No shipment found for this order",
		})
	}

	// Cancel with Delhivery
	err = h.DelhiveryService.CancelShipment(order.ShippingInfo.Waybill)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to cancel shipment",
			"error":   err.Error(),
		})
	}

	// Update order
	orderCollection.UpdateOne(ctx, bson.M{"_id": objID}, bson.M{
		"$set": bson.M{
			"shipping_info.shipment_status":    "cancelled",
			"shipping_info.last_status_update": time.Now(),
			"status":                           "cancelled",
			"updated_at":                       time.Now(),
		},
	})

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Shipment cancelled successfully",
	})
}

// GetShippingLabel gets the shipping label URL for an order (admin only)
func (h *ShippingHandler) GetShippingLabel(c *fiber.Ctx) error {
	orderID := c.Params("orderID")
	if orderID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Order ID is required",
		})
	}

	// Parse order ID
	objID, err := primitive.ObjectIDFromHex(orderID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID",
		})
	}

	// Get the order
	ctx := c.Context()
	orderCollection := h.DB.Collections().Orders
	var order models.Order
	err = orderCollection.FindOne(ctx, bson.M{"_id": objID}).Decode(&order)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	// Check if shipment exists
	if order.ShippingInfo == nil || order.ShippingInfo.Waybill == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "No shipment found for this order",
		})
	}

	// Generate label URL (Delhivery provides this)
	labelURL := fmt.Sprintf("%s/api/p/packing_slip?wbns=%s&pdf=true",
		h.Config.DelhiveryBaseURL, order.ShippingInfo.Waybill)

	return c.JSON(fiber.Map{
		"success":  true,
		"labelUrl": labelURL,
		"waybill":  order.ShippingInfo.Waybill,
	})
}

// BulkTrackShipments tracks multiple shipments (admin only)
func (h *ShippingHandler) BulkTrackShipments(c *fiber.Ctx) error {
	var req struct {
		Waybills []string `json:"waybills"`
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
		})
	}

	if len(req.Waybills) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "At least one waybill is required",
		})
	}

	// Track each waybill
	results := make(map[string]interface{})
	for _, waybill := range req.Waybills {
		tracking, err := h.DelhiveryService.TrackShipment(waybill)
		if err != nil {
			results[waybill] = fiber.Map{
				"error": err.Error(),
			}
		} else {
			results[waybill] = tracking
		}
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    results,
	})
}

// RequestPickup requests a pickup from Delhivery (admin only)
func (h *ShippingHandler) RequestPickup(c *fiber.Ctx) error {
	var req struct {
		PickupDate       string `json:"pickupDate"`       // YYYY-MM-DD format
		PickupTime       string `json:"pickupTime"`       // HH:MM:SS format (e.g., "10:00:00")
		ExpectedPackages int    `json:"expectedPackages"` // Number of packages to be picked up
	}

	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
		})
	}

	if req.PickupDate == "" {
		req.PickupDate = time.Now().Format("2006-01-02")
	}
	if req.PickupTime == "" {
		req.PickupTime = "10:00:00" // Default pickup time
	}
	if req.ExpectedPackages <= 0 {
		req.ExpectedPackages = 1
	}

	err := h.DelhiveryService.RequestPickup(req.PickupDate, req.PickupTime, req.ExpectedPackages)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to request pickup",
			"error":   err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Pickup requested successfully",
		"date":    req.PickupDate,
	})
}

// Unused import fix
var _ = io.EOF
