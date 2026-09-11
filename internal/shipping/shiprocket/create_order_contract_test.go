package shiprocket

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// These tests pin the /orders/create/adhoc request against Shiprocket's
// confirmed-working contract.
//
// The production failure they guard against:
//
//	422 {"message":"Oops! Invalid Data.",
//	     "errors":{"billing_last_name":["validation.present"]}}
//
// Laravel's `present` rule requires the key to exist in the payload and says
// nothing about its value. Go's omitempty deleted the key, so an order for a
// customer named "Shivam Mishra" was rejected outright.

// contractKeys is every top-level field in Shiprocket's documented working
// payload. Each must be present in ours, whether or not we have a value.
var contractKeys = []string{
	"order_id", "order_date", "pickup_location", "comment",
	"billing_customer_name", "billing_last_name", "billing_address",
	"billing_address_2", "billing_city", "billing_pincode", "billing_state",
	"billing_country", "billing_email", "billing_phone",
	"shipping_is_billing", "shipping_customer_name", "shipping_last_name",
	"shipping_address", "shipping_address_2", "shipping_city",
	"shipping_pincode", "shipping_country", "shipping_state",
	"shipping_email", "shipping_phone",
	"order_items", "payment_method", "shipping_charges", "giftwrap_charges",
	"transaction_charges", "total_discount", "sub_total",
	"length", "breadth", "height", "weight",
}

var contractItemKeys = []string{"name", "sku", "units", "selling_price", "discount", "tax", "hsn"}

// makOrder is the real MAK order shape, with the customer name under test.
func makOrder(customerName string) shipping.CreateShipmentRequest {
	addr := shipping.ShipmentAddress{
		Name:    customerName,
		Line1:   "Matva Street, Near Balaji Complex",
		City:    "Jetpur",
		State:   "Gujarat",
		Pincode: "360370",
		Country: "India",
		Phone:   "9974959693",
		Email:   "buyer@example.test",
	}
	return shipping.CreateShipmentRequest{
		OrderRef:          "MAK-20260911-001",
		OrderDate:         time.Date(2026, 9, 11, 11, 11, 0, 0, time.UTC),
		PickupLocation:    "work",
		Billing:           addr,
		Shipping:          addr,
		ShippingIsBilling: true,
		Lines: []shipping.ShipmentLine{
			{Name: "NoiseFit Halo 3 AMOLED Smartwatch", SKU: "6aa2d570b3f1a1920a1f3182", Units: 1, SellingPrice: 4999},
		},
		SubTotal:       4999,
		ShippingCharge: 265.62,
		Package:        shipping.PackageSpec{WeightGrams: 500, LengthCm: 15, BreadthCm: 10, HeightCm: 8, Defaults: true},
	}
}

// sendAndCapture books a shipment against the mock and returns the decoded
// outgoing payload. Only the request body is captured -- never headers -- so
// no Authorization value can reach a test log.
func sendAndCapture(t *testing.T, req shipping.CreateShipmentRequest) map[string]any {
	t.Helper()
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 200, fixture(t, "create_order_success.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})

	if _, err := p.CreateShipment(context.Background(), req); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(m.lastBody(http.MethodPost, "/orders/create/adhoc")), &payload); err != nil {
		t.Fatalf("outgoing payload was not JSON: %v", err)
	}
	return payload
}

// ---------------------------------------------------------------- the regression

// TestShivamMishraIsSplitIntoFirstAndLast is the exact customer case that
// failed in production.
func TestShivamMishraIsSplitIntoFirstAndLast(t *testing.T) {
	payload := sendAndCapture(t, makOrder("Shivam Mishra"))

	if got := payload["billing_customer_name"]; got != "Shivam" {
		t.Errorf("billing_customer_name = %v, want \"Shivam\"", got)
	}
	if got := payload["billing_last_name"]; got != "Mishra" {
		t.Errorf("billing_last_name = %v, want \"Mishra\"", got)
	}
}

// TestCreateOrderSendsEveryContractKey is the general guard: the 422 was
// caused by a *missing key*, so completeness is the property to hold, not just
// this one field.
func TestCreateOrderSendsEveryContractKey(t *testing.T) {
	payload := sendAndCapture(t, makOrder("Shivam Mishra"))

	var missing []string
	for _, k := range contractKeys {
		if _, ok := payload[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("keys absent from the request (Shiprocket validates these as `present`): %v", missing)
	}

	items, ok := payload["order_items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("order_items missing or empty: %v", payload["order_items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("order item is not an object: %v", items[0])
	}
	for _, k := range contractItemKeys {
		if _, present := item[k]; !present {
			t.Errorf("order item key %q is absent", k)
		}
	}
}

// TestShippingIsBillingIsPreservedWithEmptyShippingBlock: the flag stays true
// and the shipping keys are present-but-empty, exactly as in Shiprocket's own
// working payload.
func TestShippingIsBillingIsPreservedWithEmptyShippingBlock(t *testing.T) {
	payload := sendAndCapture(t, makOrder("Shivam Mishra"))

	if payload["shipping_is_billing"] != true {
		t.Errorf("shipping_is_billing = %v, want true", payload["shipping_is_billing"])
	}
	for _, k := range []string{
		"shipping_customer_name", "shipping_last_name", "shipping_address",
		"shipping_address_2", "shipping_city", "shipping_pincode",
		"shipping_state", "shipping_country", "shipping_email", "shipping_phone",
	} {
		v, present := payload[k]
		if !present {
			t.Errorf("%s is absent; it must be present even when billing is reused", k)
			continue
		}
		if v != "" {
			t.Errorf("%s = %v, want empty when shipping_is_billing is true", k, v)
		}
	}
}

// TestSeparateShippingAddressIsAlsoSplit covers the other branch.
func TestSeparateShippingAddressIsAlsoSplit(t *testing.T) {
	req := makOrder("Shivam Mishra")
	req.ShippingIsBilling = false
	req.Shipping = shipping.ShipmentAddress{
		Name: "Anita Kumari Sharma", Line1: "12 Test Road", City: "Mumbai",
		State: "Maharashtra", Pincode: "400001", Country: "India", Phone: "9999999999",
	}
	payload := sendAndCapture(t, req)

	if payload["shipping_is_billing"] != false {
		t.Errorf("shipping_is_billing = %v, want false", payload["shipping_is_billing"])
	}
	if got := payload["shipping_customer_name"]; got != "Anita Kumari" {
		t.Errorf("shipping_customer_name = %v, want \"Anita Kumari\"", got)
	}
	if got := payload["shipping_last_name"]; got != "Sharma" {
		t.Errorf("shipping_last_name = %v, want \"Sharma\"", got)
	}
	if got := payload["shipping_city"]; got != "Mumbai" {
		t.Errorf("shipping_city = %v", got)
	}
}

// ---------------------------------------------------------------- name normalization

func TestSplitName(t *testing.T) {
	cases := []struct {
		in, first, last string
		why             string
	}{
		{"Shivam Mishra", "Shivam", "Mishra", "the production case"},
		{"Shivam Kumar Mishra", "Shivam Kumar", "Mishra", "middle names stay with the given name, nothing discarded"},
		{"  Shivam   Mishra  ", "Shivam", "Mishra", "leading, trailing and repeated whitespace collapse"},
		{"\tShivam\tMishra\n", "Shivam", "Mishra", "tabs and newlines are whitespace too"},
		{"Shivam", "Shivam", "", "a single word yields no surname rather than an invented one"},
		{"", "", "", "no name at all"},
		{"   ", "", "", "whitespace only"},
		{"A B C D", "A B C", "D", "the last token is the surname however many precede it"},
		{"Rajesh  Kumar   Singh Yadav", "Rajesh Kumar Singh", "Yadav", "collapsing does not drop parts"},
		{"O'Brien", "O'Brien", "", "punctuation inside a single token is untouched"},
		{"Van Der Berg", "Van Der", "Berg", "multi-word surnames split at the last token -- a known limitation"},
	}
	for _, c := range cases {
		first, last := splitName(c.in)
		if first != c.first || last != c.last {
			t.Errorf("splitName(%q) = (%q, %q), want (%q, %q) -- %s", c.in, first, last, c.first, c.last, c.why)
		}
	}
}

// TestSingleWordNameStillSendsThePresentKey: the 422 was about presence, so a
// customer with one name must still produce the key.
func TestSingleWordNameStillSendsThePresentKey(t *testing.T) {
	payload := sendAndCapture(t, makOrder("Shivam"))

	if got := payload["billing_customer_name"]; got != "Shivam" {
		t.Errorf("billing_customer_name = %v", got)
	}
	v, present := payload["billing_last_name"]
	if !present {
		t.Fatal("billing_last_name is absent -- this is exactly what produced validation.present")
	}
	if v != "" {
		t.Errorf("billing_last_name = %v, want empty (no surname is invented)", v)
	}
}

// TestMissingCustomerNameStillSendsBothKeys: an order with no name at all must
// not drop the keys either.
func TestMissingCustomerNameStillSendsBothKeys(t *testing.T) {
	payload := sendAndCapture(t, makOrder(""))

	for _, k := range []string{"billing_customer_name", "billing_last_name"} {
		v, present := payload[k]
		if !present {
			t.Errorf("%s is absent", k)
			continue
		}
		if v != "" {
			t.Errorf("%s = %v, want empty", k, v)
		}
	}
}

// TestWhitespaceOnlyNameDoesNotProduceBlankPaddedFields guards against sending
// " " where Shiprocket expects a value or an empty string.
func TestWhitespaceOnlyNameDoesNotProduceBlankPaddedFields(t *testing.T) {
	payload := sendAndCapture(t, makOrder("   \t  "))

	if got := payload["billing_customer_name"]; got != "" {
		t.Errorf("billing_customer_name = %q, want empty rather than padding", got)
	}
	if got := payload["billing_last_name"]; got != "" {
		t.Errorf("billing_last_name = %q, want empty", got)
	}
}

// ---------------------------------------------------------------- field-by-field

// TestCreateOrderFieldValues checks each field the audit called out carries the
// value the MAK order actually holds.
func TestCreateOrderFieldValues(t *testing.T) {
	payload := sendAndCapture(t, makOrder("Shivam Mishra"))

	expect := map[string]any{
		"order_id":              "MAK-20260911-001",
		"order_date":            "2026-09-11 11:11",
		"pickup_location":       "work",
		"billing_customer_name": "Shivam",
		"billing_last_name":     "Mishra",
		"billing_address":       "Matva Street, Near Balaji Complex",
		"billing_city":          "Jetpur",
		"billing_pincode":       "360370",
		"billing_state":         "Gujarat",
		"billing_country":       "India",
		"billing_email":         "buyer@example.test",
		"billing_phone":         "9974959693",
		"shipping_is_billing":   true,
		"payment_method":        "Prepaid",
		"shipping_charges":      265.62,
		"sub_total":             float64(4999),
		"total_discount":        float64(0),
		"giftwrap_charges":      float64(0),
		"transaction_charges":   float64(0),
		// Centimetres, and kilograms from the 500g default.
		"length":  float64(15),
		"breadth": float64(10),
		"height":  float64(8),
		"weight":  0.5,
	}
	for k, want := range expect {
		if got := payload[k]; got != want {
			t.Errorf("%s = %#v, want %#v", k, got, want)
		}
	}

	items := payload["order_items"].([]any)
	item := items[0].(map[string]any)
	itemExpect := map[string]any{
		"name":          "NoiseFit Halo 3 AMOLED Smartwatch",
		"sku":           "6aa2d570b3f1a1920a1f3182",
		"units":         float64(1),
		"selling_price": float64(4999),
		// Present but empty: the order carries no discount, tax or HSN.
		"discount": "",
		"tax":      "",
		"hsn":      "",
	}
	for k, want := range itemExpect {
		if got := item[k]; got != want {
			t.Errorf("order_items[0].%s = %#v, want %#v", k, got, want)
		}
	}
}

// TestCODOrderSendsCODPaymentMethod: payment_method is derived from the order,
// not hardcoded.
func TestCODOrderSendsCODPaymentMethod(t *testing.T) {
	req := makOrder("Shivam Mishra")
	req.COD = true
	req.CODAmount = 5264.62
	payload := sendAndCapture(t, req)

	if got := payload["payment_method"]; got != "COD" {
		t.Errorf("payment_method = %v, want COD", got)
	}
}

// ---------------------------------------------------------------- the 422 itself

// TestProductionValidationErrorIsNormalized replays the exact response
// Shiprocket returned in production and checks it becomes the normalized
// error, with the provider's own wording confined to operator detail.
func TestProductionValidationErrorIsNormalized(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 422,
		fixture(t, "create_order_422_billing_last_name.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})

	_, err := p.CreateShipment(context.Background(), makOrder("Shivam Mishra"))
	if err == nil {
		t.Fatal("expected the 422 to fail the booking")
	}
	if code := shipping.CodeOf(err); code != shipping.CodeShipmentCreationFailed {
		t.Fatalf("code = %s, want %s", code, shipping.CodeShipmentCreationFailed)
	}

	se := shipping.AsError(err)
	// The operator needs to see which field the carrier objected to...
	if !strings.Contains(se.Detail, "billing_last_name") {
		t.Errorf("operator detail lost the carrier's reason: %q", se.Detail)
	}
	if !strings.Contains(se.Detail, "validation.present") {
		t.Errorf("operator detail lost the validation rule: %q", se.Detail)
	}
	// ...and the customer must not.
	if strings.Contains(se.Message, "billing_last_name") ||
		strings.Contains(se.Message, "validation") ||
		strings.Contains(se.Message, "Invalid Data") {
		t.Errorf("customer-facing message leaked provider detail: %q", se.Message)
	}
	// A 422 is our request being wrong, not the carrier being down.
	if se.HTTPStatus() < 400 {
		t.Errorf("HTTPStatus = %d", se.HTTPStatus())
	}
}

// TestCreateOrderRequestIsNotReplayedOnValidationFailure: a 422 is terminal,
// so the booking must not be retried into a duplicate parcel.
func TestCreateOrderRequestIsNotReplayedOnValidationFailure(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 422,
		fixture(t, "create_order_422_billing_last_name.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})

	if _, err := p.CreateShipment(context.Background(), makOrder("Shivam Mishra")); err == nil {
		t.Fatal("expected failure")
	}
	if got := m.callCount(http.MethodPost, "/orders/create/adhoc"); got != 1 {
		t.Fatalf("create attempted %d times, want 1", got)
	}
}

// TestOutgoingPayloadCarriesNoCredential is a belt-and-braces check that the
// request body cannot carry the account password or a token.
func TestOutgoingPayloadCarriesNoCredential(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 200, fixture(t, "create_order_success.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})
	if _, err := p.CreateShipment(context.Background(), makOrder("Shivam Mishra")); err != nil {
		t.Fatal(err)
	}

	body := m.lastBody(http.MethodPost, "/orders/create/adhoc")
	for _, secret := range []string{"test-password", "Bearer", "Authorization", "HEADER.PAYLOAD.SIGNATURE"} {
		if strings.Contains(body, secret) {
			t.Errorf("outgoing body contains %q", secret)
		}
	}
}

// ---------------------------------------------------------------- request log

// TestRejectedRequestLogRedactsPersonalDataButKeepsShape: the log exists to
// find a missing or empty field, so it must keep the keys and whether each
// held a value, while not printing the customer's details.
func TestRejectedRequestLogRedactsPersonalDataButKeepsShape(t *testing.T) {
	var buf bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(restore) }()

	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 422,
		fixture(t, "create_order_422_billing_last_name.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})

	if _, err := p.CreateShipment(context.Background(), makOrder("Shivam Mishra")); err == nil {
		t.Fatal("expected the 422 to fail")
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("a rejected booking logged nothing; the payload is undiagnosable")
	}

	// The customer's details must not be there.
	for _, personal := range []string{
		"Shivam", "Mishra", "Matva Street", "9974959693", "buyer@example.test",
		"NoiseFit Halo 3 AMOLED Smartwatch",
	} {
		if strings.Contains(logged, personal) {
			t.Errorf("log leaked personal data %q", personal)
		}
	}
	// No credential, by construction -- the body never carries one.
	for _, secret := range []string{"test-password", "Bearer", "Authorization"} {
		if strings.Contains(logged, secret) {
			t.Errorf("log leaked %q", secret)
		}
	}
	// But the diagnostic shape must survive.
	for _, key := range []string{
		"billing_last_name", "billing_customer_name", "shipping_is_billing",
		"order_items", "sub_total", "pickup_location",
	} {
		if !strings.Contains(logged, key) {
			t.Errorf("log dropped the key %q, which is what makes it useful", key)
		}
	}
	if !strings.Contains(logged, redactionMarker) {
		t.Error("expected redaction markers")
	}
	if !strings.Contains(logged, "422") {
		t.Error("log should record the status")
	}
	// A populated field and an empty one must be distinguishable.
	if !strings.Contains(logged, `"shipping_address":""`) {
		t.Errorf("an empty field should log as empty, not redacted: %s", logged)
	}
}

// TestSuccessfulRequestIsNotLogged: the payload is personal data, so it is
// logged only when something went wrong.
func TestSuccessfulRequestIsNotLogged(t *testing.T) {
	var buf bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&buf)
	defer func() { log.SetOutput(restore) }()

	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 200, fixture(t, "create_order_success.json"))
	p := newTestProvider(t, m, Config{PickupLocation: "work"})

	if _, err := p.CreateShipment(context.Background(), makOrder("Shivam Mishra")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "request sent") {
		t.Errorf("a successful booking logged its payload: %s", buf.String())
	}
}
