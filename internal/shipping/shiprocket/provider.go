package shiprocket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// Config is everything the Shiprocket adapter needs. Credentials arrive from
// process configuration and are never logged, serialized or echoed.
type Config struct {
	Email          string
	Password       string
	BaseURL        string
	PickupLocation string
	ChannelID      string
	WebhookSecret  string
	SellerGSTIN    string
}

// Provider is the Shiprocket implementation of shipping.Provider.
type Provider struct {
	cfg    Config
	tr     *transport
	tokens *tokenManager
	now    func() time.Time
}

// New builds a Shiprocket provider. It performs no network I/O: authentication
// is lazy, so the API still boots with absent or wrong credentials and fails
// only the shipping calls.
func New(cfg Config) *Provider {
	tr := newTransport(cfg.BaseURL)
	return &Provider{
		cfg:    cfg,
		tr:     tr,
		tokens: newTokenManager(cfg.Email, cfg.Password, tr),
		now:    time.Now,
	}
}

func (p *Provider) Name() string { return shipping.ProviderShiprocket }

// Configured reports whether credentials are present.
func (p *Provider) Configured() bool {
	return p.cfg.Email != "" && p.cfg.Password != ""
}

// call performs an authenticated request, refreshing the token exactly once if
// the carrier rejects it.
//
// A 401 means Shiprocket did not process the request, so replaying it after
// re-authentication is safe even for a mutation. The retry is capped at one so
// a persistently rejected credential cannot loop.
func (p *Provider) call(ctx context.Context, req request) (*response, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.bearer = token

	resp, err := p.tr.do(ctx, req)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeShippingUnavailable, err,
			"shiprocket %s %s transport failure", req.method, req.path)
	}
	if resp.status != http.StatusUnauthorized {
		return resp, nil
	}

	p.tokens.Invalidate(token)
	fresh, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	if fresh == token {
		// Nothing changed, so a replay would fail identically.
		return resp, nil
	}
	req.bearer = fresh
	resp, err = p.tr.do(ctx, req)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeShippingUnavailable, err,
			"shiprocket %s %s transport failure after re-authentication", req.method, req.path)
	}
	return resp, nil
}

// fail converts a non-2xx response into a classified shipping error, keeping
// the provider body in operator detail only.
func fail(resp *response, op string, failure shipping.ErrorCode) *shipping.Error {
	code := shipping.ClassifyHTTPStatus(resp.status, failure)
	return shipping.Errf(code, "shiprocket %s: %s", op, safeDetail(resp.status, resp.body)).
		WithStatus(resp.status)
}

func ok(status int) bool { return status >= 200 && status < 300 }

// ---------------------------------------------------------------- rates

// Rates calls GET /courier/serviceability/ and normalizes the courier list.
func (p *Provider) Rates(ctx context.Context, req shipping.RateRequest) ([]shipping.RateOption, error) {
	if len(strings.TrimSpace(req.DeliveryPincode)) != 6 {
		return nil, shipping.Errf(shipping.CodeInvalidPincode,
			"delivery pincode %q is not six digits", req.DeliveryPincode)
	}
	if strings.TrimSpace(req.PickupPincode) == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"pickup pincode is not configured")
	}

	q := url.Values{}
	q.Set("pickup_postcode", strings.TrimSpace(req.PickupPincode))
	q.Set("delivery_postcode", strings.TrimSpace(req.DeliveryPincode))
	q.Set("cod", boolParam(req.COD))
	// Shiprocket expects kilograms here; the subsystem carries grams.
	q.Set("weight", strconv.FormatFloat(req.Package.WeightGrams/1000, 'f', 2, 64))
	if req.Package.LengthCm > 0 {
		q.Set("length", trimFloat(req.Package.LengthCm))
		q.Set("breadth", trimFloat(req.Package.BreadthCm))
		q.Set("height", trimFloat(req.Package.HeightCm))
	}
	if req.DeclaredValue > 0 {
		q.Set("declared_value", trimFloat(req.DeclaredValue))
	}

	resp, err := p.call(ctx, request{
		method:     http.MethodGet,
		path:       "/courier/serviceability/",
		query:      "?" + q.Encode(),
		replayable: true,
	})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		// A 404 here is the carrier saying it does not serve the lane, which
		// is a business answer rather than a missing resource.
		if resp.status == http.StatusNotFound {
			return nil, shipping.Errf(shipping.CodeNotServiceable,
				"shiprocket reports pincode %s unserviceable", req.DeliveryPincode)
		}
		return nil, fail(resp, "serviceability", shipping.CodeRateUnavailable)
	}

	var out serviceabilityResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeRateUnavailable, err,
			"shiprocket serviceability response was not readable")
	}

	recommended := map[int]bool{}
	if id := out.Data.RecommendedCourierID.Int(); id != 0 {
		recommended[id] = true
	}
	if id := out.Data.ShiprocketRecommendedID.Int(); id != 0 {
		recommended[id] = true
	}

	options := make([]shipping.RateOption, 0, len(out.Data.AvailableCourierCompanies))
	for _, c := range out.Data.AvailableCourierCompanies {
		// A blocked courier is offered in the payload but cannot actually
		// carry the parcel.
		if c.Blocked.Bool() {
			continue
		}
		courierID := c.CourierCompanyID.Int()
		if courierID == 0 {
			continue
		}

		charge := chargeFor(c)
		if charge <= 0 {
			// No usable price means no honest option to show.
			continue
		}

		opt := shipping.RateOption{
			ID:                shipping.OptionID(shipping.ProviderShiprocket, strconv.Itoa(courierID)),
			Provider:          shipping.ProviderShiprocket,
			ProviderCourierID: strconv.Itoa(courierID),
			CourierName:       strings.TrimSpace(c.CourierName.String()),
			Charge:            round2(charge),
			CODAvailable:      c.COD.Bool(),
			// Recommendation comes only from the carrier's own nomination,
			// never from position in the array.
			Recommended: recommended[courierID],
		}
		// An absent ETA stays absent. A delivery promise we did not receive
		// is not one we invent.
		if days := leadingInt(c.EstimatedDeliveryDays.String()); days > 0 {
			opt.EstimatedDeliveryDays = days
		}
		if etd := strings.TrimSpace(c.ETD.String()); etd != "" {
			opt.ETD = etd
		}
		opt.Mode = modeLabel(c)
		if opt.CourierName == "" {
			opt.CourierName = "Courier " + strconv.Itoa(courierID)
		}
		options = append(options, opt)
	}

	if len(options) == 0 {
		return nil, shipping.Errf(shipping.CodeRateUnavailable,
			"shiprocket returned no usable courier for pincode %s", req.DeliveryPincode)
	}
	return options, nil
}

// chargeFor picks the payable amount for a courier entry.
//
// `rate` is Shiprocket's all-in figure when present. Falling back to the
// component sum matters because the two are not interchangeable: freight
// alone would under-quote a COD shipment by the COD fee.
func chargeFor(c courierCompany) float64 {
	if r := c.Rate.Float(); r > 0 {
		return r
	}
	return c.FreightCharge.Float() + c.CODCharges.Float() + c.OtherCharges.Float()
}

// modeLabel names the shipping mode in words, or says nothing.
//
// Shiprocket's `mode` is a numeric code, not a label -- returning it verbatim
// put a bare "1" on the courier card at checkout, beside the price, meaning
// nothing to the customer. Only `is_surface` carries a meaning we can state,
// so a numeric mode is discarded rather than displayed.
func modeLabel(c courierCompany) string {
	if m := strings.TrimSpace(c.Mode.String()); m != "" {
		// A code is not a label. Anything purely numeric is dropped.
		if _, err := strconv.Atoi(m); err != nil {
			return m
		}
	}
	if c.IsSurface.Bool() {
		return "Surface"
	}
	return ""
}

// ---------------------------------------------------------------- create

// CreateShipment calls POST /orders/create/adhoc.
func (p *Provider) CreateShipment(ctx context.Context, req shipping.CreateShipmentRequest) (*shipping.CreateShipmentResult, error) {
	pickup := strings.TrimSpace(req.PickupLocation)
	if pickup == "" {
		pickup = strings.TrimSpace(p.cfg.PickupLocation)
	}
	if pickup == "" {
		return nil, shipping.Errf(shipping.CodeShipmentCreationFailed,
			"no shiprocket pickup location is configured")
	}

	// Shiprocket wants the name in halves and validates that both keys are
	// present. A full name of "Shivam Mishra" becomes "Shivam" + "Mishra".
	//
	// A single-word name yields an empty surname. No placeholder is invented:
	// a fabricated surname would be printed on the shipping label and read out
	// to the courier at the door. An empty value is expected to satisfy the
	// `present` rule that rejected the *missing* key, though that is not
	// verifiable without a live create-order call -- see the report.
	billingFirst, billingLast := splitName(req.Billing.Name)

	payload := createOrderRequest{
		OrderID:        req.OrderRef,
		OrderDate:      req.OrderDate.Format("2006-01-02 15:04"),
		PickupLocation: pickup,
		ChannelID:      strings.TrimSpace(p.cfg.ChannelID),
		// Present but empty: the order carries no customer note.
		Comment: "",

		BillingCustomerName: billingFirst,
		BillingLastName:     billingLast,
		BillingAddress:      req.Billing.Line1,
		BillingAddress2:     req.Billing.Line2,
		BillingCity:         req.Billing.City,
		BillingPincode:      req.Billing.Pincode,
		BillingState:        req.Billing.State,
		BillingCountry:      defaultStr(req.Billing.Country, "India"),
		BillingEmail:        req.Billing.Email,
		BillingPhone:        req.Billing.Phone,

		ShippingIsBilling: req.ShippingIsBilling,

		PaymentMethod:   paymentMethod(req.COD),
		ShippingCharges: round2(req.ShippingCharge),
		TotalDiscount:   round2(req.TotalDiscount),
		SubTotal:        round2(req.SubTotal),

		// Shiprocket takes centimetres and kilograms here.
		Length:  req.Package.LengthCm,
		Breadth: req.Package.BreadthCm,
		Height:  req.Package.HeightCm,
		Weight:  req.Package.WeightGrams / 1000,

		CustomerGSTIN: strings.TrimSpace(req.CustomerGSTIN),
		InvoiceNumber: strings.TrimSpace(req.InvoiceNumber),
	}

	// The shipping block is only *filled* when it differs from billing, but the
	// keys are always sent. With shipping_is_billing true Shiprocket reads the
	// billing address, and its own working payload carries these as empty
	// strings rather than dropping them -- which is what the `present` rule
	// requires.
	if !req.ShippingIsBilling {
		shippingFirst, shippingLast := splitName(req.Shipping.Name)
		payload.ShippingCustomerName = shippingFirst
		payload.ShippingLastName = shippingLast
		payload.ShippingAddress = req.Shipping.Line1
		payload.ShippingAddress2 = req.Shipping.Line2
		payload.ShippingCity = req.Shipping.City
		payload.ShippingPincode = req.Shipping.Pincode
		payload.ShippingState = req.Shipping.State
		payload.ShippingCountry = defaultStr(req.Shipping.Country, "India")
		payload.ShippingEmail = req.Shipping.Email
		payload.ShippingPhone = req.Shipping.Phone
	}

	for _, line := range req.Lines {
		item := createOrderItem{
			Name:         line.Name,
			SKU:          line.SKU,
			Units:        line.Units,
			SellingPrice: round2(line.SellingPrice),
			// Present but empty when the order does not carry them. Empty, not
			// zero: an invented HSN can reach customs, and a zero tax asserts
			// something about tax rather than declining to.
			Discount: "",
			Tax:      "",
			HSN:      "",
		}
		if line.Discount > 0 {
			item.Discount = round2(line.Discount)
		}
		if line.Tax > 0 {
			item.Tax = round2(line.Tax)
		}
		if h := strings.TrimSpace(line.HSN); h != "" {
			item.HSN = h
		}
		payload.OrderItems = append(payload.OrderItems, item)
	}
	if len(payload.OrderItems) == 0 {
		return nil, shipping.Errf(shipping.CodeShipmentCreationFailed,
			"order %s has no line items to ship", req.OrderRef)
	}

	resp, err := p.call(ctx, request{
		method: http.MethodPost,
		path:   "/orders/create/adhoc",
		body:   payload,
		// Never replayed on a transport error: a timed-out create may have
		// succeeded, and a duplicate would book a second parcel. Idempotency
		// is enforced by ShippingService before we ever get here.
		replayable: false,
	})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		// A rejected booking is the one case where the outgoing request is
		// worth having in the log: a 422 names a field, and without the
		// payload there is no way to see what we actually sent. Logged only on
		// failure, and only through logOutgoingRequest, which redacts the
		// customer's personal details.
		logOutgoingRequest("create order", payload, resp.status)
		return nil, fail(resp, "create order", shipping.CodeShipmentCreationFailed)
	}

	var out createOrderResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeShipmentCreationFailed, err,
			"shiprocket create-order response was not readable")
	}
	if out.ShipmentID.Int() == 0 && out.OrderID.Int() == 0 {
		return nil, shipping.Errf(shipping.CodeShipmentCreationFailed,
			"shiprocket create-order returned no identifiers: %s", safeDetail(resp.status, resp.body))
	}

	result := &shipping.CreateShipmentResult{
		ProviderOrderID:    out.OrderID.String(),
		ProviderShipmentID: out.ShipmentID.String(),
		TrackingNumber:     strings.TrimSpace(out.AWBCode.String()),
		CourierCompanyID:   out.CourierCompanyID.String(),
		CourierName:        strings.TrimSpace(out.CourierName.String()),
		Status:             mapStatus(out.StatusCode.Int(), out.Status.String()),
	}
	if result.TrackingNumber != "" {
		result.TrackingURL = TrackingURL(result.TrackingNumber)
	}
	return result, nil
}

// ---------------------------------------------------------------- AWB

// AssignAWB calls POST /courier/assign/awb.
//
// Creation does not assign an AWB for a custom order, so this is a required
// second step rather than an optimization.
func (p *Provider) AssignAWB(ctx context.Context, req shipping.AssignAWBRequest) (*shipping.AssignAWBResult, error) {
	shipmentID := strings.TrimSpace(req.ProviderShipmentID)
	if shipmentID == "" {
		return nil, shipping.Errf(shipping.CodeAWBAssignmentFailed,
			"cannot assign an AWB without a shiprocket shipment id")
	}

	resp, err := p.call(ctx, request{
		method: http.MethodPost,
		path:   "/courier/assign/awb",
		body: assignAWBRequest{
			ShipmentID: shipmentID,
			CourierID:  strings.TrimSpace(req.CourierID),
		},
		replayable: false,
	})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		return nil, fail(resp, "assign awb", shipping.CodeAWBAssignmentFailed)
	}

	var out assignAWBResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeAWBAssignmentFailed, err,
			"shiprocket assign-awb response was not readable")
	}

	// response.data is an object on success and sometimes a bare string on
	// failure, so it is decoded only after the object shape is confirmed.
	var data assignAWBData
	if len(out.Response.Data) > 0 && out.Response.Data[0] == '{' {
		if err := json.Unmarshal(out.Response.Data, &data); err != nil {
			return nil, shipping.Wrap(shipping.CodeAWBAssignmentFailed, err,
				"shiprocket assign-awb data was not readable")
		}
	}

	awb := strings.TrimSpace(data.AWBCode.String())
	if awb == "" {
		return nil, shipping.Errf(shipping.CodeAWBAssignmentFailed,
			"shiprocket did not return an AWB for shipment %s: %s",
			shipmentID, safeDetail(resp.status, resp.body))
	}

	result := &shipping.AssignAWBResult{
		TrackingNumber:   awb,
		CourierCompanyID: data.CourierCompanyID.String(),
		CourierName:      strings.TrimSpace(data.CourierName.String()),
		TrackingURL:      TrackingURL(awb),
	}
	if t := data.AssignedDateTime.Ptr(); t != nil {
		result.AssignedAt = *t
	} else {
		result.AssignedAt = p.now()
	}
	return result, nil
}

// ---------------------------------------------------------------- tracking

// Track prefers the shipment id, which is stable across a courier change, and
// falls back to the AWB.
func (p *Provider) Track(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Tracking, error) {
	var path string
	switch {
	case strings.TrimSpace(ref.ProviderShipmentID) != "":
		path = "/courier/track/shipment/" + url.PathEscape(strings.TrimSpace(ref.ProviderShipmentID))
	case strings.TrimSpace(ref.TrackingNumber) != "":
		path = "/courier/track/awb/" + url.PathEscape(strings.TrimSpace(ref.TrackingNumber))
	default:
		return nil, shipping.Errf(shipping.CodeShipmentNotFound,
			"no shiprocket shipment id or AWB to track")
	}

	resp, err := p.call(ctx, request{method: http.MethodGet, path: path, replayable: true})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		return nil, fail(resp, "track", shipping.CodeTrackingUnavailable)
	}

	var out trackingResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeTrackingUnavailable, err,
			"shiprocket tracking response was not readable")
	}
	td := out.TrackingData
	if msg := strings.TrimSpace(td.Error.String()); msg != "" {
		return nil, shipping.Errf(shipping.CodeTrackingUnavailable,
			"shiprocket tracking error: %s", msg)
	}
	if len(td.ShipmentTrack) == 0 && len(td.Activities) == 0 {
		return nil, shipping.Errf(shipping.CodeTrackingUnavailable,
			"shiprocket has no tracking data yet for this shipment")
	}

	tracking := &shipping.Tracking{
		Provider:    shipping.ProviderShiprocket,
		TrackingURL: strings.TrimSpace(td.TrackURL.String()),
	}
	if etd := strings.TrimSpace(td.ETD.String()); etd != "" {
		tracking.ExpectedDelivery = etd
	}

	if len(td.ShipmentTrack) > 0 {
		e := td.ShipmentTrack[0]
		tracking.TrackingNumber = strings.TrimSpace(e.AWBCode.String())
		tracking.CourierName = strings.TrimSpace(e.CourierName.String())
		tracking.StatusDetail = strings.TrimSpace(e.CurrentStatus.String())
		tracking.CurrentLocation = strings.TrimSpace(e.Destination.String())
		tracking.PickedUpAt = e.PickupDate.Ptr()
		tracking.DeliveredAt = e.DeliveredDate.Ptr()
		if tracking.ExpectedDelivery == "" {
			tracking.ExpectedDelivery = strings.TrimSpace(e.EDD.String())
		}
	}
	if tracking.TrackingNumber == "" {
		tracking.TrackingNumber = strings.TrimSpace(ref.TrackingNumber)
	}
	if tracking.TrackingURL == "" && tracking.TrackingNumber != "" {
		tracking.TrackingURL = TrackingURL(tracking.TrackingNumber)
	}

	tracking.Status = mapStatus(td.ShipmentStatus.Int(), tracking.StatusDetail)

	for _, a := range td.Activities {
		at, _ := parseFlexTime(a.Date.String())
		scan := shipping.TrackingScan{
			At:       at,
			Status:   firstNonEmpty(a.Activity.String(), a.SRStatusLabel.String(), a.Status.String()),
			Location: strings.TrimSpace(a.Location.String()),
		}
		tracking.Scans = append(tracking.Scans, scan)
		if !at.IsZero() && (tracking.LastEventAt == nil || at.After(*tracking.LastEventAt)) {
			v := at
			tracking.LastEventAt = &v
		}
	}
	return tracking, nil
}

// ---------------------------------------------------------------- cancel

// Cancel calls POST /orders/cancel, which answers 204 with no body on success.
func (p *Provider) Cancel(ctx context.Context, ref shipping.ShipmentRef) error {
	id, err := strconv.ParseInt(strings.TrimSpace(ref.ProviderOrderID), 10, 64)
	if err != nil || id == 0 {
		return shipping.Errf(shipping.CodeShipmentNotFound,
			"cannot cancel without a numeric shiprocket order id")
	}

	resp, err := p.call(ctx, request{
		method:     http.MethodPost,
		path:       "/orders/cancel",
		body:       cancelRequest{IDs: []int64{id}},
		replayable: false,
	})
	if err != nil {
		return err
	}
	// 204 No Content is the documented success. Treating a missing body as a
	// failure would report every successful cancellation as an error.
	if resp.status == http.StatusNoContent || ok(resp.status) {
		return nil
	}
	// An order Shiprocket has already cancelled is the state we wanted.
	if resp.status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(string(resp.body)), "already") {
		return nil
	}
	return fail(resp, "cancel", shipping.CodeShipmentCreationFailed)
}

// ---------------------------------------------------------------- label

// Label generates and then downloads the label document.
//
// The carrier's label_url is never returned to a caller: the bytes are fetched
// here so the API can stream them from an authenticated MAK endpoint.
func (p *Provider) Label(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Label, error) {
	shipmentID := strings.TrimSpace(ref.ProviderShipmentID)
	if shipmentID == "" {
		return nil, shipping.Errf(shipping.CodeLabelUnavailable,
			"cannot generate a label without a shiprocket shipment id")
	}

	resp, err := p.call(ctx, request{
		method:  http.MethodPost,
		path:    "/courier/generate/label",
		body:    labelRequest{ShipmentID: []string{shipmentID}},
		timeout: labelTimeout,
		// Generating a label twice is harmless; Shiprocket returns the same
		// document.
		replayable: true,
	})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		return nil, fail(resp, "generate label", shipping.CodeLabelUnavailable)
	}

	var out labelResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeLabelUnavailable, err,
			"shiprocket label response was not readable")
	}
	labelURL := strings.TrimSpace(out.LabelURL.String())
	if !out.LabelCreated.Bool() || labelURL == "" {
		return nil, shipping.Errf(shipping.CodeLabelUnavailable,
			"shiprocket did not create a label for shipment %s: %s",
			shipmentID, strings.TrimSpace(out.Response.String()))
	}

	data, contentType, err := p.tr.getAbsolute(ctx, labelURL, maxLabelBytes)
	if err != nil {
		return nil, shipping.Wrap(shipping.CodeLabelUnavailable, err,
			"could not download the shiprocket label document")
	}
	if contentType == "" {
		contentType = "application/pdf"
	}
	return &shipping.Label{
		ContentType: contentType,
		Filename:    fmt.Sprintf("label-%s.pdf", shipmentID),
		Data:        data,
	}, nil
}

// ---------------------------------------------------------------- pickup

// SchedulePickup calls POST /courier/generate/pickup.
func (p *Provider) SchedulePickup(ctx context.Context, ref shipping.ShipmentRef) (*shipping.Pickup, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(ref.ProviderShipmentID), 10, 64)
	if err != nil || id == 0 {
		return nil, shipping.Errf(shipping.CodePickupFailed,
			"cannot schedule a pickup without a numeric shiprocket shipment id")
	}

	resp, err := p.call(ctx, request{
		method:     http.MethodPost,
		path:       "/courier/generate/pickup",
		body:       pickupRequest{ShipmentID: []int64{id}},
		replayable: false,
	})
	if err != nil {
		return nil, err
	}

	var out pickupResponse
	// The duplicate-pickup case arrives as a 4xx carrying an explanatory
	// message, so the body is decoded before the status is judged.
	_ = decode(resp.body, &out)
	message := strings.ToLower(strings.Join([]string{
		out.Message.String(),
		out.Response.Others.String(),
		out.Response.Data.String(),
	}, " "))

	// Shiprocket reports an existing pickup as an error. For us that is the
	// desired end state, so it is success -- this is what keeps an admin
	// double-click from being a failure.
	alreadyScheduled := strings.Contains(message, "already") ||
		strings.Contains(message, "pickup has already been scheduled") ||
		strings.Contains(message, "in pickup queue")

	if !ok(resp.status) && !alreadyScheduled {
		return nil, fail(resp, "generate pickup", shipping.CodePickupFailed)
	}

	pickup := &shipping.Pickup{
		Token:            strings.TrimSpace(out.Response.PickupTokenNumber.String()),
		ScheduledDate:    strings.TrimSpace(out.Response.PickupScheduledDate.String()),
		ManifestURL:      strings.TrimSpace(out.ManifestURL.String()),
		AlreadyScheduled: alreadyScheduled,
	}
	if t, valid := parseFlexTime(pickup.ScheduledDate); valid {
		pickup.ScheduledAt = &t
	}
	return pickup, nil
}

// ---------------------------------------------------------------- pickup locations

// PickupLocations calls GET /settings/company/pickup.
//
// Used to validate that a configured pickup location actually exists at the
// carrier. It never creates one: a pickup address is a real-world fact that an
// operator must confirm, not something an application should invent at boot.
func (p *Provider) PickupLocations(ctx context.Context) ([]shipping.PickupLocation, error) {
	resp, err := p.call(ctx, request{
		method:     http.MethodGet,
		path:       "/settings/company/pickup",
		replayable: true,
	})
	if err != nil {
		return nil, err
	}
	if !ok(resp.status) {
		return nil, fail(resp, "pickup locations", shipping.CodeShippingUnavailable)
	}

	var out pickupLocationsResponse
	if err := decode(resp.body, &out); err != nil {
		return nil, shipping.Wrap(shipping.CodeShippingUnavailable, err,
			"shiprocket pickup-location response was not readable")
	}

	locations := make([]shipping.PickupLocation, 0, len(out.Data.ShippingAddress))
	for _, a := range out.Data.ShippingAddress {
		name := strings.TrimSpace(a.PickupLocation.String())
		if name == "" {
			continue
		}
		locations = append(locations, shipping.PickupLocation{
			Name:    name,
			Address: strings.TrimSpace(strings.TrimSpace(a.Address.String()) + " " + strings.TrimSpace(a.Address2.String())),
			City:    strings.TrimSpace(a.City.String()),
			State:   strings.TrimSpace(a.State.String()),
			Pincode: strings.TrimSpace(a.PinCode.String()),
			Phone:   strings.TrimSpace(a.Phone.String()),
		})
	}
	return locations, nil
}

// ---------------------------------------------------------------- webhook

// ParseWebhook authenticates and normalizes a Shiprocket callback.
//
// Authentication happens first and unconditionally. The shared secret is
// compared in constant time, and a request without a configured secret is
// rejected rather than trusted -- an unauthenticated shipping callback can
// drive an order to delivered.
func (p *Provider) ParseWebhook(headers map[string]string, body []byte) (*shipping.WebhookEvent, error) {
	secret := strings.TrimSpace(p.cfg.WebhookSecret)
	if secret == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"SHIPROCKET_WEBHOOK_SECRET is not configured; refusing to trust the callback")
	}
	// Shiprocket sends the token as x-api-key.
	presented := strings.TrimSpace(headerLookup(headers, "x-api-key"))
	if presented == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"shiprocket webhook is missing the x-api-key header")
	}
	// Hashing first makes the comparison length-independent as well as
	// constant time.
	if !constantTimeEqual(presented, secret) {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"shiprocket webhook presented an invalid x-api-key")
	}

	if len(body) == 0 {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"shiprocket webhook body is empty")
	}
	var payload webhookPayload
	if err := decode(body, &payload); err != nil {
		return nil, shipping.Wrap(shipping.CodeInvalidRequest, err,
			"shiprocket webhook body is not readable")
	}

	awb := strings.TrimSpace(payload.AWB.String())
	// sr_order_id is unambiguous when present; order_id carries the same value
	// in the documented payload shape, where sr_order_id is absent entirely.
	srOrderID := firstNonEmpty(payload.SROrderID.String(), payload.OrderID.String())
	shipmentID := payload.ShipmentID.String()
	if awb == "" && srOrderID == "" && shipmentID == "" {
		return nil, shipping.Errf(shipping.CodeInvalidRequest,
			"shiprocket webhook carries no shipment identifier")
	}

	statusID := payload.ShipmentStatusID.Int()
	if statusID == 0 {
		statusID = payload.CurrentStatusID.Int()
	}
	label := firstNonEmpty(payload.ShipmentStatus.String(), payload.CurrentStatus.String())

	event := &shipping.WebhookEvent{
		Provider:           shipping.ProviderShiprocket,
		ProviderOrderID:    srOrderID,
		ProviderShipmentID: shipmentID,
		TrackingNumber:     awb,
		// channel_order_id is the merchant reference we sent. It is a lookup
		// hint only; the shipment is resolved by provider identifiers.
		OrderRef:       strings.TrimSpace(payload.ChannelOrderID.String()),
		Status:         mapStatus(statusID, label),
		StatusDetail:   strings.TrimSpace(label),
		CourierName:    strings.TrimSpace(payload.CourierName.String()),
		ExpectedDate:   strings.TrimSpace(payload.ETD.String()),
		IsReturn:       payload.IsReturn.Bool(),
		PickupSchedule: strings.TrimSpace(payload.PickupScheduled.String()),
	}

	if t, valid := parseFlexTime(payload.CurrentTimestamp.String()); valid {
		event.OccurredAt = t
	} else {
		event.OccurredAt = p.now()
	}

	for _, s := range payload.Scans {
		at, _ := parseFlexTime(s.Date.String())
		event.Scans = append(event.Scans, shipping.TrackingScan{
			At:       at,
			Status:   firstNonEmpty(s.Activity.String(), s.Status.String()),
			Location: strings.TrimSpace(s.Location.String()),
		})
		if !at.IsZero() {
			event.Location = strings.TrimSpace(s.Location.String())
		}
	}

	switch event.Status {
	case shipping.StatusDelivered:
		t := event.OccurredAt
		event.DeliveredAt = &t
	case shipping.StatusPickedUp:
		t := event.OccurredAt
		event.PickedUpAt = &t
	}

	event.EventKey = eventKey(shipping.ProviderShiprocket, awb, shipmentID, statusID, event.OccurredAt)
	return event, nil
}

// eventKey fingerprints an event so a redelivery is recognizable.
func eventKey(provider, awb, shipmentID string, statusID int, at time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d", provider, awb, shipmentID, statusID, at.Unix())
	return fmt.Sprintf("%x", h.Sum(nil))
}

// constantTimeEqual compares two secrets without leaking length or content
// through timing.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return hmac.Equal(ah[:], bh[:])
}

// headerLookup finds a header case-insensitively.
func headerLookup(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
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

// TrackingURL is the customer-facing Shiprocket tracking page for an AWB.
func TrackingURL(awb string) string {
	if awb == "" {
		return ""
	}
	return "https://shiprocket.co/tracking/" + url.PathEscape(awb)
}

// ---------------------------------------------------------------- helpers

func boolParam(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func paymentMethod(cod bool) string {
	if cod {
		return "COD"
	}
	return "Prepaid"
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// leadingInt reads the first integer in a string, so "3", "3-4 days" and
// "3 to 5" all yield 3 and an empty or non-numeric value yields 0.
func leadingInt(s string) int {
	s = strings.TrimSpace(s)
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			n, _ := strconv.Atoi(s[start:i])
			return n
		}
	}
	if start >= 0 {
		n, _ := strconv.Atoi(s[start:])
		return n
	}
	return 0
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func round2(v float64) float64 {
	return float64(int64(v*100+copysign(0.5, v))) / 100
}

func copysign(v, sign float64) float64 {
	if sign < 0 {
		return -v
	}
	return v
}

// Compile-time proof the adapter satisfies the neutral contract.
var _ shipping.Provider = (*Provider)(nil)
