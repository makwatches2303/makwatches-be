package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// These tests drive the real /checkout endpoint over a throwaway database and
// prove the shipping charge is server-authoritative.
//
// Quotes are signed directly with the application's own Quoter rather than
// fetched from the carrier: the carrier call is covered by the Shiprocket unit
// tests and the live read-only check, and signing here keeps these tests
// offline and deterministic. What is under test is everything checkout does
// with a quote once it has one.
//
// COD is used throughout so no payment gateway is involved.

// checkoutFixture is a seeded customer with one product in their bag.
type checkoutFixture struct {
	db        *database.DBClient
	app       *fiber.App
	userID    primitive.ObjectID
	productID primitive.ObjectID
	price     float64
	token     string
	quoter    *shipping.Quoter
}

func newCheckoutFixture(t *testing.T) *checkoutFixture {
	t.Helper()
	db := idorDB(t)
	app := idorApp(t, db)
	ctx := context.Background()

	userID := primitive.NewObjectID()
	productID := primitive.NewObjectID()
	const price = 4999.0

	if _, err := db.MongoDB.Collection("products").InsertOne(ctx, bson.M{
		"_id":   productID,
		"name":  "Classic Watch",
		"brand": "MAK",
		"price": price,
		"stock": 10,
	}); err != nil {
		t.Fatalf("seeding product: %v", err)
	}
	if _, err := db.MongoDB.Collection("cart_items").InsertOne(ctx, bson.M{
		"user_id":    userID,
		"product_id": productID,
		"quantity":   1,
		"created_at": time.Now(),
	}); err != nil {
		t.Fatalf("seeding cart: %v", err)
	}

	return &checkoutFixture{
		db:        db,
		app:       app,
		userID:    userID,
		productID: productID,
		price:     price,
		token:     tokenFor(t, userID, "user"),
		// The service reuses the JWT secret for quote signing.
		quoter: shipping.NewQuoter(idorJWTSecret),
	}
}

// binding is the binding checkout will reconstruct for this fixture.
func (f *checkoutFixture) binding(quantity int, cod bool) shipping.QuoteBinding {
	return shipping.QuoteBinding{
		UserID: f.userID.Hex(),
		CartHash: shipping.CartFingerprint([]shipping.CartLine{
			{ProductID: f.productID.Hex(), Quantity: quantity},
		}),
		Pincode:     "400001",
		WeightGrams: 500,
		COD:         cod,
	}
}

func (f *checkoutFixture) signQuote(t *testing.T, opt shipping.RateOption, b shipping.QuoteBinding, at time.Time) string {
	t.Helper()
	token, err := f.quoter.Sign(opt, b, at)
	if err != nil {
		t.Fatalf("signing quote: %v", err)
	}
	return token
}

func courierOption(charge float64, cod bool) shipping.RateOption {
	return shipping.RateOption{
		ID:                    "shiprocket:51",
		Provider:              shipping.ProviderShiprocket,
		ProviderCourierID:     "51",
		CourierName:           "Test Courier",
		Charge:                charge,
		EstimatedDeliveryDays: 3,
		CODAvailable:          cod,
	}
}

// postCheckout submits an order and returns the status plus decoded body.
func (f *checkoutFixture) postCheckout(t *testing.T, payload map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/checkout", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.app.Test(req, -1)
	if err != nil {
		t.Fatalf("POST /checkout: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// codOrderPayload is a complete COD checkout body.
func codOrderPayload(quote string, clientTotal float64) map[string]any {
	return map[string]any{
		"shippingAddress": map[string]any{
			"name": "A Customer", "street": "12 Test Road", "city": "Mumbai",
			"state": "Maharashtra", "zipCode": "400001", "country": "India",
			"phone": "9999999999",
		},
		"paymentInfo":   map[string]any{"method": "cod"},
		"customerName":  "A Customer",
		"customerPhone": "9999999999",
		"shippingQuote": quote,
		"clientTotal":   clientTotal,
	}
}

// ------------------------------------------------------------------ happy path

// TestCheckoutAppliesTheQuotedShippingCharge is the fix for the audit's
// headline blocker: the selected option now reaches the order and is priced.
func TestCheckoutAppliesTheQuotedShippingCharge(t *testing.T) {
	f := newCheckoutFixture(t)

	const charge = 65.0
	quote := f.signQuote(t, courierOption(charge, true), f.binding(1, true), time.Now())

	status, body := f.postCheckout(t, codOrderPayload(quote, f.price+charge))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", status, body)
	}

	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("no order in response: %v", body)
	}
	if got := data["total"]; got != f.price+charge {
		t.Errorf("total = %v, want %v (goods + delivery)", got, f.price+charge)
	}
	if got := data["subtotal"]; got != f.price {
		t.Errorf("subtotal = %v, want %v", got, f.price)
	}
	if got := data["shippingCharge"]; got != charge {
		t.Errorf("shippingCharge = %v, want %v", got, charge)
	}

	// The snapshot must record the courier, not just the money.
	option, _ := data["shippingOption"].(map[string]any)
	if option == nil {
		t.Fatalf("shippingOption was not persisted: %v", data)
	}
	if option["courierName"] != "Test Courier" {
		t.Errorf("courierName = %v", option["courierName"])
	}
	if option["providerCourierId"] != "51" {
		t.Errorf("providerCourierId = %v", option["providerCourierId"])
	}
	if option["provider"] != shipping.ProviderShiprocket {
		t.Errorf("provider = %v", option["provider"])
	}
	if option["estimatedDeliveryDays"] != float64(3) {
		t.Errorf("estimatedDeliveryDays = %v", option["estimatedDeliveryDays"])
	}
}

// TestCheckoutWithoutAQuoteChargesNoShipping records the compatibility path: a
// client that sends no selection still places an order, at zero delivery
// charge, unless REQUIRE_SHIPPING_SELECTION is on.
func TestCheckoutWithoutAQuoteChargesNoShipping(t *testing.T) {
	f := newCheckoutFixture(t)

	payload := codOrderPayload("", f.price)
	delete(payload, "shippingQuote")

	status, body := f.postCheckout(t, payload)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	data, _ := body["data"].(map[string]any)
	if got := data["total"]; got != f.price {
		t.Errorf("total = %v, want the goods total %v", got, f.price)
	}
	if _, present := data["shippingOption"]; present {
		t.Error("no option was chosen, so none should be recorded")
	}
}

// ------------------------------------------------------------------ manipulation

// TestCheckoutRejectsManipulatedShippingPrice covers the whole class: the
// charge lives inside a signed token, so there is no field a client can edit
// to change it. Each case here is an attempt to pay a different delivery
// charge than the one that was quoted.
func TestCheckoutRejectsManipulatedShippingPrice(t *testing.T) {
	f := newCheckoutFixture(t)
	now := time.Now()
	valid := f.binding(1, true)

	// A quote signed by us for ₹65 is the only honest input. These forgeries
	// re-sign with the wrong key, or edit the payload.
	foreign := shipping.NewQuoter("not-the-server-secret")
	forged, err := foreign.Sign(courierOption(0, true), valid, now)
	if err != nil {
		t.Fatal(err)
	}

	genuine := f.signQuote(t, courierOption(65, true), valid, now)
	bodyHalf, sig, _ := splitToken(genuine)
	tampered := bodyHalf[:len(bodyHalf)-4] + "AAAA" + "." + sig

	cases := map[string]string{
		"quote signed with another key": forged,
		"payload edited":                tampered,
		"not a token":                   "garbage",
		"empty signature":               bodyHalf + ".",
	}
	for name, quote := range cases {
		status, body := f.postCheckout(t, codOrderPayload(quote, f.price))
		if status == http.StatusCreated {
			t.Errorf("%s: order was created; the charge is not authoritative", name)
			continue
		}
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%v)", name, status, body)
		}
	}
}

// TestCheckoutRejectsAnotherCustomersQuote: quotes are bound to the customer
// they were issued to, so one cannot be shared or replayed.
func TestCheckoutRejectsAnotherCustomersQuote(t *testing.T) {
	f := newCheckoutFixture(t)

	someoneElse := f.binding(1, true)
	someoneElse.UserID = primitive.NewObjectID().Hex()
	quote := f.signQuote(t, courierOption(5, true), someoneElse, time.Now())

	status, _ := f.postCheckout(t, codOrderPayload(quote, f.price+5))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for another customer's quote", status)
	}
}

// TestCheckoutRejectsQuoteForADifferentDestination: a cheap metro quote must
// not be usable to ship somewhere remote.
func TestCheckoutRejectsQuoteForADifferentDestination(t *testing.T) {
	f := newCheckoutFixture(t)

	elsewhere := f.binding(1, true)
	elsewhere.Pincode = "797001"
	quote := f.signQuote(t, courierOption(65, true), elsewhere, time.Now())

	status, _ := f.postCheckout(t, codOrderPayload(quote, f.price+65))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a quote issued elsewhere", status)
	}
}

// TestCheckoutRejectsQuoteAfterTheBagChanges: quoting one watch and shipping
// two must fail.
func TestCheckoutRejectsQuoteAfterTheBagChanges(t *testing.T) {
	f := newCheckoutFixture(t)

	// Quote the bag as it stands (one unit)...
	quote := f.signQuote(t, courierOption(65, true), f.binding(1, true), time.Now())

	// ...then add another unit before submitting.
	if _, err := f.db.MongoDB.Collection("cart_items").UpdateOne(context.Background(),
		bson.M{"user_id": f.userID, "product_id": f.productID},
		bson.M{"$set": bson.M{"quantity": 2}}); err != nil {
		t.Fatal(err)
	}

	status, _ := f.postCheckout(t, codOrderPayload(quote, f.price*2+65))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 once the bag no longer matches the quote", status)
	}
}

func TestCheckoutRejectsExpiredQuote(t *testing.T) {
	f := newCheckoutFixture(t)

	// Signed far enough in the past that its TTL has run out.
	stale := time.Now().Add(-(shipping.QuoteTTL + time.Hour))
	quote := f.signQuote(t, courierOption(65, true), f.binding(1, true), stale)

	status, body := f.postCheckout(t, codOrderPayload(quote, f.price+65))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an expired quote (%v)", status, body)
	}
	// The code lets the client know a re-quote will fix it.
	if body["code"] != string(shipping.CodeRateUnavailable) {
		t.Errorf("code = %v, want %s", body["code"], shipping.CodeRateUnavailable)
	}
}

// TestCheckoutRejectsQuoteIssuedForTheOtherPaymentMode: carriers price COD
// differently, so a prepaid quote cannot pay for a COD shipment.
func TestCheckoutRejectsQuoteIssuedForTheOtherPaymentMode(t *testing.T) {
	f := newCheckoutFixture(t)

	prepaidQuote := f.signQuote(t, courierOption(65, true), f.binding(1, false), time.Now())

	status, _ := f.postCheckout(t, codOrderPayload(prepaidQuote, f.price+65))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a prepaid quote on a COD order", status)
	}
}

// TestCheckoutRejectsCODOnACourierThatDoesNotOfferIt: the courier's own COD
// flag decides, not the client's choice of payment method.
func TestCheckoutRejectsCODOnACourierThatDoesNotOfferIt(t *testing.T) {
	f := newCheckoutFixture(t)

	// A genuine, correctly-bound COD-mode quote -- for a courier that does
	// not carry COD.
	quote := f.signQuote(t, courierOption(65, false), f.binding(1, true), time.Now())

	status, body := f.postCheckout(t, codOrderPayload(quote, f.price+65))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", status, body)
	}
	message, _ := body["message"].(string)
	if message == "" {
		t.Error("the customer should be told why, so they can pick another courier")
	}
}

// TestCheckoutTotalMismatchIsStillCaught: the client-reported total remains a
// tripwire, now measured against goods + delivery.
func TestCheckoutTotalMismatchIsStillCaught(t *testing.T) {
	f := newCheckoutFixture(t)

	quote := f.signQuote(t, courierOption(65, true), f.binding(1, true), time.Now())

	// The client claims the pre-shipping total, as a stale page would.
	status, _ := f.postCheckout(t, codOrderPayload(quote, f.price))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the displayed total omits delivery", status)
	}
}

// TestCheckoutDoesNotCommitStockOnARejectedQuote: a refused order must leave
// inventory alone.
func TestCheckoutDoesNotCommitStockOnARejectedQuote(t *testing.T) {
	f := newCheckoutFixture(t)
	ctx := context.Background()

	quote := f.signQuote(t, courierOption(65, true), f.binding(1, false), time.Now())
	if status, _ := f.postCheckout(t, codOrderPayload(quote, f.price+65)); status == http.StatusCreated {
		t.Fatal("the mismatched quote should have been refused")
	}

	var product struct {
		Stock int `bson:"stock"`
	}
	if err := f.db.MongoDB.Collection("products").
		FindOne(ctx, bson.M{"_id": f.productID}).Decode(&product); err != nil {
		t.Fatal(err)
	}
	if product.Stock != 10 {
		t.Fatalf("stock = %d, want 10 untouched after a rejected checkout", product.Stock)
	}
}

// ------------------------------------------------------------------ options endpoint

// TestShippingOptionsRequiresAuthentication: the bound quotes are per-customer,
// so the endpoint cannot be anonymous.
func TestShippingOptionsRequiresAuthentication(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/checkout/shipping-options",
		bytes.NewReader([]byte(`{"pincode":"400001"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestShippingOptionsRejectsAnEmptyBag: there is nothing to quote, and the
// carrier should not be asked.
func TestShippingOptionsRejectsAnEmptyBag(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)
	userID := primitive.NewObjectID()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/checkout/shipping-options",
		bytes.NewReader([]byte(`{"pincode":"400001"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, userID, "user"))

	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty bag", resp.StatusCode)
	}
}

func TestShippingOptionsRequiresAPincode(t *testing.T) {
	f := newCheckoutFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/checkout/shipping-options",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// splitToken splits a quote into its payload and signature halves.
func splitToken(token string) (string, string, bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return token[:i], token[i+1:], true
		}
	}
	return token, "", false
}
