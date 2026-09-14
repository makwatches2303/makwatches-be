package handlers

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// ShippingV1Handler is the provider-neutral shipping surface.
//
// It holds no carrier knowledge: every operation goes through
// shipping.Service, so the same endpoint works whether the order was booked
// with Shiprocket or Delhivery.
type ShippingV1Handler struct {
	DB      *database.DBClient
	Config  *config.Config
	Service *shipping.Service
}

// NewShippingV1Handler builds the handler around an already-assembled service.
func NewShippingV1Handler(db *database.DBClient, cfg *config.Config, svc *shipping.Service) *ShippingV1Handler {
	return &ShippingV1Handler{DB: db, Config: cfg, Service: svc}
}

// shippingError renders a shipping failure without leaking provider detail.
//
// The operator detail goes to the log; the response carries only the stable
// code and the customer-safe message. A carrier error body can echo an
// address, a phone number or an account hint, none of which belongs in an API
// response.
func shippingError(c *fiber.Ctx, err error, context string) error {
	se := shipping.AsError(err)
	log.Printf("[SHIPPING-API] %s failed: code=%s detail=%s", context, se.Code, se.Detail)
	code, message := se.Public()
	return c.Status(se.HTTPStatus()).JSON(fiber.Map{
		"success": false,
		"code":    string(code),
		"message": message,
	})
}

// adminShippingError is shippingError for routes behind the admin role.
//
// Same log line, but the operator detail also goes in the response. The
// caller here is the shop's own staff on /admin/*, not a customer: when a
// booking fails, "We could not book the shipment for this order." tells them
// nothing they can act on, and the reason ("no shiprocket pickup location is
// configured", a carrier's rejection text) was reachable only by reading
// CloudWatch. The privacy reasoning on shippingError above is about not
// echoing a customer's address or phone to a *customer-facing* response;
// admin staff already see the full order.
func adminShippingError(c *fiber.Ctx, err error, context string) error {
	se := shipping.AsError(err)
	log.Printf("[SHIPPING-API] %s failed: code=%s detail=%s", context, se.Code, se.Detail)
	code, message := se.Public()
	body := fiber.Map{
		"success": false,
		"code":    string(code),
		"message": message,
	}
	if se.Detail != "" {
		body["detail"] = se.Detail
	}
	return c.Status(se.HTTPStatus()).JSON(body)
}

// ---------------------------------------------------------------- rates

// rateRequestBody is the checkout-facing serviceability request.
type rateRequestBody struct {
	DeliveryPincode string  `json:"deliveryPincode"`
	Pincode         string  `json:"pincode"`
	COD             bool    `json:"cod"`
	DeclaredValue   float64 `json:"declaredValue"`
	WeightGrams     float64 `json:"weightGrams"`
	Provider        string  `json:"provider"`
}

// Serviceability returns the delivery options for a destination.
//
// Each option carries an opaque signed quote. Creating a shipment merely
// because a customer typed a pincode would book parcels for abandoned carts,
// so this endpoint only ever reads.
func (h *ShippingV1Handler) Serviceability(c *fiber.Ctx) error {
	var body rateRequestBody
	if err := c.BodyParser(&body); err != nil {
		// Also accept a query parameter so the same handler serves a GET.
		body.DeliveryPincode = c.Query("pincode")
	}
	pincode := strings.TrimSpace(firstNonEmptyStr(body.DeliveryPincode, body.Pincode, c.Query("pincode")))
	if pincode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeInvalidPincode),
			"message": "Please enter a valid 6-digit pincode.",
		})
	}

	req := shipping.RateRequest{
		DeliveryPincode: pincode,
		COD:             body.COD,
		DeclaredValue:   body.DeclaredValue,
	}
	// The parcel is whatever operational configuration says, never what the
	// client claims: a client-chosen weight would let a customer quote a
	// lighter parcel than we ship.
	req.Package = h.Service.DefaultPackage()

	provider := strings.TrimSpace(body.Provider)
	// Anonymous binding: quotes from this public route are for display only.
	// Pricing an order requires a quote bound to the customer and their cart,
	// issued by the authenticated checkout endpoint.
	options, err := h.Service.Rates(c.UserContext(), provider, req, shipping.QuoteBinding{})
	if err != nil {
		return shippingError(c, err, "serviceability "+pincode)
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"serviceable": true,
		"data": fiber.Map{
			"pincode":  pincode,
			"provider": firstNonEmptyStr(provider, h.Service.PrimaryProviderName()),
			"options":  options,
		},
	})
}

// ---------------------------------------------------------------- authorization

// Authorization outcomes. These are returned rather than written, because
// fiber's c.JSON returns nil on success -- a handler that treated "response
// already written" as a non-nil error would fall through and answer twice.
var (
	errNotAuthenticated = errors.New("not authenticated")
	errNotAdmin         = errors.New("administrator access required")
	errBadOrderID       = errors.New("invalid order id")
	errOrderNotVisible  = errors.New("order not found")
)

// authError renders an authorization outcome.
func authError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errNotAuthenticated):
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false, "message": "Unauthorized",
		})
	case errors.Is(err, errNotAdmin):
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false, "message": "Administrator access is required",
		})
	case errors.Is(err, errBadOrderID):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false, "message": "Invalid order ID",
		})
	default:
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false, "message": "Order not found",
		})
	}
}

// orderForActor loads an order and authorizes the caller against it.
//
// adminOnly gates the fulfillment operations. Otherwise the caller must own
// the order or be an admin -- the check is against the order's stored user_id,
// never against an id supplied in the request.
func (h *ShippingV1Handler) orderForActor(c *fiber.Ctx, adminOnly bool) (*models.Order, error) {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return nil, errNotAuthenticated
	}
	if adminOnly && user.Role != "admin" {
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

	if user.Role != "admin" && order.UserID != user.UserID {
		// Deliberately the same "not found" as a missing order: distinguishing
		// them would let a caller enumerate other customers' order ids.
		return nil, errOrderNotVisible
	}
	return &order, nil
}

// ---------------------------------------------------------------- create

// CreateShipment books a parcel for an order. Admin only.
func (h *ShippingV1Handler) CreateShipment(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, true)
	if err != nil {
		return authError(c, err)
	}

	var body struct {
		Provider string `json:"provider"`
		SkipAWB  bool   `json:"skipAwb"`
	}
	_ = c.BodyParser(&body)

	// No quote is accepted here. A quote is bound to a customer and a cart, so
	// an admin has none to present; the service reuses the option the customer
	// selected and paid for, which is recorded on the order.
	opts := shipping.CreateOptions{
		Provider:  strings.TrimSpace(body.Provider),
		AssignAWB: !body.SkipAWB,
	}

	shipment, err := h.Service.CreateShipmentForOrder(c.UserContext(), order, opts)
	if err != nil {
		return shippingError(c, err, "create shipment for order "+order.ID.Hex())
	}
	h.invalidateOrderCache(c, order)
	return c.JSON(fiber.Map{"success": true, "data": shipment})
}

// CheckoutShippingOptions returns the delivery options for the caller's own
// cart, each with a quote bound to them.
//
// POST /api/v1/checkout/shipping-options -- authenticated.
//
// This is the only endpoint whose quotes can price an order. The public
// serviceability routes issue unbound quotes for display; those fail
// verification at checkout because they carry no customer and no cart. The
// binding is built here from the session and the stored cart, never from the
// request body.
func (h *ShippingV1Handler) CheckoutShippingOptions(c *fiber.Ctx) error {
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return authError(c, errNotAuthenticated)
	}

	var body struct {
		Pincode string `json:"pincode"`
		COD     bool   `json:"cod"`
	}
	_ = c.BodyParser(&body)
	pincode := strings.TrimSpace(body.Pincode)
	if pincode == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeInvalidPincode),
			"message": "Please enter a valid 6-digit pincode.",
		})
	}

	ctx := c.UserContext()
	binding, subtotal, err := h.cartBinding(ctx, user.UserID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": err.Error(),
		})
	}

	pkg := h.Service.DefaultPackage()
	result, err := h.Service.Serviceability(ctx, "", shipping.RateRequest{
		DeliveryPincode: pincode,
		COD:             body.COD,
		Package:         pkg,
		// The carrier prices insurance off the declared value, so it comes
		// from the server-priced cart, not from the client.
		DeclaredValue: subtotal,
	}, binding)
	if err != nil {
		return shippingError(c, err, "checkout shipping options "+pincode)
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"serviceable": true,
		"data":        result,
	})
}

// cartBinding builds the quote binding for a customer's current cart.
//
// The fingerprint covers composition only, so adding or removing items
// invalidates every outstanding quote and forces a re-quote before the order
// can be priced.
func (h *ShippingV1Handler) cartBinding(ctx context.Context, userID primitive.ObjectID) (shipping.QuoteBinding, float64, error) {
	lines, subtotal, err := cartComposition(ctx, h.DB, userID)
	if err != nil {
		return shipping.QuoteBinding{}, 0, err
	}
	if len(lines) == 0 {
		return shipping.QuoteBinding{}, 0, errors.New("Your bag is empty.")
	}
	return shipping.QuoteBinding{
		UserID:   userID.Hex(),
		CartHash: shipping.CartFingerprint(lines),
	}, subtotal, nil
}

// ---------------------------------------------------------------- awb

// AssignAWB attaches a courier and tracking number. Admin only.
func (h *ShippingV1Handler) AssignAWB(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, true)
	if err != nil {
		return authError(c, err)
	}
	var body struct {
		CourierID string `json:"courierId"`
	}
	_ = c.BodyParser(&body)

	shipment, err := h.Service.AssignAWBForOrder(c.UserContext(), order, strings.TrimSpace(body.CourierID))
	if err != nil {
		return shippingError(c, err, "assign awb for order "+order.ID.Hex())
	}
	h.invalidateOrderCache(c, order)
	return c.JSON(fiber.Map{"success": true, "data": shipment})
}

// ---------------------------------------------------------------- tracking

// Tracking returns live tracking for an order. Owner or admin.
//
// Both the customer view and the admin view call this, which is what removes
// the divergence the audit found between the two frontend code paths.
func (h *ShippingV1Handler) Tracking(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, false)
	if err != nil {
		return authError(c, err)
	}

	tracking, err := h.Service.Track(c.UserContext(), order)
	if err != nil {
		// Fall back to the last known state rather than failing the page: an
		// order booked a minute ago legitimately has no carrier data yet.
		if info := order.ShippingInfo; info.HasShipment() {
			return c.JSON(fiber.Map{
				"success": true,
				"data": fiber.Map{
					"provider":         info.ProviderName(),
					"trackingNumber":   info.AWB(),
					"trackingUrl":      info.TrackingURL,
					"status":           info.ShipmentStatus,
					"courierName":      info.CourierName,
					"currentLocation":  info.CurrentLocation,
					"expectedDelivery": info.ExpectedDelivery,
					"lastUpdate":       info.LastStatusUpdate,
					"stale":            true,
				},
			})
		}
		return shippingError(c, err, "tracking for order "+order.ID.Hex())
	}
	return c.JSON(fiber.Map{"success": true, "data": tracking})
}

// ---------------------------------------------------------------- cancel

// CancelShipment withdraws a shipment at the carrier. Admin only.
func (h *ShippingV1Handler) CancelShipment(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, true)
	if err != nil {
		return authError(c, err)
	}
	if err := h.Service.Cancel(c.UserContext(), order); err != nil {
		return shippingError(c, err, "cancel shipment for order "+order.ID.Hex())
	}
	h.invalidateOrderCache(c, order)
	return c.JSON(fiber.Map{"success": true, "message": "Shipment cancelled"})
}

// ---------------------------------------------------------------- label

// Label streams the carrier label through an authenticated MAK endpoint.
//
// The carrier's own label URL is never returned. Delhivery's packing slip
// requires our API token in a header, so a URL handed to a browser 401s and
// attaching the token would leak it; Shiprocket's link is an unguessable
// third-party URL. Fetching server-side and streaming the bytes is the only
// form that is both usable and safe.
func (h *ShippingV1Handler) Label(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, false)
	if err != nil {
		return authError(c, err)
	}

	label, err := h.Service.Label(c.UserContext(), order)
	if err != nil {
		return shippingError(c, err, "label for order "+order.ID.Hex())
	}

	filename := label.Filename
	if filename == "" {
		filename = "label.pdf"
	}
	c.Set(fiber.HeaderContentType, label.ContentType)
	c.Set(fiber.HeaderContentDisposition, `inline; filename="`+filename+`"`)
	// A label is per-order and authorization-gated; caching it in a shared
	// proxy would serve one customer's label to another.
	c.Set(fiber.HeaderCacheControl, "no-store, private")
	return c.Send(label.Data)
}

// ---------------------------------------------------------------- pickup

// SchedulePickup books a carrier pickup. Admin only.
func (h *ShippingV1Handler) SchedulePickup(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, true)
	if err != nil {
		return authError(c, err)
	}
	pickup, err := h.Service.SchedulePickup(c.UserContext(), order)
	if err != nil {
		return shippingError(c, err, "pickup for order "+order.ID.Hex())
	}
	message := "Pickup scheduled"
	if pickup.AlreadyScheduled {
		message = "Pickup was already scheduled for this shipment"
	}
	return c.JSON(fiber.Map{"success": true, "message": message, "data": pickup})
}

// PickupLocations lists and validates the carrier pickup locations. Admin only.
//
// Read-only by design: a pickup address is a real-world fact an operator must
// register with the carrier, not something this application should create.
func (h *ShippingV1Handler) PickupLocations(c *fiber.Ctx) error {
	provider := strings.TrimSpace(c.Query("provider"))
	locations, err := h.Service.PickupLocations(c.UserContext(), provider)
	if err != nil {
		return shippingError(c, err, "pickup locations")
	}
	configured, valid, valErr := h.Service.ValidatePickupLocation(c.UserContext(), provider)
	if valErr != nil {
		log.Printf("[SHIPPING-API] pickup location validation: %v", shipping.AsError(valErr).Detail)
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"provider":           firstNonEmptyStr(provider, h.Service.PrimaryProviderName()),
			"locations":          locations,
			"configuredLocation": configured,
			"configuredIsValid":  valid,
		},
	})
}

// invalidateOrderCache drops the cached order views a shipment change affects.
func (h *ShippingV1Handler) invalidateOrderCache(c *fiber.Ctx, order *models.Order) {
	ctx := c.UserContext()
	h.DB.CacheDel(ctx, "order:"+order.ID.Hex())
	h.DB.CacheDel(ctx, "orders:"+order.UserID.Hex())
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
