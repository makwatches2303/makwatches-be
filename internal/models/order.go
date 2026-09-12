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

// ShippingInfo represents shipping/delivery information from Delhivery
type ShippingInfo struct {
	Provider          string    `json:"provider" bson:"provider"`                                         // "delhivery"
	Waybill           string    `json:"waybill,omitempty" bson:"waybill,omitempty"`                       // Delhivery waybill/tracking number
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
	CreatedAt       time.Time          `json:"createdAt" bson:"created_at"`
	UpdatedAt       time.Time          `json:"updatedAt" bson:"updated_at"`
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
}
