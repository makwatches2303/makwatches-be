package delhivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// Config is what the Delhivery adapter needs beyond the credentials already
// held by the underlying service.
type Config struct {
	// FlatShippingCharge is what we charge a customer for a Delhivery
	// shipment. The Delhivery integration on this account exposes pincode
	// serviceability but no rate card, so there is no carrier-quoted price to
	// normalize. This value comes from operational configuration and defaults
	// to zero, which is the current business reality (shipping is included in
	// the item price). It is never presented as a carrier quote.
	FlatShippingCharge float64
	// WebhookSecret is a shared token this backend requires on the Delhivery
	// callback. Delhivery does not sign its webhooks, so a secret we control
	// is the only authentication available -- see ParseWebhook.
	WebhookSecret string
	// PickupLocation mirrors the registered location name.
	PickupLocation string
	SellerGSTIN    string
}

// Provider adapts the existing Delhivery service to the neutral contract.
//
// It deliberately delegates to services.DelhiveryService rather than
// reimplementing it: that code books real shipments today, and historical
// orders were created by it. Provider-specific authentication and request
// mapping stay inside this adapter -- Delhivery keeps its static API token and
// never touches Shiprocket's token logic.
type Provider struct {
	svc *services.DelhiveryService
	cfg Config
	now func() time.Time
}

// New wraps an already-configured Delhivery service.
func New(svc *services.DelhiveryService, cfg Config) *Provider {
	return &Provider{svc: svc, cfg: cfg, now: time.Now}
}

func (p *Provider) Name() string { return shipping.ProviderDelhivery }

// checkCtx honours a caller's cancellation before starting carrier I/O.
//
// The underlying service predates context plumbing and enforces its own 30s
// client timeout. Checking here means a caller who has already given up does
// not trigger a fresh carrier call; it does not abort one already in flight.
func checkCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return shipping.Wrap(shipping.CodeShippingUnavailable, err, "request cancelled before carrier call")
	}
	return nil
}

// Rates reports whether Delhivery serves a pincode.
//
// It returns at most one option, priced from configuration rather than from
// the carrier, because this integration has no rate API. The ETA is left
// empty: Delhivery's serviceability response carries no delivery estimate and
// inventing one would put a promise on the checkout page that nothing backs.
func (p *Provider) Rates(ctx context.Context, req shipping.RateRequest) ([]shipping.RateOption, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(req.DeliveryPincode)) != 6 {
		return nil, shipping.Errf(shipping.CodeInvalidPincode,
			"delivery pincode %q is not six digits", req.DeliveryPincode)
	}

	svc, err := p.svc.CheckPincodeServiceability(strings.TrimSpace(req.DeliveryPincode))
	if err != nil {
		// The existing service already separates "the carrier says no" from
		// "we could not ask", and that distinction must survive here: telling
		// a customer we do not deliver, when the truth is that our token
		// failed, loses the order for no reason.
		if errors.Is(err, services.ErrPincodeNotServiceable) {
			return nil, shipping.Wrap(shipping.CodeNotServiceable, err,
				"delhivery reports pincode %s unserviceable", req.DeliveryPincode)
		}
		return nil, shipping.Wrap(shipping.CodeShippingUnavailable, err,
			"delhivery serviceability check failed for %s", req.DeliveryPincode)
	}

	// A COD order to a prepaid-only pincode has no valid option.
	if req.COD && !svc.COD {
		return nil, shipping.Errf(shipping.CodeRateUnavailable,
			"delhivery does not offer COD at pincode %s", req.DeliveryPincode)
	}
	if !req.COD && !svc.Prepaid {
		return nil, shipping.Errf(shipping.CodeRateUnavailable,
			"delhivery does not offer prepaid delivery at pincode %s", req.DeliveryPincode)
	}
	if req.COD && svc.MaxAmount > 0 && req.DeclaredValue > float64(svc.MaxAmount) {
		return nil, shipping.Errf(shipping.CodeRateUnavailable,
			"order value exceeds the delhivery COD limit at pincode %s", req.DeliveryPincode)
	}

	return []shipping.RateOption{{
		ID:                shipping.OptionID(shipping.ProviderDelhivery, "surface"),
		Provider:          shipping.ProviderDelhivery,
		ProviderCourierID: "surface",
		CourierName:       "Delhivery Surface",
		Charge:            p.cfg.FlatShippingCharge,
		CODAvailable:      svc.COD,
		Mode:              "Surface",
	}}, nil
}

// ResolveDestination names the locality behind a pincode.
//
// Delhivery's pin-code API reports a district and a state code but no separate
// city, so the district is reported as the city -- that is what the field
// means here, and inventing a distinct city name would be worse than repeating
// the one fact the carrier gave us. Implements shipping.DestinationResolver.
func (p *Provider) ResolveDestination(ctx context.Context, pincode string) (string, string, string, error) {
	if err := checkCtx(ctx); err != nil {
		return "", "", "", err
	}
	svc, err := p.svc.CheckPincodeServiceability(strings.TrimSpace(pincode))
	if err != nil {
		return "", "", "", shipping.Wrap(shipping.CodeShippingUnavailable, err,
			"delhivery could not resolve pincode %s", pincode)
	}
	return svc.City, svc.District, svc.State, nil
}

// CreateShipment manifests the parcel with Delhivery.
func (p *Provider) CreateShipment(ctx context.Context, req shipping.CreateShipmentRequest) (*shipping.CreateShipmentResult, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}

	dest := req.Shipping
	if req.ShippingIsBilling {
		dest = req.Billing
	}

	var names []string
	quantity := 0
	for _, line := range req.Lines {
		names = append(names, line.Name)
		quantity += line.Units
	}
	desc := strings.Join(names, ", ")
	if len(desc) > 200 {
		desc = desc[:197] + "..."
	}

	address := strings.TrimSpace(dest.Line1)
	if l2 := strings.TrimSpace(dest.Line2); l2 != "" {
		address = address + ", " + l2
	}

	paymentMode := "Prepaid"
	if req.COD {
		paymentMode = "COD"
	}

	svcReq := services.CreateShipmentRequest{
		CustomerName:    dest.Name,
		CustomerPhone:   dest.Phone,
		CustomerEmail:   dest.Email,
		CustomerAddress: address,
		CustomerCity:    dest.City,
		CustomerState:   dest.State,
		CustomerPincode: dest.Pincode,
		CustomerCountry: firstNonEmpty(dest.Country, "India"),

		OrderID:         req.OrderRef,
		OrderDate:       req.OrderDate.Format("2006-01-02"),
		TotalAmount:     req.SubTotal + req.ShippingCharge,
		PaymentMode:     paymentMode,
		CODAmount:       req.CODAmount,
		ProductQuantity: quantity,
		ProductDesc:     desc,

		// Grams and centimetres, matching the neutral PackageSpec. The
		// underlying service converts to kilograms at the wire boundary.
		Weight:  req.Package.WeightGrams,
		Length:  req.Package.LengthCm,
		Breadth: req.Package.BreadthCm,
		Height:  req.Package.HeightCm,

		SellerGSTIN: firstNonEmpty(req.SellerGSTIN, p.cfg.SellerGSTIN),
	}
	for _, line := range req.Lines {
		svcReq.Items = append(svcReq.Items, services.ShipmentItem{
			Name:     line.Name,
			SKU:      line.SKU,
			Quantity: line.Units,
			Price:    line.SellingPrice,
		})
	}

	resp, err := p.svc.CreateShipment(svcReq)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeShipmentCreationFailed, err,
			"delhivery shipment creation failed for order %s", req.OrderRef)
	}
	if resp == nil || strings.TrimSpace(resp.Waybill) == "" {
		return nil, shipping.Errf(shipping.CodeShipmentCreationFailed,
			"delhivery returned no waybill for order %s", req.OrderRef)
	}

	awb := strings.TrimSpace(resp.Waybill)
	return &shipping.CreateShipmentResult{
		// Delhivery has no separate order/shipment identifiers: the waybill is
		// the only handle it issues, so it is recorded as both the shipment id
		// and the tracking number. The order reference stays our own number.
		ProviderOrderID:    req.OrderRef,
		ProviderShipmentID: awb,
		TrackingNumber:     awb,
		CourierName:        "Delhivery",
		Status:             shipping.StatusManifested,
		TrackingURL:        TrackingURL(awb),
	}, nil
}

// AssignAWB is not a step Delhivery has.
//
// Manifesting a parcel already returns its waybill, so there is nothing to
// assign afterwards. Reporting this as unsupported lets ShippingService skip
// the step rather than treat a missing capability as a failure.
func (p *Provider) AssignAWB(context.Context, shipping.AssignAWBRequest) (*shipping.AssignAWBResult, error) {
	return nil, shipping.ErrUnsupported
}

// Track fetches and normalizes Delhivery tracking.
func (p *Provider) Track(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Tracking, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	awb := firstNonEmpty(ref.TrackingNumber, ref.ProviderShipmentID)
	if awb == "" {
		return nil, shipping.Errf(shipping.CodeShipmentNotFound,
			"no delhivery waybill to track")
	}

	status, err := p.svc.TrackShipment(awb)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeTrackingUnavailable, err,
			"delhivery tracking failed for waybill %s", awb)
	}

	tracking := &shipping.Tracking{
		Provider:         shipping.ProviderDelhivery,
		TrackingNumber:   awb,
		CourierName:      "Delhivery",
		Status:           status.ShipmentStatus,
		StatusDetail:     status.Status,
		CurrentLocation:  status.StatusLocation,
		ExpectedDelivery: status.ExpectedDelivery,
		TrackingURL:      TrackingURL(awb),
	}
	for _, scan := range status.Scans {
		at, _ := parseTime(scan.ScanDateTime)
		tracking.Scans = append(tracking.Scans, shipping.TrackingScan{
			At:       at,
			Status:   firstNonEmpty(scan.StatusDetail, scan.ScanType),
			Location: scan.ScannedAt,
			Remarks:  scan.Remarks,
		})
		if !at.IsZero() && (tracking.LastEventAt == nil || at.After(*tracking.LastEventAt)) {
			v := at
			tracking.LastEventAt = &v
		}
	}
	if at, okParse := parseTime(status.StatusDateTime); okParse {
		if tracking.LastEventAt == nil || at.After(*tracking.LastEventAt) {
			tracking.LastEventAt = &at
		}
		switch status.ShipmentStatus {
		case shipping.StatusDelivered:
			v := at
			tracking.DeliveredAt = &v
		case shipping.StatusPickedUp:
			v := at
			tracking.PickedUpAt = &v
		}
	}
	return tracking, nil
}

// Cancel withdraws a manifested parcel.
func (p *Provider) Cancel(ctx context.Context, ref shipping.ShipmentRef) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	awb := firstNonEmpty(ref.TrackingNumber, ref.ProviderShipmentID)
	if awb == "" {
		return shipping.Errf(shipping.CodeShipmentNotFound,
			"no delhivery waybill to cancel")
	}
	if err := p.svc.CancelShipment(awb); err != nil {
		return shipping.Wrap(shipping.CodeShipmentCreationFailed, err,
			"delhivery cancellation failed for waybill %s", awb)
	}
	return nil
}

// Label downloads the packing slip as bytes.
func (p *Provider) Label(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Label, error) {
	awb := firstNonEmpty(ref.TrackingNumber, ref.ProviderShipmentID)
	if awb == "" {
		return nil, shipping.Errf(shipping.CodeLabelUnavailable,
			"no delhivery waybill for a label")
	}
	data, contentType, err := p.svc.FetchPackingSlip(ctx, awb)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeLabelUnavailable, err,
			"delhivery packing slip failed for waybill %s", awb)
	}
	return &shipping.Label{
		ContentType: contentType,
		Filename:    fmt.Sprintf("label-%s.pdf", awb),
		Data:        data,
	}, nil
}

// SchedulePickup asks Delhivery to collect from the registered location.
//
// Delhivery schedules per pickup location and package count, not per parcel,
// so the shipment reference is only used to make the request one-per-parcel at
// our end. Idempotency is enforced by ShippingService.
func (p *Provider) SchedulePickup(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Pickup, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	date := p.now().Format("2006-01-02")
	if err := p.svc.RequestPickup(date, "", 1); err != nil {
		return nil, shipping.Wrap(shipping.CodePickupFailed, err,
			"delhivery pickup request failed for %s", date)
	}
	return &shipping.Pickup{ScheduledDate: date}, nil
}

// PickupLocations reports the configured location.
//
// Delhivery exposes no list endpoint on this integration, so the honest answer
// is the location this backend is configured to manifest against, not an
// invented inventory.
func (p *Provider) PickupLocations(context.Context) ([]shipping.PickupLocation, error) {
	name := strings.TrimSpace(p.cfg.PickupLocation)
	if name == "" {
		return nil, shipping.Errf(shipping.CodeShippingUnavailable,
			"no delhivery pickup location is configured")
	}
	return []shipping.PickupLocation{{Name: name}}, nil
}

// ParseWebhook authenticates and normalizes a Delhivery status callback.
//
// Delhivery does not sign its callbacks and this account has no HMAC facility,
// so no cryptographic verification is invented here. What is enforced is a
// shared secret this backend requires on every call, presented as an
// X-Delhivery-Token header or a `token` query parameter (Delhivery lets the
// callback URL be configured freely, which is what makes the query form
// workable).
//
// A missing configured secret is a rejection, not a bypass. The endpoint was
// previously unauthenticated: anyone who knew a waybill could drive an order
// to delivered.
func (p *Provider) ParseWebhook(headers map[string]string, body []byte) (*shipping.WebhookEvent, error) {
	secret := strings.TrimSpace(p.cfg.WebhookSecret)
	if secret == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"DELHIVERY_WEBHOOK_SECRET is not configured; refusing to trust the callback")
	}
	presented := firstNonEmpty(
		headerLookup(headers, "x-delhivery-token"),
		headerLookup(headers, "x-api-key"),
		headerLookup(headers, "token"),
	)
	if presented == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"delhivery webhook is missing its shared secret")
	}
	if !constantTimeEqual(presented, secret) {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"delhivery webhook presented an invalid shared secret")
	}

	if len(body) == 0 {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"delhivery webhook body is empty")
	}
	payload, err := services.ParseWebhook(body)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeInvalidRequest, err,
			"delhivery webhook body is not readable")
	}
	awb := strings.TrimSpace(payload.Waybill)
	if awb == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"delhivery webhook carries no waybill")
	}

	event := &shipping.WebhookEvent{
		Provider:           shipping.ProviderDelhivery,
		ProviderShipmentID: awb,
		TrackingNumber:     awb,
		OrderRef:           strings.TrimSpace(payload.ReferenceNo),
		Status:             services.GetOrderStatusFromWebhook(payload.StatusType),
		StatusDetail:       strings.TrimSpace(payload.Status),
		CourierName:        "Delhivery",
		Location:           strings.TrimSpace(payload.StatusLocation),
		ExpectedDate:       strings.TrimSpace(payload.ExpectedDate),
	}
	if at, valid := parseTime(payload.StatusDateTime); valid {
		event.OccurredAt = at
	} else {
		event.OccurredAt = p.now()
	}
	switch event.Status {
	case shipping.StatusDelivered:
		t := event.OccurredAt
		event.DeliveredAt = &t
	case shipping.StatusPickedUp:
		t := event.OccurredAt
		event.PickedUpAt = &t
	}
	event.EventKey = eventKey(awb, payload.StatusType, event.OccurredAt)
	return event, nil
}

// TrackingURL is the public Delhivery tracking page. Unchanged from the
// original integration so historical orders keep a working link.
func TrackingURL(awb string) string {
	if awb == "" {
		return ""
	}
	return "https://www.delhivery.com/track/package/" + awb
}

// ---------------------------------------------------------------- helpers

var timeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05.000",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func headerLookup(headers map[string]string, name string) string {
	if v, found := headers[name]; found {
		return v
	}
	lower := strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return hmac.Equal(ah[:], bh[:])
}

func eventKey(awb, statusType string, at time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d", shipping.ProviderDelhivery, awb, statusType, at.Unix())
	return fmt.Sprintf("%x", h.Sum(nil))
}

var _ shipping.Provider = (*Provider)(nil)
