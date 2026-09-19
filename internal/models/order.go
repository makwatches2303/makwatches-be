package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// PaymentInfo represents payment information
type PaymentInfo struct {
	Method            string `json:"method" bson:"method"` // "razorpay", "card", "cod", etc.
	CardNumber        string `json:"cardNumber,omitempty" bson:"card_number,omitempty"`
	ExpiryDate        string `json:"expiryDate,omitempty" bson:"expiry_date,omitempty"`
	CVV               string `json:"cvv,omitempty" bson:"-"` // Never store CVV
	RazorpayOrderID   string `json:"razorpayOrderId,omitempty" bson:"razorpay_order_id,omitempty"`
	RazorpayPaymentID string `json:"razorpayPaymentId,omitempty" bson:"razorpay_payment_id,omitempty"`
	RazorpaySignature string `json:"razorpaySignature,omitempty" bson:"razorpay_signature,omitempty"`
}

// ShippingInfo is the denormalized shipping view carried on the order.
//
// It is provider-neutral. The original Delhivery-shaped fields are retained
// verbatim -- historical orders written before the Shiprocket integration
// still decode into this struct, and `waybill` remains the only identifier
// those documents have. New shipments populate both `waybill` and
// `tracking_number` with the AWB so old and new readers agree; use AWB()
// rather than reading either field directly.
//
// The authoritative record is the separate `shipments` collection (see
// Shipment); this copy exists so order reads need no join.
type ShippingInfo struct {
	Provider          string    `json:"provider" bson:"provider"`                                         // "shiprocket" | "delhivery"
	Waybill           string    `json:"waybill,omitempty" bson:"waybill,omitempty"`                       // Legacy/compat AWB field
	TrackingURL       string    `json:"trackingUrl,omitempty" bson:"tracking_url,omitempty"`              // Public tracking URL
	ShipmentStatus    string    `json:"shipmentStatus,omitempty" bson:"shipment_status,omitempty"`        // Current delivery status from carrier
	LastStatusUpdate  time.Time `json:"lastStatusUpdate,omitempty" bson:"last_status_update,omitempty"`   // When status was last updated
	ExpectedDelivery  string    `json:"expectedDelivery,omitempty" bson:"expected_delivery,omitempty"`    // Expected delivery date
	CurrentLocation   string    `json:"currentLocation,omitempty" bson:"current_location,omitempty"`      // Current location of package
	PickedUpAt        time.Time `json:"pickedUpAt,omitempty" bson:"picked_up_at,omitempty"`               // When package was picked up
	DeliveredAt       time.Time `json:"deliveredAt,omitempty" bson:"delivered_at,omitempty"`              // When package was delivered
	ShipmentCreatedAt time.Time `json:"shipmentCreatedAt,omitempty" bson:"shipment_created_at,omitempty"` // When shipment was created with carrier
	ShipmentError     string    `json:"shipmentError,omitempty" bson:"shipment_error,omitempty"`          // Error message if shipment creation failed
	RetryCount        int       `json:"retryCount,omitempty" bson:"retry_count,omitempty"`                // Number of times shipment creation was retried

	// Provider-neutral additions. All optional, so documents written before
	// this change decode with these left at their zero values.
	ProviderOrderID    string  `json:"providerOrderId,omitempty" bson:"provider_order_id,omitempty"`
	ProviderShipmentID string  `json:"providerShipmentId,omitempty" bson:"provider_shipment_id,omitempty"`
	TrackingNumber     string  `json:"trackingNumber,omitempty" bson:"tracking_number,omitempty"`
	CourierCompanyID   string  `json:"courierCompanyId,omitempty" bson:"courier_company_id,omitempty"`
	CourierName        string  `json:"courierName,omitempty" bson:"courier_name,omitempty"`
	StatusReason       string  `json:"statusReason,omitempty" bson:"status_reason,omitempty"`
	ShippingCharge     float64 `json:"shippingCharge,omitempty" bson:"shipping_charge,omitempty"`
	PickupLocation     string  `json:"pickupLocation,omitempty" bson:"pickup_location,omitempty"`
	ErrorCode          string  `json:"errorCode,omitempty" bson:"error_code,omitempty"`

	PickupScheduledAt time.Time `json:"pickupScheduledAt,omitempty" bson:"pickup_scheduled_at,omitempty"`
	ShippedAt         time.Time `json:"shippedAt,omitempty" bson:"shipped_at,omitempty"`
	// LastEventAt guards against out-of-order carrier callbacks.
	LastEventAt time.Time `json:"lastEventAt,omitempty" bson:"last_event_at,omitempty"`
}

// AWB returns the carrier tracking number, reading the modern field first and
// falling back to the historical `waybill`.
//
// Every caller that needs a tracking number must go through this: reading
// TrackingNumber alone silently returns empty for every Delhivery order
// created before this integration.
func (s *ShippingInfo) AWB() string {
	if s == nil {
		return ""
	}
	if s.TrackingNumber != "" {
		return s.TrackingNumber
	}
	return s.Waybill
}

// ProviderName returns the carrier for a shipment, defaulting to Delhivery.
// Orders written by the original integration always set the field, but a
// document that somehow lacks it predates Shiprocket by definition.
func (s *ShippingInfo) ProviderName() string {
	if s == nil {
		return ""
	}
	if s.Provider == "" {
		return "delhivery"
	}
	return s.Provider
}

// HasShipment reports whether anything was successfully booked with a carrier.
func (s *ShippingInfo) HasShipment() bool {
	if s == nil {
		return false
	}
	return s.AWB() != "" || s.ProviderShipmentID != "" || s.ProviderOrderID != ""
}

// Dispatch approval states.
//
// An order is created in MAK Watches first and only handed to a carrier once a
// member of staff has looked at it and said so. These are the two states that
// decision can be in; anything else -- cancelled, refunded, shipped -- is the
// order's own Status and is not duplicated here.
const (
	// DispatchPending is an order waiting for a human to review it. It is the
	// state every new order starts in.
	DispatchPending = "pending"
	// DispatchApproved means an admin has reviewed the order and nominated the
	// carrier it should be dispatched with.
	DispatchApproved = "approved"
)

// OrderApproval records the admin's explicit go-ahead to dispatch an order.
//
// # Why this is its own record rather than another order status
//
// Approval is a decision *about* fulfillment, not a stage of it. Folding it
// into Order.Status would collide with the carrier-driven states that already
// live there (a webhook moving an order to "shipped" would erase who approved
// it), and it has to survive every later status change so support can still
// answer "who sent this to Shiprocket, and when".
//
// Provider is the carrier the admin chose for dispatch. It is deliberately
// separate from the customer's own selection at checkout
// (Order.ShippingOption): the customer picks a delivery speed and price, the
// shop picks who actually carries the parcel, and the admin must be able to
// see the first while deciding the second.
//
// Every field is optional on the wire, so orders written before this existed
// decode with a nil Approval and are treated as pending -- except where a
// parcel was already booked, which is itself evidence the dispatch was
// authorised. See Order.DispatchApproved.
type OrderApproval struct {
	// Status is DispatchPending or DispatchApproved.
	Status string `json:"status" bson:"status"`
	// Provider is the carrier chosen for dispatch: "shiprocket" or "delhivery".
	Provider string `json:"provider,omitempty" bson:"provider,omitempty"`
	// ApprovedBy is the admin's user id. Hex string rather than ObjectID so a
	// record stays readable if the account is later removed.
	ApprovedBy string    `json:"approvedBy,omitempty" bson:"approved_by,omitempty"`
	ApprovedAt time.Time `json:"approvedAt,omitempty" bson:"approved_at,omitempty"`
	// Note is free text the admin may leave with the decision.
	Note string `json:"note,omitempty" bson:"note,omitempty"`
}

// ApprovalState reports the order's dispatch-approval state.
//
// A missing record reads as DispatchPending: every order placed before
// approval existed is unreviewed by definition, and defaulting the other way
// would hand a carrier a backlog of orders nobody looked at.
func (o *Order) ApprovalState() string {
	if o == nil || o.Approval == nil || o.Approval.Status == "" {
		return DispatchPending
	}
	return o.Approval.Status
}

// DispatchApproved reports whether this order may be handed to a carrier.
//
// An order that already carries a booked shipment counts as approved whatever
// its Approval record says. That is not a loophole -- it is what keeps every
// existing order fully operable: a parcel booked before this gate existed must
// still be trackable, re-AWB-able, labellable and cancellable, and each of
// those paths runs through the same authorization.
func (o *Order) DispatchApproved() bool {
	if o == nil {
		return false
	}
	if o.ApprovalState() == DispatchApproved {
		return true
	}
	return o.ShippingInfo.HasShipment()
}

// ApprovedProvider returns the carrier an admin nominated, or "" if none was
// recorded. Callers fall back to the customer's selection and then to the
// configured primary, exactly as they did before approval existed.
func (o *Order) ApprovedProvider() string {
	if o == nil || o.Approval == nil {
		return ""
	}
	return o.Approval.Provider
}

// OrderItem represents an item in an order
type OrderItem struct {
	ProductID   primitive.ObjectID `json:"productId" bson:"product_id"`
	ProductName string             `json:"productName" bson:"product_name"`
	Brand       string             `json:"brand,omitempty" bson:"brand,omitempty"`
	Image       string             `json:"image,omitempty" bson:"image,omitempty"`
	Price       float64            `json:"price" bson:"price"`
	Size        string             `json:"size,omitempty" bson:"size,omitempty"`
	Quantity    int                `json:"quantity" bson:"quantity"`
	Subtotal    float64            `json:"subtotal" bson:"subtotal"`
}

// PickupDetails represents the seller/pickup location details
type PickupDetails struct {
	LocationName string `json:"locationName,omitempty" bson:"location_name,omitempty"` // Pickup location name (e.g., "Shree Ganesh Watch")
	SellerName   string `json:"sellerName,omitempty" bson:"seller_name,omitempty"`     // Seller/business name
	Address      string `json:"address,omitempty" bson:"address,omitempty"`            // Full pickup address
	City         string `json:"city,omitempty" bson:"city,omitempty"`                  // Pickup city
	State        string `json:"state,omitempty" bson:"state,omitempty"`                // Pickup state
	Pincode      string `json:"pincode,omitempty" bson:"pincode,omitempty"`            // Pickup pincode
	Phone        string `json:"phone,omitempty" bson:"phone,omitempty"`                // Pickup contact phone
	Country      string `json:"country,omitempty" bson:"country,omitempty"`            // Pickup country
	GSTNumber    string `json:"gstNumber,omitempty" bson:"gst_number,omitempty"`       // GST number if applicable
}

// Order represents a user order
type Order struct {
	ID              primitive.ObjectID `json:"id" bson:"_id,omitempty"`         // <-- ensure json:"id"
	OrderNumber     string             `json:"orderNumber" bson:"order_number"` // Human-readable order number like MAK-20251214-001
	UserID          primitive.ObjectID `json:"userId" bson:"user_id"`           // <-- ensure json:"userId"
	Items           []OrderItem        `json:"items" bson:"items"`
	Subtotal        float64            `json:"subtotal,omitempty" bson:"subtotal,omitempty"`
	CouponCode      string             `json:"couponCode,omitempty" bson:"coupon_code,omitempty"`
	DiscountAmount  float64            `json:"discountAmount,omitempty" bson:"discount_amount,omitempty"`
	ShippingCharge  float64            `json:"shippingCharge,omitempty" bson:"shipping_charge,omitempty"`
	ShippingOption  *RateChoice        `json:"shippingOption,omitempty" bson:"shipping_option,omitempty"`
	Total           float64            `json:"total" bson:"total"`
	Status          string             `json:"status" bson:"status"`
	PaymentStatus   string             `json:"paymentStatus" bson:"payment_status"`
	ShippingAddress Address            `json:"shippingAddress" bson:"shipping_address"`
	PaymentInfo     PaymentInfo        `json:"paymentInfo" bson:"payment_info"`
	ShippingInfo    *ShippingInfo      `json:"shippingInfo,omitempty" bson:"shipping_info,omitempty"`   // Delivery tracking info
	PickupDetails   *PickupDetails     `json:"pickupDetails,omitempty" bson:"pickup_details,omitempty"` // Seller/pickup location details
	CustomerPhone   string             `json:"customerPhone,omitempty" bson:"customer_phone,omitempty"` // Customer contact for delivery
	CustomerEmail   string             `json:"customerEmail,omitempty" bson:"customer_email,omitempty"` // Customer email
	CustomerName    string             `json:"customerName,omitempty" bson:"customer_name,omitempty"`   // Customer name for delivery

	// Approval is the admin's explicit go-ahead to dispatch, and the carrier
	// they chose. Nil on orders placed before this existed, which read as
	// pending. See OrderApproval.
	Approval *OrderApproval `json:"approval,omitempty" bson:"approval,omitempty"`

	// Set when a delivery address is corrected after the order was placed
	// (PATCH /orders/:id/address). Kept so support can tell an address the
	// customer entered from one a staff member took over the phone, which
	// matters when a parcel goes to the wrong place.
	AddressUpdatedAt time.Time `json:"addressUpdatedAt,omitempty" bson:"address_updated_at,omitempty"`
	AddressUpdatedBy string    `json:"addressUpdatedBy,omitempty" bson:"address_updated_by,omitempty"`

	CreatedAt time.Time `json:"createdAt" bson:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" bson:"updated_at"`
}

// CheckoutRequest represents the data required for placing an order
type CheckoutRequest struct {
	UserID          string      `json:"userId" validate:"required"`
	ShippingAddress Address     `json:"shippingAddress" validate:"required"`
	PaymentInfo     PaymentInfo `json:"paymentInfo" validate:"required"`
	CouponCode      string      `json:"couponCode,omitempty"`
	ClientTotal     *float64    `json:"clientTotal,omitempty" bson:"-"`
	CustomerPhone   string      `json:"customerPhone,omitempty"` // For delivery contact
	CustomerEmail   string      `json:"customerEmail,omitempty"` // For delivery updates
	CustomerName    string      `json:"customerName,omitempty"`  // For delivery label

	// ShippingQuote is the opaque signed token for the delivery option the
	// customer selected. The client never sends a shipping amount: the charge
	// is read out of this token after the server re-verifies its signature and
	// its binding to this customer, cart, destination and payment mode.
	ShippingQuote string `json:"shippingQuote,omitempty" bson:"-"`
}
