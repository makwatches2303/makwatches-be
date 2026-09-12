package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
)

// shippingApp builds the real route table over an unusable database.
//
// As with newRoutedApp, the assertions here are about routing, authentication
// and webhook authorization -- all of which happen before any database access.
// A request that gets past those panics on the nil client and is recovered
// into a 500, so "500" means "it got through" and "401" means "it was
// refused". No MongoDB is required.
func shippingApp(t *testing.T, cfg *config.Config) *fiber.App {
	t.Helper()
	app := fiber.New()
	if cfg.JWTSecret == "" {
		cfg.JWTSecret = "test-secret"
	}
	SetupRoutes(app, &database.DBClient{}, cfg)
	return app
}

func postJSON(t *testing.T, app *fiber.App, path, body string, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

const validShiprocketWebhook = `{"awb":"1491110022334","shipment_status_id":7,"sr_order_id":16161616,"current_timestamp":"2024-07-05 15:42:00"}`

// TestShippingWebhooksFailClosedWithoutSecret is the regression test for the
// audit's headline finding: the Delhivery callback accepted any anonymous
// caller, and the same handler settled COD payment.
//
// With no secret configured there is no way to distinguish a real callback
// from a forged one, so both endpoints must refuse rather than trust.
func TestShippingWebhooksFailClosedWithoutSecret(t *testing.T) {
	// Deliberately no webhook secrets configured.
	app := shippingApp(t, &config.Config{})

	cases := []struct{ name, path, body string }{
		{"delhivery", "/webhooks/delhivery", `{"Waybill":"1491110022334","StatusType":"DL"}`},
		{"shiprocket", "/webhooks/shipping/shiprocket", validShiprocketWebhook},
		{"delhivery-v2-path", "/webhooks/shipping/delhivery", `{"Waybill":"1491110022334","StatusType":"DL"}`},
	}
	for _, tc := range cases {
		got := postJSON(t, app, tc.path, tc.body, nil)
		if got != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 (must not be trusted with no secret configured)", tc.name, got)
		}
	}
}

func TestShiprocketWebhookRejectsMissingAndWrongKey(t *testing.T) {
	app := shippingApp(t, &config.Config{ShiprocketWebhookSecret: "the-real-secret"})

	if got := postJSON(t, app, "/webhooks/shipping/shiprocket", validShiprocketWebhook, nil); got != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", got)
	}
	if got := postJSON(t, app, "/webhooks/shipping/shiprocket", validShiprocketWebhook,
		map[string]string{"X-Api-Key": "guessed"}); got != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", got)
	}
}

// TestShiprocketWebhookWithValidKeyReachesTheService proves the secret is what
// gates the endpoint: with the right key the request gets past authentication
// and on to shipment resolution, which panics on the nil database and is
// recovered into a 500.
func TestShiprocketWebhookWithValidKeyReachesTheService(t *testing.T) {
	app := shippingApp(t, &config.Config{ShiprocketWebhookSecret: "the-real-secret"})

	got := postJSON(t, app, "/webhooks/shipping/shiprocket", validShiprocketWebhook,
		map[string]string{"X-Api-Key": "the-real-secret"})
	if got == http.StatusUnauthorized {
		t.Fatal("a correctly-signed callback was refused")
	}
	if got != http.StatusInternalServerError {
		t.Logf("status = %d (expected 500 from the nil database once past auth)", got)
	}
}

func TestDelhiveryWebhookAcceptsConfiguredSecret(t *testing.T) {
	app := shippingApp(t, &config.Config{DelhiveryWebhookSecret: "delhivery-secret"})
	body := `{"Waybill":"1491110022334","StatusType":"DL","StatusDateTime":"2024-07-05T15:42:00.000"}`

	// Header form.
	if got := postJSON(t, app, "/webhooks/delhivery", body,
		map[string]string{"X-Delhivery-Token": "delhivery-secret"}); got == http.StatusUnauthorized {
		t.Error("the header form of the shared secret was refused")
	}
	// Query form, which is what a freely-configurable callback URL allows.
	if got := postJSON(t, app, "/webhooks/delhivery?token=delhivery-secret", body, nil); got == http.StatusUnauthorized {
		t.Error("the query form of the shared secret was refused")
	}
	// And a wrong one is still refused.
	if got := postJSON(t, app, "/webhooks/delhivery?token=wrong", body, nil); got != http.StatusUnauthorized {
		t.Errorf("wrong query secret: status = %d, want 401", got)
	}
}

func TestDelhiveryWebhookRejectsMalformedBody(t *testing.T) {
	app := shippingApp(t, &config.Config{DelhiveryWebhookSecret: "delhivery-secret"})

	if got := postJSON(t, app, "/webhooks/delhivery?token=delhivery-secret",
		`<html>not json</html>`, nil); got != http.StatusUnauthorized {
		// Rejected at the parse stage, reported the same way so an
		// unauthenticated caller learns nothing about which check failed.
		t.Errorf("malformed body: status = %d, want 401", got)
	}
}

// TestV1ShippingMutationsRequireAuthentication covers the fulfillment surface.
func TestV1ShippingMutationsRequireAuthentication(t *testing.T) {
	app := shippingApp(t, &config.Config{})
	orderID := "507f1f77bcf86cd799439011"

	protected := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/create"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/awb"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/cancel"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/pickup"},
		{http.MethodGet, "/api/v1/shipping/orders/" + orderID + "/tracking"},
		{http.MethodGet, "/api/v1/shipping/orders/" + orderID + "/label"},
		{http.MethodGet, "/api/v1/shipping/pickup-locations"},
	}
	for _, route := range protected {
		if got := statusFor(t, app, route.method, route.path); got != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", route.method, route.path, got)
		}
	}
}

// TestLegacyAdminShippingRoutesRequireAuthentication guards the original flat
// admin surface, which stays mounted.
func TestLegacyAdminShippingRoutesRequireAuthentication(t *testing.T) {
	app := shippingApp(t, &config.Config{})
	orderID := "507f1f77bcf86cd799439011"

	protected := []struct{ method, path string }{
		{http.MethodPost, "/admin/shipping/orders/" + orderID + "/retry"},
		{http.MethodPost, "/admin/shipping/orders/" + orderID + "/cancel"},
		{http.MethodGet, "/admin/shipping/orders/" + orderID + "/label"},
		{http.MethodPost, "/admin/shipping/bulk-track"},
		{http.MethodPost, "/admin/shipping/request-pickup"},
		{http.MethodGet, "/shipping/track/order/" + orderID},
	}
	for _, route := range protected {
		if got := statusFor(t, app, route.method, route.path); got != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", route.method, route.path, got)
		}
	}
}

// TestShippingRateRoutesArePublic: the checkout page needs delivery options
// before an account exists, and quoting a rate never books a parcel.
func TestShippingRateRoutesArePublic(t *testing.T) {
	app := shippingApp(t, &config.Config{})

	public := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/shipping/serviceability"},
		{http.MethodPost, "/api/v1/shipping/rates"},
		{http.MethodGet, "/api/v1/shipping/serviceability?pincode=360370"},
		{http.MethodGet, "/shipping/check-pincode/360370"},
		{http.MethodGet, "/shipping/check-pincode?pincode=360370"},
	}
	for _, route := range public {
		if got := statusFor(t, app, route.method, route.path); got == http.StatusUnauthorized {
			t.Errorf("%s %s answered 401; this route must stay public", route.method, route.path)
		}
	}
}

// TestServiceabilityRejectsMissingPincodeWithoutCallingCarrier: a bad request
// is answered before any carrier or database work.
func TestServiceabilityRejectsMissingPincode(t *testing.T) {
	app := shippingApp(t, &config.Config{})

	if got := postJSON(t, app, "/api/v1/shipping/serviceability", `{}`, nil); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
	if got := statusFor(t, app, http.MethodGet, "/shipping/check-pincode/"); got == http.StatusOK {
		t.Error("an empty pincode must not answer 200")
	}
}
