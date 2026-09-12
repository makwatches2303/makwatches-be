package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
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
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
)

// OrderHandler handles order related requests
type OrderHandler struct {
	DB       *database.DBClient
	Config   *config.Config
	// Shipping is the provider-neutral shipping service. The handler holds no
	// carrier client of its own: booking a parcel at checkout and booking one
	// from the admin retry endpoint now run the same code, against whichever
	// provider is configured.
	Shipping *shipping.Service
}

// NewOrderHandler creates a new instance of OrderHandler.
//
// The shipping service is injected rather than constructed here, so there is
// exactly one carrier client per process instead of one per handler.
func NewOrderHandler(db *database.DBClient, cfg *config.Config, shippingSvc *shipping.Service) *OrderHandler {
	return &OrderHandler{
		DB:       db,
		Config:   cfg,
		Shipping: shippingSvc,
	}
}

// rateChoiceFrom converts a verified quote into the persisted snapshot.
//
// Returns nil for an order placed without a delivery selection, so the field
// stays absent rather than recording a zero-charge choice nobody made.
func rateChoiceFrom(q *shipping.VerifiedQuote) *models.RateChoice {
	if q == nil {
		return nil
	}
	return &models.RateChoice{
		OptionID:              shipping.OptionID(q.Provider, q.CourierID),
		Provider:              q.Provider,
		ProviderCourierID:     q.CourierID,
		CourierName:           q.CourierName,
		Charge:                q.Charge,
		EstimatedDeliveryDays: q.EstimatedDeliveryDays,
		ETD:                   q.ETD,
		CODAvailable:          q.CODAvailable,
	}
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
	var subtotal float64
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
		subtotal += orderItem.Subtotal
	}

	total := subtotal
	var discountAmount float64
	var appliedCoupon *models.Coupon

	if strings.TrimSpace(req.CouponCode) != "" {
		code := strings.ToUpper(strings.TrimSpace(req.CouponCode))
		var coupon models.Coupon
		err := h.DB.Collections().Coupons.FindOne(ctx, bson.M{"code": code}).Decode(&coupon)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("Invalid or non-existent coupon code '%s'", code),
			})
		}
		calculatedDiscount, err := coupon.CalculateDiscount(subtotal)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": fmt.Sprintf("Coupon error: %s", err.Error()),
			})
		}
		discountAmount = calculatedDiscount
		total = math.Max(0, subtotal-discountAmount)
		total = math.Round(total*100) / 100
		appliedCoupon = &coupon
		req.CouponCode = coupon.Code
	}

	// The goods total after discount, before delivery.
	goodsTotal := total
	isCOD := req.PaymentInfo.Method == "cod"

	// Resolve the delivery charge from the customer's selected quote.
	//
	// The client sends only the opaque token. The charge is read out of it
	// after the signature and the whole binding are re-verified against this
	// customer, this cart, this destination and this payment mode -- so a
	// tampered amount, a courier swap, an expired quote, another customer's
	// quote or a quote for a different address are all rejected here rather
	// than silently priced.
	var verifiedQuote *shipping.VerifiedQuote
	shippingCharge := 0.0

	if token := strings.TrimSpace(req.ShippingQuote); token != "" {
		if h.Shipping == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"success": false,
				"message": "Delivery options are unavailable right now. Please try again.",
			})
		}

		pkg := h.Shipping.DefaultPackage()
		binding := shipping.QuoteBinding{
			UserID:   user.UserID.Hex(),
			CartHash: shipping.CartFingerprint(cartLinesFromItems(cartItems)),
			Pincode:  strings.TrimSpace(req.ShippingAddress.ZipCode),
			// Never from the request: a client-chosen parcel would let a
			// customer quote a lighter, cheaper shipment than we send.
			WeightGrams: pkg.WeightGrams,
			COD:         isCOD,
		}

		verified, qErr := h.Shipping.Quoter().Verify(token, binding, time.Now())
		if qErr != nil {
			se := shipping.AsError(qErr)
			log.Printf("[CHECKOUT] rejected shipping quote for user %s: %s", user.UserID.Hex(), se.Detail)
			// Expired or superseded quotes are recoverable by re-quoting, so
			// the code is returned for the client to act on.
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"code":    string(se.Code),
				"message": "Your delivery option is no longer valid. Please choose it again.",
			})
		}

		// A courier that will not carry COD cannot be used for a COD order,
		// whatever the client selected.
		if isCOD && !verified.CODAvailable {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"code":    string(shipping.CodeInvalidRequest),
				"message": "The selected courier does not offer cash on delivery. Choose another courier or pay online.",
			})
		}

		verifiedQuote = verified
		shippingCharge = verified.Charge
		total = goodsTotal + shippingCharge
	} else if h.Shipping != nil && h.Config.RequireShippingSelection {
		// Falling through with no delivery charge would ship at our expense
		// and hide the omission, so it is refused when a selection is required.
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeInvalidRequest),
			"message": "Please choose a delivery option before placing your order.",
		})
	}

	// Verify the payment against the authoritative total.
	//
	// This used to check only the HMAC over (order_id|payment_id), which
	// proves the identifiers are genuine but says nothing about the amount. A
	// customer could pay a ₹1,000 intent, then add ₹10,000 of items and submit
	// the same payment triple: the signature verified and the order was created
	// as paid. VerifyPaid asks Razorpay what was actually captured and compares
	// it to the total computed above.
	if req.PaymentInfo.Method == "razorpay" {
		if req.PaymentInfo.RazorpayOrderID == "" || req.PaymentInfo.RazorpayPaymentID == "" || req.PaymentInfo.RazorpaySignature == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Missing Razorpay payment details"})
		}

		// A payment may settle exactly one order. Without this, the same
		// captured payment could be replayed to create order after order.
		used, err := h.DB.Collections().Orders.CountDocuments(ctx, bson.M{
			"payment_info.razorpay_payment_id": req.PaymentInfo.RazorpayPaymentID,
		})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false, "message": "Could not verify payment",
			})
		}
		if used > 0 {
			log.Printf("[CHECKOUT] rejected replayed razorpay payment for user %s", user.UserID.Hex())
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"message": "This payment has already been used for an order.",
			})
		}

		verifier := NewRazorpayVerifier(h.Config.RazorpayKey, h.Config.RazorpaySecret)
		if err := verifier.VerifyPaid(ctx,
			req.PaymentInfo.RazorpayOrderID,
			req.PaymentInfo.RazorpayPaymentID,
			req.PaymentInfo.RazorpaySignature,
			total,
		); err != nil {
			// The reason stays in the log; the customer gets one message.
			log.Printf("[CHECKOUT] payment verification failed for user %s: %v", user.UserID.Hex(), err)
			status := fiber.StatusBadRequest
			if errors.Is(err, ErrPaymentGatewayUnreach) {
				status = fiber.StatusBadGateway
			}
			return c.Status(status).JSON(fiber.Map{
				"success": false,
				"message": "We could not confirm your payment. Nothing has been charged for this order.",
			})
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
		ID:             primitive.NewObjectID(),
		OrderNumber:    orderNumber,
		UserID:         user.UserID,
		Items:          orderItems,
		Total:          total,
		Subtotal:       subtotal,
		CouponCode:     req.CouponCode,
		DiscountAmount: discountAmount,
		ShippingCharge: shippingCharge,
		// Snapshot the delivery choice so the order stays explainable later,
		// and so an admin retry books the courier the customer actually paid
		// for rather than re-quoting weeks afterwards.
		ShippingOption: rateChoiceFrom(verifiedQuote),
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

	// If a coupon was applied, increment its redemption count
	if appliedCoupon != nil {
		_, _ = h.DB.Collections().Coupons.UpdateOne(
			ctx,
			bson.M{"_id": appliedCoupon.ID},
			bson.M{"$inc": bson.M{"usage_count": 1}},
		)
	}

	// Log order creation success
	log.Printf("[CHECKOUT] ✅ Order created: OrderNumber=%s, OrderID=%s", order.OrderNumber, order.ID.Hex())
	if order.PickupDetails != nil {
		log.Printf("[CHECKOUT] 🏪 PickupDetails saved: Location='%s'", order.PickupDetails.LocationName)
	} else {
		log.Printf("[CHECKOUT] ⚠️ PickupDetails is NIL!")
	}

	// Book the shipment in the background so the customer is not made to wait
	// on a carrier round trip.
	//
	// The goroutine gets its own context: c.UserContext() is cancelled the
	// moment this response is written, which would abort the carrier call. The
	// booking is idempotent at the service layer, so this racing with an admin
	// retry cannot produce two parcels.
	if h.Shipping != nil {
		log.Printf("[CHECKOUT] 📦 Booking shipment for OrderID=%s", order.ID.Hex())
		go h.bookShipment(order, verifiedQuote)
	} else {
		log.Printf("[CHECKOUT] ⚠️ shipping service unavailable - no shipment booked for OrderID=%s", order.ID.Hex())
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

	// Mark matching abandoned carts as recovered
	customerPhone := req.CustomerPhone
	if customerPhone == "" {
		customerPhone = req.ShippingAddress.Phone
	}
	if customerPhone != "" {
		if normPhone, err := whatsapp.NormalizePhoneNumber(customerPhone); err == nil {
			_, _ = h.DB.Collections().AbandonedCarts.UpdateMany(
				ctx,
				bson.M{
					"phone":  normPhone,
					"status": bson.M{"$in": []string{"active", "reminded"}},
				},
				bson.M{"$set": bson.M{"status": "recovered", "updated_at": time.Now()}},
			)
		}
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "Order placed successfully",
		"data":    order,
	})
}

// bookShipment books the parcel for a freshly-placed order.
//
// This replaces the checkout-side shipment builder that used to live here. All
// of the mapping -- customer details, package figures, payment mode, the
// carrier order reference -- now happens once inside shipping.Service, so the
// checkout path and the admin retry path can no longer drift apart. They did:
// one used the human-readable order number as the carrier reference and
// defaulted the country, the other used the raw ObjectID and did neither.
func (h *OrderHandler) bookShipment(order models.Order, quote *shipping.VerifiedQuote) {
	// Detached context with its own deadline: the request context is already
	// cancelled by the time this runs.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	display := order.OrderNumber
	if display == "" {
		display = order.ID.Hex()
	}

	shipment, err := h.Shipping.CreateShipmentForOrder(ctx, &order, shipping.CreateOptions{
		AssignAWB: true,
		// The courier the customer selected and paid for. Nil for an order
		// placed without a selection, in which case the service falls back to
		// whatever is recorded on the order.
		Quote: quote,
	})
	if err != nil {
		// The failure is already recorded on the order and the shipment record
		// by the service; an admin can retry from the dashboard. Only the
		// classification is logged here -- provider detail stays in the
		// service's own log line.
		log.Printf("[CHECKOUT] shipment booking failed for order %s: %s",
			display, shipping.CodeOf(err))
		return
	}
	log.Printf("[CHECKOUT] shipment booked for order %s: provider=%s tracking=%s",
		display, shipment.Provider, shipment.TrackingNumber)
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

	// Withdraw the parcel at whichever carrier holds it, if it can still be
	// withdrawn. The order is cancelled either way: a carrier that declines
	// must not block the customer's cancellation, and a parcel already in
	// transit is handled by the carrier's own return flow.
	if h.Shipping != nil && order.ShippingInfo.HasShipment() {
		cancelled, cancelErr := h.Shipping.CancelIfCancellable(ctx, &order)
		switch {
		case cancelErr != nil:
			log.Printf("[CANCEL_ORDER] ⚠️ carrier cancellation failed for order %s: %s",
				order.ID.Hex(), shipping.CodeOf(cancelErr))
		case cancelled:
			log.Printf("[CANCEL_ORDER] ✅ carrier shipment withdrawn for order %s", order.ID.Hex())
		default:
			log.Printf("[CANCEL_ORDER] ℹ️ shipment for order %s is past the cancellable window (status: %s)",
				order.ID.Hex(), order.ShippingInfo.ShipmentStatus)
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

// GetAllOrders returns orders, paginated (admin only).
//
// Previously fetched every order in the database on every call -- fine at a
// few hundred orders, not at scale. Optional query params: page, limit
// (defaults 1/20, same convention as GetProducts), status (exact match) and
// q (free-text, matches order number, customer name, or shipping-address
// name).
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

	page, err := strconv.Atoi(c.Query("page", "1"))
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(c.Query("limit", "20"))
	if err != nil || limit < 1 {
		limit = 20
	}

	filter := bson.M{}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}
	if search := c.Query("q"); search != "" {
		pattern := regexp.QuoteMeta(search)
		filter["$or"] = bson.A{
			bson.M{"order_number": bson.M{"$regex": pattern, "$options": "i"}},
			bson.M{"customer_name": bson.M{"$regex": pattern, "$options": "i"}},
			bson.M{"shipping_address.name": bson.M{"$regex": pattern, "$options": "i"}},
		}
	}

	orderCollection := h.DB.Collections().Orders

	total, err := orderCollection.CountDocuments(ctx, filter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to count orders",
			"error":   err.Error(),
		})
	}
	meta := fiber.Map{
		"page":  page,
		"limit": limit,
		"total": total,
		"pages": (total + int64(limit) - 1) / int64(limit),
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(int64((page - 1) * limit)).
		SetLimit(int64(limit))
	cursor, err := orderCollection.Find(ctx, filter, opts)
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
		"meta":    meta,
	})
}
