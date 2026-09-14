package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// ShippingHandler serves the original flat shipping routes.
//
// Every route it exposed before is still mounted on the same path with the
// same response shape, so the live storefront and admin app keep working. What
// changed is underneath: it no longer talks to Delhivery directly but goes
// through shipping.Service, which is what makes these endpoints work for
// Shiprocket-booked orders and Delhivery-booked orders alike.
type ShippingHandler struct {
	DB      *database.DBClient
	Config  *config.Config
	Service *shipping.Service
}

// NewShippingHandler builds the handler around the shared shipping service.
func NewShippingHandler(db *database.DBClient, cfg *config.Config, svc *shipping.Service) *ShippingHandler {
	return &ShippingHandler{DB: db, Config: cfg, Service: svc}
}

// TrackShipment returns tracking for one of the caller's orders.
//
// GET /shipping/track/order/:orderID
func (h *ShippingHandler) TrackShipment(c *fiber.Ctx) error {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized",
		})
	}

	objID, err := primitive.ObjectIDFromHex(strings.TrimSpace(c.Params("orderID")))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid order ID",
		})
	}

	ctx := c.UserContext()
	var order models.Order
	if err := h.DB.Collections().Orders.FindOne(ctx, bson.M{"_id": objID}).Decode(&order); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}
	if user.Role != "admin" && order.UserID != user.UserID {
		// Answered as "not found", identically to an order that does not
		// exist. A 403 here distinguished real order ids from imaginary ones,
		// which turned this route into an enumeration oracle for other
		// customers' orders. See TestOtherUserCannotReachAnotherCustomersShipment.
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	info := order.ShippingInfo
	if !info.HasShipment() {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "No tracking information available for this order",
		})
	}

	tracking, err := h.Service.Track(ctx, &order)
	if err != nil {
		// Fall back to the last known state. An order booked a minute ago
		// legitimately has no carrier data yet, and that is not an error the
		// customer should see as a failure.
		return c.JSON(fiber.Map{
			"success": true,
			"data": fiber.Map{
				"provider":         info.ProviderName(),
				"waybill":          info.AWB(),
				"trackingNumber":   info.AWB(),
				"trackingUrl":      info.TrackingURL,
				"status":           info.ShipmentStatus,
				"courierName":      info.CourierName,
				"lastUpdate":       info.LastStatusUpdate,
				"expectedDelivery": info.ExpectedDelivery,
				"currentLocation":  info.CurrentLocation,
				"error":            "Unable to fetch live tracking, showing last known status",
			},
		})
	}
	return c.JSON(fiber.Map{"success": true, "data": tracking})
}

// TrackByWaybill returns tracking for a bare tracking number.
//
// GET /shipping/track/:waybill -- public, the same as a carrier's own tracking
// page. It resolves the shipment locally first so the lookup is routed to the
// carrier that actually holds the parcel, which is what makes a historical
// Delhivery waybill and a new Shiprocket AWB both work here.
func (h *ShippingHandler) TrackByWaybill(c *fiber.Ctx) error {
	waybill := strings.TrimSpace(c.Params("waybill"))
	if waybill == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Waybill number is required",
		})
	}

	ctx := c.UserContext()
	var order models.Order
	err := h.DB.Collections().Orders.FindOne(ctx, bson.M{
		"$or": []bson.M{
			{"shipping_info.waybill": waybill},
			{"shipping_info.tracking_number": waybill},
		},
	}).Decode(&order)
	if err != nil {
		// Not ours. Answering "not found" rather than probing every carrier
		// keeps this endpoint from being a free tracking proxy.
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Unable to track shipment",
		})
	}

	tracking, err := h.Service.Track(ctx, &order)
	if err != nil {
		return shippingError(c, err, "public tracking "+waybill)
	}
	return c.JSON(fiber.Map{"success": true, "data": tracking})
}

// CheckPincode reports whether we deliver to a pincode.
//
// GET /shipping/check-pincode/:pincode (and ?pincode=). The response keeps the
// shape the storefront already reads -- pincode/city/district/state/cod/prepaid
// -- and adds the normalized courier options alongside.
func (h *ShippingHandler) CheckPincode(c *fiber.Ctx) error {
	pincode := strings.TrimSpace(c.Params("pincode"))
	if pincode == "" {
		pincode = strings.TrimSpace(c.Query("pincode"))
	}
	if pincode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Pincode is required",
		})
	}

	// Anonymous binding: this route is public, so the quotes it returns are
	// display-only and cannot satisfy a checkout, which always presents a
	// customer and a cart.
	result, err := h.Service.Serviceability(c.UserContext(), "", shipping.RateRequest{
		DeliveryPincode: pincode,
		Package:         h.Service.DefaultPackage(),
	}, shipping.QuoteBinding{})
	if err != nil {
		se := shipping.AsError(err)
		// "We could not check" is not "we do not deliver here". Collapsing the
		// two told customers at serviceable addresses that we do not ship to
		// them whenever our carrier credentials failed.
		switch se.Code {
		case shipping.CodeNotServiceable, shipping.CodeRateUnavailable:
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success":     false,
				"serviceable": false,
				"message":     "Pincode not serviceable",
			})
		case shipping.CodeInvalidPincode:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"code":    string(se.Code),
				"message": "Please enter a valid 6-digit pincode.",
			})
		default:
			return shippingError(c, err, "check pincode "+pincode)
		}
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"serviceable": true,
		"data":        result,
	})
}

// GetCheckoutShippingOptions returns delivery courier options for cart checkout.
func (h *ShippingHandler) GetCheckoutShippingOptions(c *fiber.Ctx) error {
	var body struct {
		Pincode string `json:"pincode"`
		COD     bool   `json:"cod"`
	}
	_ = c.BodyParser(&body)
	pincode := strings.TrimSpace(body.Pincode)
	if pincode == "" {
		pincode = strings.TrimSpace(c.Query("pincode"))
	}
	if pincode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeInvalidPincode),
			"message": "Please enter a valid 6-digit pincode.",
		})
	}

	cod := body.COD
	if !cod && strings.EqualFold(c.Query("cod"), "true") {
		cod = true
	}

	ctx := c.UserContext()
	var binding shipping.QuoteBinding
	var subtotal float64

	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if (!ok || user == nil) && h.Config != nil && h.Config.JWTSecret != "" {
		if authHeader := c.Get("Authorization"); authHeader != "" {
			parts := strings.Split(authHeader, " ")
			if len(parts) == 2 && parts[0] == "Bearer" {
				token, err := jwt.Parse(parts[1], func(t *jwt.Token) (interface{}, error) {
					if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
						return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
					}
					return []byte(h.Config.JWTSecret), nil
				})
				if err == nil && token.Valid {
					if claims, ok := token.Claims.(jwt.MapClaims); ok {
						if expFloat, ok := claims["exp"].(float64); ok && !time.Now().After(time.Unix(int64(expFloat), 0)) {
							if userIDStr, ok := claims["userId"].(string); ok {
								if uid, err := primitive.ObjectIDFromHex(userIDStr); err == nil {
									role, _ := claims["role"].(string)
									if role == "" {
										role = "user"
									}
									user = &middleware.TokenMetadata{
										UserID: uid,
										Role:   role,
										Exp:    time.Unix(int64(expFloat), 0),
									}
									c.Locals("user", user)
								}
							}
						}
					}
				}
			}
		}
	}

	if user != nil {
		if lines, sub, err := cartComposition(ctx, h.DB, user.UserID); err == nil && len(lines) > 0 {
			binding = shipping.QuoteBinding{
				UserID:   user.UserID.Hex(),
				CartHash: shipping.CartFingerprint(lines),
			}
			subtotal = sub
		} else {
			binding.UserID = user.UserID.Hex()
		}
	}

	pkg := h.Service.DefaultPackage()
	binding.Pincode = pincode
	binding.WeightGrams = pkg.WeightGrams
	binding.COD = cod

	result, err := h.Service.Serviceability(ctx, "", shipping.RateRequest{
		DeliveryPincode: pincode,
		COD:             cod,
		Package:         pkg,
		DeclaredValue:   subtotal,
	}, binding)
	if err != nil {
		return shippingError(c, err, "checkout shipping options "+pincode)
	}

	// ── Apply admin-controlled tier charges ──────────────────────────────────
	// Load the tier config (defaults to ₹149/₹99/Free if not set by admin).
	tierCfg := models.DefaultShippingTierConfig()
	if h.DB != nil && h.DB.MongoDB != nil {
		_ = h.DB.MongoDB.Collection(models.ShippingTierConfigCollection).
			FindOne(ctx, bson.M{}).Decode(&tierCfg)
	}

	// Re-price each option according to its delivery speed, then re-sign the
	// quote token so the charge baked into the token matches what is displayed.
	now := time.Now()
	repriced := make([]shipping.QuotedOption, 0, len(result.Options))
	for _, opt := range result.Options {
		charge := tierChargeFor(opt.EstimatedDeliveryDays, tierCfg)
		opt.RateOption.Charge = charge
		token, signErr := h.Service.Quoter().Sign(opt.RateOption, binding, now)
		if signErr == nil {
			opt.Quote = token
		}
		repriced = append(repriced, opt)
	}
	result.Options = repriced

	return c.JSON(fiber.Map{
		"success":     true,
		"serviceable": true,
		"data":        result,
		"pincode":     result.Pincode,
		"provider":    result.Provider,
		"cod":         result.COD,
		"prepaid":     result.Prepaid,
		"options":     result.Options,
	})
}

// tierChargeFor returns the delivery surcharge for an option based on its
// estimated delivery days, using the admin-configured tier values.
//
//   - Air  : 0 < days ≤ 2  → AirCharge
//   - Express: days ≤ 3    → ExpressCharge
//   - Surface: days >= 4 or no ETA → SurfaceCharge
func tierChargeFor(days int, cfg models.ShippingTierConfig) float64 {
	if days > 0 && days <= 2 {
		return cfg.AirCharge
	}
	if days > 0 && days <= 3 {
		return cfg.ExpressCharge
	}
	return cfg.SurfaceCharge
}

// RetryShipment books a shipment for an order that has none.
//
// POST /admin/shipping/orders/:orderID/retry. Idempotent through
// shipping.Service: a repeated click cannot create a second parcel.
func (h *ShippingHandler) RetryShipment(c *fiber.Ctx) error {
	order, err := h.adminOrder(c)
	if err != nil {
		return authError(c, err)
	}

	shipment, err := h.Service.CreateShipmentForOrder(c.UserContext(), order, shipping.CreateOptions{
		AssignAWB: true,
	})
	if err != nil {
		return adminShippingError(c, err, "retry shipment for order "+order.ID.Hex())
	}
	h.invalidateOrderCache(c, order)

	if shipment.TrackingNumber == "" && shipment.ProviderShipmentID == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": "A shipment for this order is already being created",
		})
	}
	// "Created" and "ready to ship" are different states, and saying the first
	// when only the second matters is what made a half-booked order look
	// finished: the carrier had the order but no tracking number, and the
	// response still read "Shipment created successfully" with an empty
	// waybill beside an AWB_ASSIGNMENT_FAILED on the record. Report what
	// actually happened so the next action is obvious.
	if shipment.TrackingNumber == "" {
		message := "Shipment booked with the carrier, but no tracking number was assigned yet."
		if shipment.StatusReason != "" {
			message += " " + shipment.StatusReason + "."
		}
		return c.JSON(fiber.Map{
			"success":     true,
			"awbAssigned": false,
			"message":     message,
			"waybill":     "",
			"trackingUrl": "",
			"data":        shipment,
		})
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"awbAssigned": true,
		"message":     "Shipment created successfully",
		"waybill":     shipment.TrackingNumber,
		"trackingUrl": shipment.TrackingURL,
		"data":        shipment,
	})
}

// CancelShipment withdraws a shipment at the carrier.
//
// POST /admin/shipping/orders/:orderID/cancel
func (h *ShippingHandler) CancelShipment(c *fiber.Ctx) error {
	order, err := h.adminOrder(c)
	if err != nil {
		return authError(c, err)
	}
	if err := h.Service.Cancel(c.UserContext(), order); err != nil {
		return adminShippingError(c, err, "cancel shipment for order "+order.ID.Hex())
	}
	h.invalidateOrderCache(c, order)
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Shipment cancelled successfully",
	})
}

// GetShippingLabel streams the carrier label.
//
// GET /admin/shipping/orders/:orderID/label. This used to return a carrier URL
// containing our account's endpoint, which a browser could not open: the
// Delhivery packing slip requires the API token in a header. The document is
// now fetched server-side and streamed, and no provider URL or credential
// reaches the client.
func (h *ShippingHandler) GetShippingLabel(c *fiber.Ctx) error {
	order, err := h.adminOrder(c)
	if err != nil {
		return authError(c, err)
	}

	label, err := h.Service.Label(c.UserContext(), order)
	if err != nil {
		return adminShippingError(c, err, "label for order "+order.ID.Hex())
	}

	filename := label.Filename
	if filename == "" {
		if order.OrderNumber != "" {
			filename = fmt.Sprintf("label-%s.pdf", order.OrderNumber)
		} else {
			filename = fmt.Sprintf("label-%s.pdf", order.ID.Hex())
		}
	}
	c.Set(fiber.HeaderContentType, label.ContentType)
	c.Set(fiber.HeaderContentDisposition, `inline; filename="`+filename+`"`)
	c.Set(fiber.HeaderCacheControl, "no-store, private")
	return c.Send(label.Data)
}

// BulkTrackShipments tracks several of our own shipments at once.
//
// POST /admin/shipping/bulk-track. Each tracking number is resolved to an
// order first, so this cannot be used to look up arbitrary third-party
// consignments through our carrier accounts.
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
	// Bound the fan-out: each entry is a carrier round trip.
	const maxBulk = 50
	if len(req.Waybills) > maxBulk {
		req.Waybills = req.Waybills[:maxBulk]
	}

	ctx := c.UserContext()
	results := make(map[string]interface{}, len(req.Waybills))
	for _, waybill := range req.Waybills {
		waybill = strings.TrimSpace(waybill)
		if waybill == "" {
			continue
		}
		var order models.Order
		if err := h.DB.Collections().Orders.FindOne(ctx, bson.M{
			"$or": []bson.M{
				{"shipping_info.waybill": waybill},
				{"shipping_info.tracking_number": waybill},
			},
		}).Decode(&order); err != nil {
			results[waybill] = fiber.Map{"error": "no order found for this tracking number"}
			continue
		}
		tracking, err := h.Service.Track(ctx, &order)
		if err != nil {
			results[waybill] = fiber.Map{"error": shipping.AsError(err).Message}
			continue
		}
		results[waybill] = tracking
	}
	return c.JSON(fiber.Map{"success": true, "data": results})
}

// RequestPickup schedules a carrier pickup for one order's shipment.
//
// POST /admin/shipping/request-pickup. It now takes an order so the pickup can
// be made idempotent per shipment; the previous form took a date and a package
// count and could be fired repeatedly with no record of what it booked.
func (h *ShippingHandler) RequestPickup(c *fiber.Ctx) error {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || user.Role != "admin" {
		return authError(c, errNotAdmin)
	}

	var req struct {
		OrderID string `json:"orderId"`
	}
	_ = c.BodyParser(&req)
	objID, err := primitive.ObjectIDFromHex(strings.TrimSpace(req.OrderID))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "A valid orderId is required to schedule a pickup",
		})
	}

	ctx := c.UserContext()
	var order models.Order
	if err := h.DB.Collections().Orders.FindOne(ctx, bson.M{"_id": objID}).Decode(&order); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Order not found",
		})
	}

	pickup, err := h.Service.SchedulePickup(ctx, &order)
	if err != nil {
		return adminShippingError(c, err, "pickup for order "+order.ID.Hex())
	}
	message := "Pickup requested successfully"
	if pickup.AlreadyScheduled {
		message = "Pickup was already scheduled for this shipment"
	}
	return c.JSON(fiber.Map{
		"success": true,
		"message": message,
		"date":    pickup.ScheduledDate,
		"data":    pickup,
	})
}

// adminOrder loads an order for an admin-only shipping operation.
func (h *ShippingHandler) adminOrder(c *fiber.Ctx) (*models.Order, error) {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return nil, errNotAuthenticated
	}
	if user.Role != "admin" {
		return nil, errNotAdmin
	}
	objID, err := primitive.ObjectIDFromHex(strings.TrimSpace(c.Params("orderID")))
	if err != nil {
		return nil, errBadOrderID
	}
	var order models.Order
	if err := h.DB.Collections().Orders.FindOne(c.UserContext(), bson.M{"_id": objID}).Decode(&order); err != nil {
		return nil, errOrderNotVisible
	}
	return &order, nil
}

func (h *ShippingHandler) invalidateOrderCache(c *fiber.Ctx, order *models.Order) {
	ctx := c.UserContext()
	h.DB.CacheDel(ctx, "order:"+order.ID.Hex())
	h.DB.CacheDel(ctx, "orders:"+order.UserID.Hex())
}
