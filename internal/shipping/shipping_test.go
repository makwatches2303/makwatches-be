package shipping

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------ quotes

func testQuoter() *Quoter { return NewQuoter("a-server-only-secret") }

func sampleOption() RateOption {
	return RateOption{
		ID:                    "shiprocket:51",
		Provider:              ProviderShiprocket,
		ProviderCourierID:     "51",
		CourierName:           "Delhivery Surface",
		Charge:                54,
		EstimatedDeliveryDays: 4,
		ETD:                   "Jul 01, 2024",
		CODAvailable:          true,
	}
}

// sampleBinding is a quote bound to one customer, one cart and one address --
// the shape a checkout quote actually has.
func sampleBinding() QuoteBinding {
	return QuoteBinding{
		UserID:      "507f1f77bcf86cd799439011",
		CartHash:    CartFingerprint([]CartLine{{ProductID: "p1", Quantity: 1}}),
		Pincode:     "400001",
		WeightGrams: 500,
		COD:         false,
	}
}

func TestQuoteRoundTrips(t *testing.T) {
	q := testQuoter()
	now := time.Now()

	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := q.Verify(token, sampleBinding(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Charge != 54 {
		t.Errorf("charge = %v, want 54", verified.Charge)
	}
	if verified.CourierID != "51" {
		t.Errorf("courierID = %q", verified.CourierID)
	}
	if verified.Provider != ProviderShiprocket {
		t.Errorf("provider = %q", verified.Provider)
	}
}

func TestQuoteRejectsTamperedCharge(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}

	// A client that edits the payload half must not be able to re-sign it.
	body, sig, _ := strings.Cut(token, ".")
	forged := body[:len(body)-4] + "AAAA" + "." + sig
	if _, err := q.Verify(forged, sampleBinding(), now); err == nil {
		t.Fatal("a tampered quote payload must not verify")
	}
}

func TestQuoteRejectsForeignSignature(t *testing.T) {
	signed, err := testQuoter().Sign(sampleOption(), sampleBinding(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other := NewQuoter("a-different-secret")
	if _, err := other.Verify(signed, sampleBinding(), time.Now()); err == nil {
		t.Fatal("a quote signed with another key must not verify")
	}
}

func TestQuoteRejectsMalformedToken(t *testing.T) {
	q := testQuoter()
	for _, token := range []string{"", ".", "nodot", "a.", ".b", "!!!.???"} {
		if _, err := q.Verify(token, sampleBinding(), time.Now()); err == nil {
			t.Errorf("token %q must not verify", token)
		}
	}
}

func TestQuoteExpires(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Verify(token, sampleBinding(), now.Add(QuoteTTL+time.Minute)); err == nil {
		t.Fatal("an expired quote must not verify")
	}
}

// TestQuoteIsBoundToItsDestination is the important one: without this binding a
// customer could quote a cheap metro rate and then ship to a remote pincode at
// that price.
func TestQuoteIsBoundToItsDestination(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := sampleBinding()
	elsewhere.Pincode = "797001"
	if _, err := q.Verify(token, elsewhere, now); err == nil {
		t.Fatal("a quote must not be usable for a different destination")
	}
}

func TestQuoteIsBoundToWeightAndPaymentMode(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}
	heavier := sampleBinding()
	heavier.WeightGrams = 5000
	if _, err := q.Verify(token, heavier, now); err == nil {
		t.Fatal("a quote must not be usable for a heavier parcel")
	}
	asCOD := sampleBinding()
	asCOD.COD = true
	if _, err := q.Verify(token, asCOD, now); err == nil {
		t.Fatal("a prepaid quote must not be usable for a COD order")
	}
}

func TestQuoteTokenDoesNotExposeCharge(t *testing.T) {
	token, err := testQuoter().Sign(sampleOption(), sampleBinding(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// It is opaque, not secret -- but it must at least not be a plaintext
	// number a casual edit could change.
	if strings.Contains(token, "54") && strings.Contains(token, "courier") {
		t.Fatal("quote token looks like plaintext")
	}
	if !strings.Contains(token, ".") {
		t.Fatal("quote token is missing its signature segment")
	}
}

func TestOptionIDRoundTrip(t *testing.T) {
	id := OptionID(ProviderShiprocket, "51")
	if id != "shiprocket:51" {
		t.Fatalf("id = %q", id)
	}
	if got := ParseCourierID(id); got != "51" {
		t.Fatalf("courier = %q", got)
	}
	if got := OptionID(ProviderDelhivery, ""); got != ProviderDelhivery {
		t.Fatalf("id with no courier = %q", got)
	}
}

// ------------------------------------------------------------------ status flow

func TestCanAdvanceMovesForward(t *testing.T) {
	forward := [][2]string{
		{StatusPending, StatusManifested},
		{StatusManifested, StatusPendingPickup},
		{StatusPendingPickup, StatusPickedUp},
		{StatusPickedUp, StatusInTransit},
		{StatusInTransit, StatusOutForDelivery},
		{StatusOutForDelivery, StatusDelivered},
	}
	for _, pair := range forward {
		if !CanAdvance(pair[0], pair[1]) {
			t.Errorf("CanAdvance(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
}

// TestCanAdvanceRefusesToRewind covers the replayed-webhook case: a carrier
// redelivering an old "in transit" event must not un-deliver an order.
func TestCanAdvanceRefusesToRewind(t *testing.T) {
	backward := [][2]string{
		{StatusDelivered, StatusInTransit},
		{StatusDelivered, StatusOutForDelivery},
		{StatusDelivered, StatusPickedUp},
		{StatusInTransit, StatusManifested},
		{StatusOutForDelivery, StatusPickedUp},
		{StatusPickedUp, StatusPending},
	}
	for _, pair := range backward {
		if CanAdvance(pair[0], pair[1]) {
			t.Errorf("CanAdvance(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestCanAdvanceAllowsExplicitExceptions(t *testing.T) {
	// An RTO genuinely follows a delivery or a failed attempt.
	if !CanAdvance(StatusDelivered, StatusReturned) {
		t.Error("delivered -> returned must be allowed (RTO)")
	}
	if !CanAdvance(StatusUndelivered, StatusOutForDelivery) {
		t.Error("undelivered -> out_for_delivery must be allowed (reattempt)")
	}
	if !CanAdvance(StatusUndelivered, StatusInTransit) {
		t.Error("undelivered -> in_transit must be allowed")
	}
}

func TestCanAdvanceAllowsRepeatOfSameStatus(t *testing.T) {
	// A repeated event should still be able to refresh location/timestamps.
	if !CanAdvance(StatusInTransit, StatusInTransit) {
		t.Error("an identical status must be allowed through")
	}
	if !CanAdvance("", StatusManifested) {
		t.Error("an empty starting status must allow anything")
	}
}

func TestOrderStatusForIsFulfillmentOnly(t *testing.T) {
	cases := map[string]string{
		StatusDelivered:      "delivered",
		StatusPickedUp:       "shipped",
		StatusInTransit:      "shipped",
		StatusOutForDelivery: "out_for_delivery",
		StatusReturned:       "returned",
		StatusCancelled:      "cancelled",
		StatusManifested:     "processing",
		StatusPendingPickup:  "processing",
		StatusFailed:         "",
		StatusUndelivered:    "",
	}
	for shipmentStatus, want := range cases {
		if got := orderStatusFor(shipmentStatus); got != want {
			t.Errorf("orderStatusFor(%q) = %q, want %q", shipmentStatus, got, want)
		}
	}
}

// ------------------------------------------------------------------ errors

func TestErrorSeparatesPublicAndOperatorHalves(t *testing.T) {
	err := Errf(CodeShipmentCreationFailed,
		"provider said: billing_pincode is required for +919999999999")

	code, message := err.Public()
	if code != CodeShipmentCreationFailed {
		t.Errorf("code = %s", code)
	}
	// The customer-facing half must not carry the provider's words, which can
	// echo an address or a phone number.
	if strings.Contains(message, "919999999999") || strings.Contains(message, "billing_pincode") {
		t.Fatalf("public message leaked operator detail: %q", message)
	}
	if !strings.Contains(err.Detail, "billing_pincode") {
		t.Fatalf("operator detail lost the reason: %q", err.Detail)
	}
}

func TestErrorHTTPStatusMapping(t *testing.T) {
	cases := map[ErrorCode]int{
		CodeInvalidPincode:         http.StatusBadRequest,
		CodeInvalidRequest:         http.StatusBadRequest,
		CodeNotServiceable:         http.StatusNotFound,
		CodeRateUnavailable:        http.StatusNotFound,
		CodeShipmentNotFound:       http.StatusNotFound,
		CodeAlreadyExists:          http.StatusConflict,
		CodeUnsupported:            http.StatusNotImplemented,
		CodeRateLimited:            http.StatusTooManyRequests,
		CodeProviderAuthFailed:     http.StatusBadGateway,
		CodeShippingUnavailable:    http.StatusBadGateway,
		CodeShipmentCreationFailed: http.StatusBadGateway,
	}
	for code, want := range cases {
		if got := Errf(code, "x").HTTPStatus(); got != want {
			t.Errorf("%s -> %d, want %d", code, got, want)
		}
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorCode
	}{
		{http.StatusUnauthorized, CodeProviderAuthFailed},
		{http.StatusForbidden, CodeProviderAuthFailed},
		{http.StatusNotFound, CodeShipmentNotFound},
		{http.StatusTooManyRequests, CodeRateLimited},
		{http.StatusUnprocessableEntity, CodeShipmentCreationFailed},
		{http.StatusBadRequest, CodeShipmentCreationFailed},
		{http.StatusInternalServerError, CodeShippingUnavailable},
		{http.StatusBadGateway, CodeShippingUnavailable},
		{http.StatusServiceUnavailable, CodeShippingUnavailable},
		{http.StatusGatewayTimeout, CodeShippingUnavailable},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPStatus(tc.status, CodeShipmentCreationFailed); got != tc.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}

func TestCodeOfDefaultsToUnavailable(t *testing.T) {
	if got := CodeOf(errors.New("some non-shipping failure")); got != CodeShippingUnavailable {
		t.Fatalf("CodeOf = %s, want %s", got, CodeShippingUnavailable)
	}
	if got := CodeOf(Errf(CodeLabelUnavailable, "x")); got != CodeLabelUnavailable {
		t.Fatalf("CodeOf = %s", got)
	}
}

func TestWrapPreservesCause(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := Wrap(CodeShippingUnavailable, cause, "carrier unreachable")
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is must find the wrapped cause")
	}
	var se *Error
	if !errors.As(err, &se) {
		t.Fatal("errors.As must find the shipping error")
	}
}

func TestErrUnsupportedIsRecognizable(t *testing.T) {
	// The service relies on this to skip a step a carrier does not have,
	// rather than treating it as a failure.
	if CodeOf(ErrUnsupported) != CodeUnsupported {
		t.Fatal("ErrUnsupported must classify as OPERATION_UNSUPPORTED")
	}
}

func TestEveryCodeHasACustomerMessage(t *testing.T) {
	all := []ErrorCode{
		CodeShippingUnavailable, CodeInvalidPincode, CodeNotServiceable,
		CodeRateUnavailable, CodeProviderAuthFailed, CodeShipmentCreationFailed,
		CodeAWBAssignmentFailed, CodeTrackingUnavailable, CodeLabelUnavailable,
		CodePickupFailed, CodeRateLimited, CodeInvalidRequest,
		CodeShipmentNotFound, CodeUnsupported, CodeAlreadyExists,
	}
	for _, code := range all {
		msg, ok := customerMessage[code]
		if !ok || strings.TrimSpace(msg) == "" {
			t.Errorf("%s has no customer-safe message", code)
		}
	}
}

// ------------------------------------------------------------------ quote binding

// TestQuoteIsBoundToTheCustomer: one customer's quote must be useless to
// another, or a cheap quote could be shared and replayed.
func TestQuoteIsBoundToTheCustomer(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}

	otherCustomer := sampleBinding()
	otherCustomer.UserID = "507f1f77bcf86cd799439099"
	if _, err := q.Verify(token, otherCustomer, now); err == nil {
		t.Fatal("another customer must not be able to use this quote")
	}
}

// TestQuoteIsBoundToTheCart: quoting one watch and shipping ten must fail.
func TestQuoteIsBoundToTheCart(t *testing.T) {
	q := testQuoter()
	now := time.Now()
	token, err := q.Sign(sampleOption(), sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}

	bigger := sampleBinding()
	bigger.CartHash = CartFingerprint([]CartLine{
		{ProductID: "p1", Quantity: 1},
		{ProductID: "p2", Quantity: 9},
	})
	if _, err := q.Verify(token, bigger, now); err == nil {
		t.Fatal("a quote must not survive the bag changing")
	}
}

// TestAnonymousQuoteCannotPriceAnOrder: the public serviceability route issues
// unbound quotes for display. They must not satisfy a checkout, which always
// presents a customer and a cart.
func TestAnonymousQuoteCannotPriceAnOrder(t *testing.T) {
	q := testQuoter()
	now := time.Now()

	public, err := q.Sign(sampleOption(), QuoteBinding{
		Pincode: "400001", WeightGrams: 500,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Verify(public, sampleBinding(), now); err == nil {
		t.Fatal("a public display quote must not be usable to price an order")
	}
}

// TestCartFingerprintIsOrderIndependent: the same cart read back in a
// different order must produce the same fingerprint, or quotes would be
// rejected at random depending on how Mongo returned the rows.
func TestCartFingerprintIsOrderIndependent(t *testing.T) {
	a := CartFingerprint([]CartLine{
		{ProductID: "p1", Quantity: 2},
		{ProductID: "p2", Quantity: 1},
	})
	b := CartFingerprint([]CartLine{
		{ProductID: "p2", Quantity: 1},
		{ProductID: "p1", Quantity: 2},
	})
	if a != b {
		t.Fatalf("fingerprint depends on row order: %q vs %q", a, b)
	}
}

func TestCartFingerprintChangesWithComposition(t *testing.T) {
	base := CartFingerprint([]CartLine{{ProductID: "p1", Quantity: 1}})

	cases := map[string][]CartLine{
		"quantity changed": {{ProductID: "p1", Quantity: 2}},
		"item added":       {{ProductID: "p1", Quantity: 1}, {ProductID: "p2", Quantity: 1}},
		"item swapped":     {{ProductID: "p9", Quantity: 1}},
		"emptied":          {},
	}
	for name, lines := range cases {
		if CartFingerprint(lines) == base {
			t.Errorf("%s: fingerprint did not change", name)
		}
	}
}

func TestCartFingerprintIgnoresZeroQuantityLines(t *testing.T) {
	// A zero-quantity row is not in the parcel, so it must not change the
	// fingerprint and invalidate an otherwise-valid quote.
	withGhost := CartFingerprint([]CartLine{
		{ProductID: "p1", Quantity: 1},
		{ProductID: "p2", Quantity: 0},
	})
	if withGhost != CartFingerprint([]CartLine{{ProductID: "p1", Quantity: 1}}) {
		t.Fatal("a zero-quantity line changed the fingerprint")
	}
}

// TestCourierAndChargeComeFromTheQuoteNotTheClient records the central
// property: whatever a client claims, these values are read out of the signed
// token.
func TestCourierAndChargeComeFromTheQuoteNotTheClient(t *testing.T) {
	q := testQuoter()
	now := time.Now()

	option := sampleOption()
	option.Charge = 129.15
	option.ProviderCourierID = "51"
	token, err := q.Sign(option, sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}

	verified, err := q.Verify(token, sampleBinding(), now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Charge != 129.15 {
		t.Errorf("charge = %v, want the signed 129.15", verified.Charge)
	}
	if verified.CourierID != "51" {
		t.Errorf("courierID = %q, want the signed 51", verified.CourierID)
	}
	if verified.Provider != ProviderShiprocket {
		t.Errorf("provider = %q, want the signed provider", verified.Provider)
	}
}
