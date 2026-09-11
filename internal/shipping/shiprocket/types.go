package shiprocket

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Shiprocket is inconsistent about scalar encoding: estimated_delivery_days
// and surface_max_weight arrive as strings, rate and freight_charge as
// numbers, cod and blocked as 0/1 integers, and several fields as null. A
// strict struct fails to decode the whole response over one such field, so
// every numeric-ish field uses a tolerant type below.

// flexFloat accepts a JSON number, a numeric string, or null.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			// A non-numeric string in a numeric slot is treated as absent
			// rather than fatal: one odd field must not lose the response.
			*f = 0
			return nil
		}
		*f = flexFloat(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

func (f flexFloat) Float() float64 { return float64(f) }

// flexInt accepts a JSON number, a numeric string, a bool, or null.
type flexInt int64

func (i *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*i = 0
		return nil
	}
	if bytes.Equal(b, []byte("true")) {
		*i = 1
		return nil
	}
	if bytes.Equal(b, []byte("false")) {
		*i = 0
		return nil
	}
	var f flexFloat
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	*i = flexInt(int64(f))
	return nil
}

func (i flexInt) Int() int   { return int(i) }
func (i flexInt) Bool() bool { return i != 0 }
func (i flexInt) String() string {
	if i == 0 {
		return ""
	}
	return strconv.FormatInt(int64(i), 10)
}

// flexString accepts a JSON string or a number, and renders null as empty.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	*s = flexString(strings.Trim(string(b), `"`))
	return nil
}

func (s flexString) String() string { return string(s) }

// flexTime accepts either "2024-07-01 12:00:00" or the PHP-style object
// {"date":"...","timezone_type":3,"timezone":"Asia/Kolkata"} that Shiprocket
// uses for assigned_date_time.
type flexTime struct {
	Time  time.Time
	Valid bool
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.000000",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
	"Jan 02, 2006",
}

func parseFlexTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0000-00-00 00:00:00" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (t *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		t.Time, t.Valid = parseFlexTime(s)
		return nil
	}
	if b[0] == '{' {
		var obj struct {
			Date string `json:"date"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return nil
		}
		t.Time, t.Valid = parseFlexTime(obj.Date)
		return nil
	}
	return nil
}

// Ptr returns the parsed time or nil when absent.
func (t flexTime) Ptr() *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// ---------- serviceability ----------

// courierCompany is one entry of data.available_courier_companies.
//
// Only the fields the integration actually reads are declared; the rest of the
// (large) carrier object is ignored on purpose so a Shiprocket addition cannot
// break decoding.
type courierCompany struct {
	CourierCompanyID      flexInt    `json:"courier_company_id"`
	CourierName           flexString `json:"courier_name"`
	Rate                  flexFloat  `json:"rate"`
	FreightCharge         flexFloat  `json:"freight_charge"`
	CODCharges            flexFloat  `json:"cod_charges"`
	OtherCharges          flexFloat  `json:"other_charges"`
	COD                   flexInt    `json:"cod"`
	EstimatedDeliveryDays flexString `json:"estimated_delivery_days"`
	ETD                   flexString `json:"etd"`
	Blocked               flexInt    `json:"blocked"`
	IsSurface             flexInt    `json:"is_surface"`
	Mode                  flexString `json:"mode"`
	MinWeight             flexFloat  `json:"min_weight"`
	ChargeWeight          flexFloat  `json:"charge_weight"`
	Zone                  flexString `json:"zone"`
	SecureShipmentOff     flexInt    `json:"secure_shipment_disabled"`
}

// serviceabilityResponse is GET /courier/serviceability/.
type serviceabilityResponse struct {
	Status int `json:"status"`
	Data   struct {
		AvailableCourierCompanies []courierCompany `json:"available_courier_companies"`
		RecommendedCourierID      flexInt          `json:"recommended_courier_company_id"`
		ShiprocketRecommendedID   flexInt          `json:"shiprocket_recommended_courier_id"`
		IsRecommendationEnabled   flexInt          `json:"is_recommendation_enabled"`
	} `json:"data"`
	// Message carries the reason on a 4xx.
	Message flexString `json:"message"`
}

// ---------- create order ----------

// createOrderRequest mirrors POST /orders/create/adhoc.
//
// Almost nothing here is omitempty, and that is deliberate.
//
// Shiprocket validates these fields with Laravel's `present` rule, which
// requires the *key to exist in the payload* and says nothing about its
// value. Go's omitempty deletes the key when the value is empty, so an absent
// surname came back as:
//
//	422 {"errors":{"billing_last_name":["validation.present"]}}
//
// An empty value satisfies the rule; a missing key does not. Shiprocket's own
// documented working payload makes the same distinction -- it carries
// "shipping_customer_name": "" rather than dropping the field. So every key in
// that contract is always sent, empty when we genuinely have nothing, and
// omitempty is reserved for the handful of fields the contract does not
// mention at all.
type createOrderRequest struct {
	OrderID        string `json:"order_id"`
	OrderDate      string `json:"order_date"`
	PickupLocation string `json:"pickup_location"`
	Comment        string `json:"comment"`
	// Not part of the documented contract; sent only when configured.
	ChannelID string `json:"channel_id,omitempty"`

	BillingCustomerName string `json:"billing_customer_name"`
	BillingLastName     string `json:"billing_last_name"`
	BillingAddress      string `json:"billing_address"`
	BillingAddress2     string `json:"billing_address_2"`
	BillingCity         string `json:"billing_city"`
	BillingPincode      string `json:"billing_pincode"`
	BillingState        string `json:"billing_state"`
	BillingCountry      string `json:"billing_country"`
	BillingEmail        string `json:"billing_email"`
	BillingPhone        string `json:"billing_phone"`

	ShippingIsBilling    bool   `json:"shipping_is_billing"`
	ShippingCustomerName string `json:"shipping_customer_name"`
	ShippingLastName     string `json:"shipping_last_name"`
	ShippingAddress      string `json:"shipping_address"`
	ShippingAddress2     string `json:"shipping_address_2"`
	ShippingCity         string `json:"shipping_city"`
	ShippingPincode      string `json:"shipping_pincode"`
	ShippingState        string `json:"shipping_state"`
	ShippingCountry      string `json:"shipping_country"`
	ShippingEmail        string `json:"shipping_email"`
	ShippingPhone        string `json:"shipping_phone"`

	OrderItems []createOrderItem `json:"order_items"`

	PaymentMethod      string  `json:"payment_method"`
	ShippingCharges    float64 `json:"shipping_charges"`
	GiftwrapCharges    float64 `json:"giftwrap_charges"`
	TransactionCharges float64 `json:"transaction_charges"`
	TotalDiscount      float64 `json:"total_discount"`
	SubTotal           float64 `json:"sub_total"`

	Length  float64 `json:"length"`
	Breadth float64 `json:"breadth"`
	Height  float64 `json:"height"`
	Weight  float64 `json:"weight"`

	CustomerGSTIN string `json:"customer_gstin,omitempty"`
	InvoiceNumber string `json:"invoice_number,omitempty"`
	OrderType     string `json:"order_type,omitempty"`
}

// createOrderItem is one line of order_items.
//
// Discount, Tax and HSN are `any` because the contract sends them as an empty
// string when there is no value and as a number when there is -- exactly the
// shape of Shiprocket's own example ("discount": "", "hsn": 441122). They are
// always present for the same `present`-validation reason as the billing
// fields, and empty rather than zero: a zero tax is a claim about tax, whereas
// "" is the absence of one.
type createOrderItem struct {
	Name         string  `json:"name"`
	SKU          string  `json:"sku"`
	Units        int     `json:"units"`
	SellingPrice float64 `json:"selling_price"`
	Discount     any     `json:"discount"`
	Tax          any     `json:"tax"`
	HSN          any     `json:"hsn"`
}

// createOrderResponse is the documented success shape. awb_code,
// courier_company_id and courier_name are null on a fresh custom order --
// creation does not assign an AWB.
type createOrderResponse struct {
	OrderID          flexInt    `json:"order_id"`
	ShipmentID       flexInt    `json:"shipment_id"`
	Status           flexString `json:"status"`
	StatusCode       flexInt    `json:"status_code"`
	AWBCode          flexString `json:"awb_code"`
	CourierCompanyID flexInt    `json:"courier_company_id"`
	CourierName      flexString `json:"courier_name"`
	// Error surface on 4xx.
	Message flexString      `json:"message"`
	Errors  json.RawMessage `json:"errors"`
}

// ---------- AWB assignment ----------

type assignAWBRequest struct {
	ShipmentID string `json:"shipment_id"`
	CourierID  string `json:"courier_id,omitempty"`
}

type assignAWBResponse struct {
	AWBAssignStatus flexInt `json:"awb_assign_status"`
	Response        struct {
		// Data is an object on success and occasionally a bare string on
		// failure, so it is decoded in two passes.
		Data json.RawMessage `json:"data"`
	} `json:"response"`
	Message flexString `json:"message"`
}

type assignAWBData struct {
	CourierCompanyID flexInt    `json:"courier_company_id"`
	AWBCode          flexString `json:"awb_code"`
	OrderID          flexInt    `json:"order_id"`
	ShipmentID       flexInt    `json:"shipment_id"`
	AWBCodeStatus    flexInt    `json:"awb_code_status"`
	CourierName      flexString `json:"courier_name"`
	AssignedDateTime flexTime   `json:"assigned_date_time"`
	PickupScheduled  flexString `json:"pickup_scheduled_date"`
}

// ---------- tracking ----------

type trackShipmentEntry struct {
	AWBCode          flexString `json:"awb_code"`
	CourierCompanyID flexInt    `json:"courier_company_id"`
	ShipmentID       flexInt    `json:"shipment_id"`
	OrderID          flexInt    `json:"order_id"`
	CourierName      flexString `json:"courier_name"`
	CurrentStatus    flexString `json:"current_status"`
	Destination      flexString `json:"destination"`
	DeliveredTo      flexString `json:"delivered_to"`
	PickupDate       flexTime   `json:"pickup_date"`
	DeliveredDate    flexTime   `json:"delivered_date"`
	EDD              flexString `json:"edd"`
}

type trackActivity struct {
	Date          flexString `json:"date"`
	Status        flexString `json:"status"`
	Activity      flexString `json:"activity"`
	Location      flexString `json:"location"`
	SRStatus      flexString `json:"sr-status"`
	SRStatusLabel flexString `json:"sr-status-label"`
}

type trackingResponse struct {
	TrackingData struct {
		TrackStatus    flexInt              `json:"track_status"`
		ShipmentStatus flexInt              `json:"shipment_status"`
		ShipmentTrack  []trackShipmentEntry `json:"shipment_track"`
		Activities     []trackActivity      `json:"shipment_track_activities"`
		TrackURL       flexString           `json:"track_url"`
		ETD            flexString           `json:"etd"`
		Error          flexString           `json:"error"`
	} `json:"tracking_data"`
}

// ---------- label ----------

type labelRequest struct {
	ShipmentID []string `json:"shipment_id"`
}

type labelResponse struct {
	LabelCreated flexInt         `json:"label_created"`
	LabelURL     flexString      `json:"label_url"`
	Response     flexString      `json:"response"`
	NotCreated   json.RawMessage `json:"not_created"`
}

// ---------- pickup ----------

type pickupRequest struct {
	ShipmentID []int64 `json:"shipment_id"`
}

type pickupResponse struct {
	PickupStatus flexInt `json:"pickup_status"`
	Response     struct {
		PickupScheduledDate flexString `json:"pickup_scheduled_date"`
		PickupTokenNumber   flexString `json:"pickup_token_number"`
		Status              flexInt    `json:"status"`
		Others              flexString `json:"others"`
		Data                flexString `json:"data"`
	} `json:"response"`
	PickupGenerated flexInt         `json:"pickup_generated"`
	ManifestURL     flexString      `json:"manifest_url"`
	Message         flexString      `json:"message"`
	Errors          json.RawMessage `json:"errors"`
}

// ---------- cancel ----------

type cancelRequest struct {
	IDs []int64 `json:"ids"`
}

// ---------- pickup locations ----------

type pickupLocationsResponse struct {
	Data struct {
		ShippingAddress []struct {
			ID             flexInt    `json:"id"`
			PickupLocation flexString `json:"pickup_location"`
			Name           flexString `json:"name"`
			Address        flexString `json:"address"`
			Address2       flexString `json:"address_2"`
			City           flexString `json:"city"`
			State          flexString `json:"state"`
			Country        flexString `json:"country"`
			PinCode        flexString `json:"pin_code"`
			Phone          flexString `json:"phone"`
			Email          flexString `json:"email"`
		} `json:"shipping_address"`
	} `json:"data"`
	Message flexString `json:"message"`
}

// ---------- webhook ----------

// webhookPayload is the documented Shiprocket shipping webhook body.
type webhookPayload struct {
	AWB              flexString `json:"awb"`
	CourierName      flexString `json:"courier_name"`
	CurrentStatus    flexString `json:"current_status"`
	CurrentStatusID  flexInt    `json:"current_status_id"`
	ShipmentStatus   flexString `json:"shipment_status"`
	ShipmentStatusID flexInt    `json:"shipment_status_id"`
	CurrentTimestamp flexString `json:"current_timestamp"`
	// OrderID is Shiprocket's own order id in the documented webhook payload,
	// NOT our merchant reference -- that arrives as channel_order_id. The two
	// were conflated here, which dropped the carrier's order id and put a
	// Shiprocket number where our order number belongs.
	OrderID flexString `json:"order_id"`
	// ChannelOrderID is the reference we sent at creation (MAK-YYYYMMDD-NNN).
	ChannelOrderID flexString `json:"channel_order_id"`
	Channel        flexString `json:"channel"`
	// SROrderID and ShipmentID are absent from the documented sample but
	// appear on some events; preferred when present because they are
	// unambiguous.
	SROrderID       flexInt         `json:"sr_order_id"`
	ShipmentID      flexInt         `json:"shipment_id"`
	AWBAssignedDate flexString      `json:"awb_assigned_date"`
	PickupScheduled flexString      `json:"pickup_scheduled_date"`
	ETD             flexString      `json:"etd"`
	IsReturn        flexInt         `json:"is_return"`
	ChannelID       flexString      `json:"channel_id"`
	PODStatus       flexString      `json:"pod_status"`
	Scans           []webhookScan   `json:"scans"`
	QCFailureReason flexString      `json:"qc_failure_reason"`
	Extra           json.RawMessage `json:"-"`
}

type webhookScan struct {
	Date     flexString `json:"date"`
	Activity flexString `json:"activity"`
	Location flexString `json:"location"`
	Status   flexString `json:"status"`
	SRStatus flexString `json:"sr-status"`
}
