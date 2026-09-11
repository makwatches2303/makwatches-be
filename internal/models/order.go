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
	ID          primitive.ObjectID `json:"id" bson:"_id,omitempty"`         // <-- ensure json:"id"
	OrderNumber string             `json:"orderNumber" bson:"order_number"` // Human-readable order number like MAK-20251214-001
	UserID      primitive.ObjectID `json:"userId" bson:"user_id"`           // <-- ensure json:"userId"
	Items       []OrderItem        `json:"items" bson:"items"`
	// Total is the amount charged: Subtotal + ShippingCharge.
	//
	// Orders written before shipping was charged carry Total == items and no
	// Subtotal, which is still accurate for them -- shipping was zero. No
	// migration is required.
	Total float64 `json:"total" bson:"total"`
	// Subtotal is the goods total, before shipping.
	Subtotal float64 `json:"subtotal,omitempty" bson:"subtotal,omitempty"`
	// ShippingCharge is the validated carrier charge the customer paid.
	ShippingCharge float64 `json:"shippingCharge,omitempty" bson:"shipping_charge,omitempty"`
	// ShippingOption is the delivery choice as quoted at purchase time.
	//
	// Persisted so the order stays explainable later without asking a carrier
	// what it charged months ago, and so an admin retry books the same courier
	// the customer actually chose.
	ShippingOption  *RateChoice    `json:"shippingOption,omitempty" bson:"shipping_option,omitempty"`
	Status          string         `json:"status" bson:"status"`
	PaymentStatus   string         `json:"paymentStatus" bson:"payment_status"`
	ShippingAddress Address        `json:"shippingAddress" bson:"shipping_address"`
	PaymentInfo     PaymentInfo    `json:"paymentInfo" bson:"payment_info"`
	ShippingInfo    *ShippingInfo  `json:"shippingInfo,omitempty" bson:"shipping_info,omitempty"`   // Delivery tracking info
	PickupDetails   *PickupDetails `json:"pickupDetails,omitempty" bson:"pickup_details,omitempty"` // Seller/pickup location details
	CustomerPhone   string         `json:"customerPhone,omitempty" bson:"customer_phone,omitempty"` // Customer contact for delivery
	CustomerEmail   string         `json:"customerEmail,omitempty" bson:"customer_email,omitempty"` // Customer email
	CustomerName    string         `json:"customerName,omitempty" bson:"customer_name,omitempty"`   // Customer name for delivery
	CreatedAt       time.Time      `json:"createdAt" bson:"created_at"`
	UpdatedAt       time.Time      `json:"updatedAt" bson:"updated_at"`
}

// CheckoutRequest represents the data required for placing an order
type CheckoutRequest struct {
	UserID          string      `json:"userId" validate:"required"`
	ShippingAddress Address     `json:"shippingAddress" validate:"required"`
	PaymentInfo     PaymentInfo `json:"paymentInfo" validate:"required"`
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
