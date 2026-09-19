package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Shipment is the authoritative record of one parcel booked with one carrier.
//
// It lives in its own collection rather than only inside the order because the
// pair (order_id, provider) carries a unique index, and that index is what
// makes shipment creation idempotent: a duplicate checkout goroutine, an admin
// double-click and a retried request all collide on insert instead of booking
// a second parcel. Orders keep their embedded ShippingInfo as the denormalized
// read model, so every existing reader -- including historical Delhivery
// orders that have no Shipment document at all -- keeps working unchanged.
type Shipment struct {
	ID          primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	OrderID     primitive.ObjectID `json:"orderId" bson:"order_id"`
	OrderNumber string             `json:"orderNumber,omitempty" bson:"order_number,omitempty"`
	UserID      primitive.ObjectID `json:"userId,omitempty" bson:"user_id,omitempty"`

	Provider string `json:"provider" bson:"provider"`

	// The three carrier identifiers, kept separate on purpose. A Shiprocket
	// order id, its shipment id and the courier's AWB are different numbers
	// with different lifetimes.
	ProviderOrderID    string `json:"providerOrderId,omitempty" bson:"provider_order_id,omitempty"`
	ProviderShipmentID string `json:"providerShipmentId,omitempty" bson:"provider_shipment_id,omitempty"`
	TrackingNumber     string `json:"trackingNumber,omitempty" bson:"tracking_number,omitempty"`

	CourierCompanyID string `json:"courierCompanyId,omitempty" bson:"courier_company_id,omitempty"`
	CourierName      string `json:"courierName,omitempty" bson:"courier_name,omitempty"`
	TrackingURL      string `json:"trackingUrl,omitempty" bson:"tracking_url,omitempty"`

	Status       string `json:"status" bson:"status"`
	StatusReason string `json:"statusReason,omitempty" bson:"status_reason,omitempty"`

	// Snapshot fields. These record what was true when the shipment was booked
	// so historical orders never depend on a future carrier API query to be
	// reconstructed.
	PickupLocation   string      `json:"pickupLocation,omitempty" bson:"pickup_location,omitempty"`
	ShippingCharge   float64     `json:"shippingCharge" bson:"shipping_charge"`
	SelectedOption   *RateChoice `json:"selectedOption,omitempty" bson:"selected_option,omitempty"`
	PackageWeightG   float64     `json:"packageWeightGrams,omitempty" bson:"package_weight_grams,omitempty"`
	PackageLengthCm  float64     `json:"packageLengthCm,omitempty" bson:"package_length_cm,omitempty"`
	PackageBreadthCm float64     `json:"packageBreadthCm,omitempty" bson:"package_breadth_cm,omitempty"`
	PackageHeightCm  float64     `json:"packageHeightCm,omitempty" bson:"package_height_cm,omitempty"`
	// PackageIsDefault records that the parcel figures came from operational
	// configuration, not from measured product data.
	PackageIsDefault bool `json:"packageIsDefault,omitempty" bson:"package_is_default,omitempty"`

	PickupToken       string     `json:"pickupToken,omitempty" bson:"pickup_token,omitempty"`
	ManifestURL       string     `json:"manifestUrl,omitempty" bson:"manifest_url,omitempty"`
	PickupScheduledAt *time.Time `json:"pickupScheduledAt,omitempty" bson:"pickup_scheduled_at,omitempty"`
	ShippedAt         *time.Time `json:"shippedAt,omitempty" bson:"shipped_at,omitempty"`
	DeliveredAt       *time.Time `json:"deliveredAt,omitempty" bson:"delivered_at,omitempty"`

	// LastEventAt and LastEventKey make webhook processing idempotent and
	// monotonic: a replayed delivery does not re-apply, and an event older
	// than the one already stored does not move the order backwards.
	LastEventAt  *time.Time `json:"lastEventAt,omitempty" bson:"last_event_at,omitempty"`
	LastEventKey string     `json:"-" bson:"last_event_key,omitempty"`

	Error      string `json:"error,omitempty" bson:"error,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty" bson:"error_code,omitempty"`
	RetryCount int    `json:"retryCount,omitempty" bson:"retry_count,omitempty"`

	CreatedAt time.Time `json:"createdAt" bson:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" bson:"updated_at"`
}

// RateChoice is the shipping option the customer actually selected, persisted
// so the order's shipping charge remains explainable after the fact.
type RateChoice struct {
	OptionID              string  `json:"id,omitempty" bson:"option_id,omitempty"`
	Provider              string  `json:"provider,omitempty" bson:"provider,omitempty"`
	ProviderCourierID     string  `json:"providerCourierId,omitempty" bson:"provider_courier_id,omitempty"`
	CourierName           string  `json:"courierName,omitempty" bson:"courier_name,omitempty"`
	Charge                float64 `json:"charge" bson:"charge"`
	EstimatedDeliveryDays int     `json:"estimatedDeliveryDays,omitempty" bson:"estimated_delivery_days,omitempty"`
	ETD                   string  `json:"etd,omitempty" bson:"etd,omitempty"`
	CODAvailable          bool    `json:"codAvailable,omitempty" bson:"cod_available,omitempty"`

	// DeliveryTier is the delivery *speed* the customer chose, in the terms
	// they were shown it: express, standard, economy or flexible. Derived
	// server-side from the signed quote's day estimate (see DeliveryTierFor)
	// and stored so the order keeps meaning what the shopper was offered, even
	// if the thresholds are retuned later.
	//
	// Deliberately not a carrier fact. Provider and ProviderCourierID above
	// stay the internal record of who was quoted; which carrier actually
	// carries the parcel is a separate decision an admin makes at approval
	// time, and this field never constrains it.
	//
	// Absent on orders placed before this existed; DeliveryTierOf derives it
	// from EstimatedDeliveryDays for those.
	DeliveryTier string `json:"deliveryTier,omitempty" bson:"delivery_tier,omitempty"`
}

// AWB returns the carrier tracking number for a shipment, tolerating the
// historical Delhivery shape where the only identifier was the waybill.
func (s *Shipment) AWB() string {
	if s == nil {
		return ""
	}
	return s.TrackingNumber
}
