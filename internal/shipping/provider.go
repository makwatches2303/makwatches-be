package shipping

import (
	"context"
	"strings"
	"time"
)

// Provider names. These are persisted on every shipment record, so they are
// part of the data contract and must not be renamed -- historical Delhivery
// orders carry ProviderDelhivery and stay readable forever.
const (
	ProviderShiprocket = "shiprocket"
	ProviderDelhivery  = "delhivery"
)

// Fulfillment statuses. Deliberately the same vocabulary the orders collection
// already uses, so existing documents, the admin UI and the storefront keep
// working without a migration.
const (
	StatusPending        = "pending"
	StatusManifested     = "manifested"
	StatusPendingPickup  = "pending_pickup"
	StatusPickedUp       = "picked_up"
	StatusInTransit      = "in_transit"
	StatusOutForDelivery = "out_for_delivery"
	StatusDelivered      = "delivered"
	StatusUndelivered    = "undelivered"
	StatusReturned       = "returned"
	StatusCancelled      = "cancelled"
	StatusFailed         = "failed"
	StatusProcessing     = "processing"
)

// PackageSpec is the physical parcel. Weight is grams and dimensions are
// centimetres throughout the subsystem; each adapter converts to whatever its
// carrier expects at the wire boundary, so no caller has to remember units.
type PackageSpec struct {
	WeightGrams float64
	LengthCm    float64
	BreadthCm   float64
	HeightCm    float64
	// Defaults records that these came from operational configuration rather
	// than from measured product data, so the report and the shipment record
	// can say so honestly.
	Defaults bool
}

// RateRequest asks a carrier what it would charge to move a parcel.
type RateRequest struct {
	PickupPincode   string
	DeliveryPincode string
	COD             bool
	Package         PackageSpec
	// DeclaredValue is the order subtotal in rupees, used by carriers for
	// insurance banding. Zero means "do not declare".
	DeclaredValue float64
}

// RateOption is one delivery choice, normalized across carriers.
//
// Only fields the carrier actually returned are populated. An absent ETA stays
// the zero value rather than being invented, because a fabricated delivery
// promise shown at checkout is worse than no promise at all.
type RateOption struct {
	ID                    string  `json:"id"`
	Provider              string  `json:"provider"`
	ProviderCourierID     string  `json:"providerCourierId"`
	CourierName           string  `json:"courierName"`
	Charge                float64 `json:"charge"`
	EstimatedDeliveryDays int     `json:"estimatedDeliveryDays,omitempty"`
	ETD                   string  `json:"etd,omitempty"`
	CODAvailable          bool    `json:"codAvailable"`
	Mode                  string  `json:"mode,omitempty"`
	// Recommended is set only when the carrier itself nominated this option.
	// It is never inferred from array position.
	Recommended bool `json:"recommended,omitempty"`
}

// ShipmentAddress is the destination, already validated by the order.
type ShipmentAddress struct {
	Name    string
	Line1   string
	Line2   string
	City    string
	State   string
	Pincode string
	Country string
	Phone   string
	Email   string
}

// ShipmentLine is one order line as the carrier needs to see it.
//
// SKU, HSN and tax are only set when the order genuinely carries them. Sending
// a placeholder HSN to a carrier that forwards it to customs is worse than
// omitting an optional field.
type ShipmentLine struct {
	Name         string
	SKU          string
	Units        int
	SellingPrice float64
	Discount     float64
	Tax          float64
	HSN          string
}

// CreateShipmentRequest is the provider-neutral booking request. It is built
// once, by ShippingService, from the authoritative order.
type CreateShipmentRequest struct {
	// OrderRef is the human-readable MAK order number (MAK-YYYYMMDD-NNN). It is
	// what appears on the label and in carrier dashboards.
	OrderRef  string
	OrderDate time.Time

	PickupLocation string

	Billing  ShipmentAddress
	Shipping ShipmentAddress
	// ShippingIsBilling is true when we hold one address for both, which is the
	// normal case for this store.
	ShippingIsBilling bool

	Lines []ShipmentLine

	// PaymentMode is "Prepaid" or "COD" in neutral terms; adapters translate.
	COD       bool
	CODAmount float64

	SubTotal       float64
	ShippingCharge float64
	TotalDiscount  float64

	Package PackageSpec

	// CourierID is the option the customer selected at checkout, already
	// re-validated server-side. Empty means "let the service assign later".
	CourierID string

	// SellerGSTIN and CustomerGSTIN are forwarded only when configured.
	SellerGSTIN   string
	CustomerGSTIN string
	InvoiceNumber string
}

// CreateShipmentResult is what a carrier gives back for a booking.
//
// The three identifiers are kept apart on purpose. Shiprocket's order_id,
// its shipment_id and the courier's AWB are different numbers with different
// lifetimes, and the previous single "waybill" field could not represent that.
type CreateShipmentResult struct {
	ProviderOrderID    string
	ProviderShipmentID string
	// TrackingNumber is the AWB. Delhivery returns it here directly; Shiprocket
	// usually leaves it empty until AssignAWB runs.
	TrackingNumber   string
	CourierCompanyID string
	CourierName      string
	Status           string
	TrackingURL      string
}

// AssignAWBRequest asks the carrier to attach a courier and an AWB to an
// already-created shipment.
type AssignAWBRequest struct {
	ProviderShipmentID string
	CourierID          string
}

// AssignAWBResult is the assigned courier and AWB.
type AssignAWBResult struct {
	TrackingNumber   string
	CourierCompanyID string
	CourierName      string
	AssignedAt       time.Time
	TrackingURL      string
}

// ShipmentRef addresses a shipment at a carrier. Adapters prefer the provider
// shipment id where the carrier supports it and fall back to the AWB.
type ShipmentRef struct {
	ProviderOrderID    string
	ProviderShipmentID string
	TrackingNumber     string
}

// TrackingScan is one normalized carrier scan event.
type TrackingScan struct {
	At       time.Time `json:"at"`
	Status   string    `json:"status"`
	Location string    `json:"location,omitempty"`
	Remarks  string    `json:"remarks,omitempty"`
}

// Tracking is the normalized tracking view. The raw provider payload is
// deliberately absent: it leaks internal courier ids, phone numbers and
// occasionally account metadata.
type Tracking struct {
	Provider         string         `json:"provider"`
	TrackingNumber   string         `json:"trackingNumber,omitempty"`
	CourierName      string         `json:"courierName,omitempty"`
	Status           string         `json:"status"`
	StatusDetail     string         `json:"statusDetail,omitempty"`
	CurrentLocation  string         `json:"currentLocation,omitempty"`
	ExpectedDelivery string         `json:"expectedDelivery,omitempty"`
	TrackingURL      string         `json:"trackingUrl,omitempty"`
	PickedUpAt       *time.Time     `json:"pickedUpAt,omitempty"`
	DeliveredAt      *time.Time     `json:"deliveredAt,omitempty"`
	LastEventAt      *time.Time     `json:"lastEventAt,omitempty"`
	Scans            []TrackingScan `json:"scans,omitempty"`
}

// Label is a shipping label already fetched as bytes.
//
// Adapters return the document itself rather than a URL. A carrier label URL
// is either bearer-authenticated (Delhivery's packing slip) or an unguessable
// third-party link, and neither should be handed to a browser.
type Label struct {
	ContentType string
	Filename    string
	Data        []byte
}

// Pickup is a scheduled carrier pickup.
type Pickup struct {
	Token         string
	ScheduledDate string
	ScheduledAt   *time.Time
	ManifestURL   string
	// AlreadyScheduled is true when the carrier reported that this shipment was
	// already in a pickup, which the service treats as success.
	AlreadyScheduled bool
}

// PickupLocation is a pickup point registered at the carrier.
type PickupLocation struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	City    string `json:"city,omitempty"`
	State   string `json:"state,omitempty"`
	Pincode string `json:"pincode,omitempty"`
	Phone   string `json:"phone,omitempty"`
}

// WebhookEvent is a validated, normalized carrier status callback.
type WebhookEvent struct {
	Provider           string
	ProviderOrderID    string
	ProviderShipmentID string
	TrackingNumber     string
	// OrderRef is the merchant reference the carrier echoed back. It is a hint
	// for lookup only and is never trusted on its own to identify an order.
	OrderRef       string
	Status         string
	StatusDetail   string
	CourierName    string
	Location       string
	OccurredAt     time.Time
	ExpectedDate   string
	IsReturn       bool
	Scans          []TrackingScan
	DeliveredAt    *time.Time
	PickedUpAt     *time.Time
	PickupSchedule string
	// EventKey is a stable fingerprint of this exact event, used to drop
	// duplicate deliveries without re-applying them.
	EventKey string
}

// Provider is one carrier behind a single neutral contract.
//
// Every method takes a context so the HTTP layer's deadline reaches the
// carrier call, and returns *Error (via the helpers in errors.go) so the
// handler never has to know which carrier failed.
//
// A carrier that lacks an operation returns ErrUnsupported rather than a
// fabricated success.
type Provider interface {
	Name() string

	Rates(ctx context.Context, req RateRequest) ([]RateOption, error)
	CreateShipment(ctx context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error)
	AssignAWB(ctx context.Context, req AssignAWBRequest) (*AssignAWBResult, error)
	Track(ctx context.Context, ref ShipmentRef) (*Tracking, error)
	Cancel(ctx context.Context, ref ShipmentRef) error
	Label(ctx context.Context, ref ShipmentRef) (*Label, error)
	SchedulePickup(ctx context.Context, ref ShipmentRef) (*Pickup, error)
	PickupLocations(ctx context.Context) ([]PickupLocation, error)

	// ParseWebhook authenticates and normalizes a callback. It must reject an
	// unauthenticated or malformed payload before any state is read or written.
	ParseWebhook(headers map[string]string, body []byte) (*WebhookEvent, error)
}

// StatusRank exposes how far along a fulfillment state is, for callers that
// need to gate on "has the parcel physically moved yet" -- correcting a
// delivery address, for one, which stops being meaningful the moment a
// courier has the box in hand. An unknown status ranks -1 rather than 0, so
// it is never mistaken for "not started yet".
func StatusRank(status string) int {
	if r, ok := statusRank[strings.ToLower(strings.TrimSpace(status))]; ok {
		return r
	}
	return -1
}

// statusRank orders fulfillment states so a late or replayed carrier callback
// cannot walk an order backwards. Terminal states share the top rank and are
// governed by allowedTransition instead.
var statusRank = map[string]int{
	StatusPending:        0,
	StatusProcessing:     1,
	StatusManifested:     2,
	StatusPendingPickup:  3,
	StatusPickedUp:       4,
	StatusInTransit:      5,
	StatusOutForDelivery: 6,
	StatusUndelivered:    6,
	StatusDelivered:      8,
	StatusReturned:       8,
	StatusCancelled:      8,
	StatusFailed:         8,
}

// allowedTransition lists the moves that are legitimate even though they do
// not increase rank. An RTO genuinely follows a delivery attempt, and a
// delivered parcel can still come back.
var allowedTransition = map[string]map[string]bool{
	StatusDelivered:   {StatusReturned: true},
	StatusUndelivered: {StatusInTransit: true, StatusOutForDelivery: true, StatusReturned: true},
}

// CanAdvance reports whether a carrier event moving from -> to should be
// applied. Equal states are allowed so a repeated event can still refresh
// location and timestamps without changing the status.
func CanAdvance(from, to string) bool {
	if from == "" || from == to {
		return true
	}
	if allowedTransition[from][to] {
		return true
	}
	fr, okFrom := statusRank[from]
	tr, okTo := statusRank[to]
	if !okFrom || !okTo {
		// An unrecognized state is not a licence to rewind.
		return !okFrom
	}
	return tr > fr
}
