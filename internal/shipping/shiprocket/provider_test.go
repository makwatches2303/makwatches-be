package shiprocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// fixture loads a recorded Shiprocket response.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// route is one canned reply from the mock carrier.
type route struct {
	status  int
	body    []byte
	headers map[string]string
	// bodies, when set, is consumed one entry per call, so a test can make the
	// first attempt fail and the second succeed.
	bodies   [][]byte
	statuses []int
}

// mockShiprocket is a stand-in for the carrier API.
//
// It is a TLS server so the label download path -- which refuses plain http --
// is exercised as written rather than special-cased for tests.
type mockShiprocket struct {
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[string]*route
	requests map[string][]string // method+path -> captured bodies
	calls    map[string]*int32
}

func newMockShiprocket(t *testing.T) *mockShiprocket {
	t.Helper()
	m := &mockShiprocket{
		routes:   map[string]*route{},
		requests: map[string][]string{},
		calls:    map[string]*int32{},
	}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(m.handle))
	// Silence the handshake noise the concurrency test produces when the
	// server shuts down with connections still open.
	m.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockShiprocket) on(method, path string, status int, body []byte) *route {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &route{status: status, body: body}
	m.routes[method+" "+path] = r
	return r
}

func (m *mockShiprocket) handle(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path

	body := make([]byte, 0)
	if r.Body != nil {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
	}

	m.mu.Lock()
	if m.calls[key] == nil {
		var zero int32
		m.calls[key] = &zero
	}
	n := atomic.AddInt32(m.calls[key], 1)
	m.requests[key] = append(m.requests[key], string(body))
	rt := m.routes[key]
	m.mu.Unlock()

	if rt == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"no mock route"}`))
		return
	}

	status, payload := rt.status, rt.body
	if len(rt.statuses) > 0 {
		idx := int(n) - 1
		if idx >= len(rt.statuses) {
			idx = len(rt.statuses) - 1
		}
		status = rt.statuses[idx]
	}
	if len(rt.bodies) > 0 {
		idx := int(n) - 1
		if idx >= len(rt.bodies) {
			idx = len(rt.bodies) - 1
		}
		payload = rt.bodies[idx]
	}
	for k, v := range rt.headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_, _ = w.Write(payload)
	}
}

func (m *mockShiprocket) callCount(method, path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.calls[method+" "+path]; c != nil {
		return int(atomic.LoadInt32(c))
	}
	return 0
}

func (m *mockShiprocket) lastBody(method, path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	bodies := m.requests[method+" "+path]
	if len(bodies) == 0 {
		return ""
	}
	return bodies[len(bodies)-1]
}

// newTestProvider wires a provider at the mock, with backoff neutralized and
// the mock's certificate trusted.
func newTestProvider(t *testing.T, m *mockShiprocket, cfg Config) *Provider {
	t.Helper()
	if cfg.Email == "" {
		cfg.Email = "api@example.test"
	}
	if cfg.Password == "" {
		cfg.Password = "test-password"
	}
	cfg.BaseURL = m.srv.URL
	if cfg.PickupLocation == "" {
		cfg.PickupLocation = "Primary"
	}

	p := New(cfg)
	// Same package, so the transport internals are reachable: trust the mock's
	// self-signed certificate and make backoff instant.
	p.tr.http = m.srv.Client()
	p.tr.sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

func mockAuth(m *mockShiprocket, t *testing.T) {
	m.on(http.MethodPost, "/auth/login", 200, fixture(t, "auth_success.json"))
}

// ------------------------------------------------------------------ auth

func TestLoginSucceedsAndTokenIsCached(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/settings/company/pickup", 200, fixture(t, "pickup_locations.json"))
	p := newTestProvider(t, m, Config{})

	for i := 0; i < 3; i++ {
		if _, err := p.PickupLocations(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 1 {
		t.Fatalf("expected exactly one login for three API calls, got %d", got)
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	m := newMockShiprocket(t)
	m.on(http.MethodPost, "/auth/login", 401, fixture(t, "error_401.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.PickupLocations(context.Background())
	if err == nil {
		t.Fatal("expected an error for rejected credentials")
	}
	if code := shipping.CodeOf(err); code != shipping.CodeProviderAuthFailed {
		t.Fatalf("code = %s, want %s", code, shipping.CodeProviderAuthFailed)
	}
	// The failure must not echo the password anywhere.
	if strings.Contains(err.Error(), "test-password") {
		t.Fatal("error text leaked the account password")
	}
}

func TestMissingCredentialsFailWithoutCallingCarrier(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})
	p.cfg.Email = ""
	p.tokens.email = ""

	if _, err := p.PickupLocations(context.Background()); err == nil {
		t.Fatal("expected an error with no credentials configured")
	}
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 0 {
		t.Fatalf("carrier was called %d times with no credentials", got)
	}
}

func TestTokenExpiryReadFromJWTClaim(t *testing.T) {
	exp := time.Now().Add(240 * time.Hour).Unix()
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	token := "h." + claims + ".s"

	m := newMockShiprocket(t)
	m.on(http.MethodPost, "/auth/login", 200, []byte(`{"token":"`+token+`"}`))
	m.on(http.MethodGet, "/settings/company/pickup", 200, fixture(t, "pickup_locations.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.PickupLocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(exp, 0).Add(-expirySkew)
	if diff := p.tokens.expiresAt.Sub(want); diff > time.Second || diff < -time.Second {
		t.Fatalf("expiry = %v, want ~%v", p.tokens.expiresAt, want)
	}
}

func TestExpiredTokenTriggersReauthentication(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/settings/company/pickup", 200, fixture(t, "pickup_locations.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.PickupLocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Age the cached token past its expiry.
	p.tokens.mu.Lock()
	p.tokens.expiresAt = time.Now().Add(-time.Minute)
	p.tokens.mu.Unlock()

	if _, err := p.PickupLocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 2 {
		t.Fatalf("expected a second login after expiry, got %d logins", got)
	}
}

func TestConcurrentCallsAuthenticateOnce(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/settings/company/pickup", 200, fixture(t, "pickup_locations.json"))
	p := newTestProvider(t, m, Config{})

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.PickupLocations(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent call failed: %v", err)
	}

	// Single-flight: a burst of concurrent callers shares one login.
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 1 {
		t.Fatalf("expected 1 login for %d concurrent calls, got %d", n, got)
	}
}

func TestUnauthorizedResponseReauthenticatesExactlyOnce(t *testing.T) {
	m := newMockShiprocket(t)
	// Two different tokens, so the retry uses a genuinely new one.
	login := m.on(http.MethodPost, "/auth/login", 200, nil)
	login.bodies = [][]byte{
		[]byte(`{"token":"first.token.sig"}`),
		[]byte(`{"token":"second.token.sig"}`),
	}
	pickup := m.on(http.MethodGet, "/settings/company/pickup", 200, nil)
	pickup.statuses = []int{401, 200}
	pickup.bodies = [][]byte{fixture(t, "error_401.json"), fixture(t, "pickup_locations.json")}

	p := newTestProvider(t, m, Config{})
	if _, err := p.PickupLocations(context.Background()); err != nil {
		t.Fatalf("expected the retried call to succeed: %v", err)
	}
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 2 {
		t.Fatalf("logins = %d, want 2", got)
	}
	if got := m.callCount(http.MethodGet, "/settings/company/pickup"); got != 2 {
		t.Fatalf("api calls = %d, want 2 (one 401, one retry)", got)
	}
}

func TestPersistent401DoesNotLoop(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t) // always the same token
	m.on(http.MethodGet, "/settings/company/pickup", 401, fixture(t, "error_401.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.PickupLocations(context.Background())
	if err == nil {
		t.Fatal("expected failure")
	}
	// The token never changes, so the adapter must give up rather than
	// re-authenticate forever.
	if got := m.callCount(http.MethodPost, "/auth/login"); got > 2 {
		t.Fatalf("logins = %d, want at most 2", got)
	}
}

func TestAuthFailureEntersCooldown(t *testing.T) {
	m := newMockShiprocket(t)
	m.on(http.MethodPost, "/auth/login", 401, fixture(t, "error_401.json"))
	p := newTestProvider(t, m, Config{})

	for i := 0; i < 5; i++ {
		if _, err := p.PickupLocations(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
	}
	// Repeated requests must not each drive a login attempt: that is how an
	// account gets locked out.
	if got := m.callCount(http.MethodPost, "/auth/login"); got != 1 {
		t.Fatalf("logins = %d, want 1 (cooldown should suppress the rest)", got)
	}
}

// ------------------------------------------------------------------ rates

func ratesProvider(t *testing.T) (*Provider, *mockShiprocket) {
	t.Helper()
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 200, fixture(t, "serviceability_success.json"))
	return newTestProvider(t, m, Config{}), m
}

func baseRateRequest() shipping.RateRequest {
	return shipping.RateRequest{
		PickupPincode:   "360370",
		DeliveryPincode: "400001",
		Package:         shipping.PackageSpec{WeightGrams: 500, LengthCm: 15, BreadthCm: 10, HeightCm: 8},
		DeclaredValue:   4999,
	}
}

func TestRatesNormalizesCourierList(t *testing.T) {
	p, _ := ratesProvider(t)

	options, err := p.Rates(context.Background(), baseRateRequest())
	if err != nil {
		t.Fatal(err)
	}

	// Blocked, id-less and unpriced couriers are all dropped, leaving two.
	if len(options) != 2 {
		names := make([]string, 0, len(options))
		for _, o := range options {
			names = append(names, o.CourierName)
		}
		t.Fatalf("options = %v, want exactly the two usable couriers", names)
	}

	byID := map[string]shipping.RateOption{}
	for _, o := range options {
		byID[o.ProviderCourierID] = o
	}

	surface, ok := byID["51"]
	if !ok {
		t.Fatal("courier 51 missing")
	}
	if surface.Charge != 54 {
		t.Errorf("charge = %v, want 54 (the carrier's `rate`)", surface.Charge)
	}
	if surface.EstimatedDeliveryDays != 4 {
		t.Errorf("days = %d, want 4", surface.EstimatedDeliveryDays)
	}
	if surface.ETD != "Jul 01, 2024" {
		t.Errorf("etd = %q", surface.ETD)
	}
	if !surface.CODAvailable {
		t.Error("courier 51 should offer COD")
	}
	if !surface.Recommended {
		t.Error("courier 51 is recommended_courier_company_id and should be flagged")
	}
	if surface.ID != "shiprocket:51" {
		t.Errorf("id = %q", surface.ID)
	}

	air, ok := byID["24"]
	if !ok {
		t.Fatal("courier 24 missing")
	}
	// rate is 0 here, so the component sum must be used instead: freight 96 +
	// cod 0 + other 4.
	if air.Charge != 100 {
		t.Errorf("charge = %v, want 100 (freight+cod+other when rate is absent)", air.Charge)
	}
	if air.CODAvailable {
		t.Error("courier 24 reports cod=0 and must not offer COD")
	}
	if !air.Recommended {
		t.Error("courier 24 is shiprocket_recommended_courier_id and should be flagged")
	}
}

func TestRatesDoesNotTreatFirstEntryAsRecommended(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// No recommendation ids at all.
	m.on(http.MethodGet, "/courier/serviceability/", 200, []byte(`{
	  "data": {"available_courier_companies": [
	    {"courier_company_id": 7, "courier_name": "First", "rate": 40, "cod": 1, "blocked": 0},
	    {"courier_company_id": 8, "courier_name": "Second", "rate": 30, "cod": 1, "blocked": 0}
	  ]}}`))
	p := newTestProvider(t, m, Config{})

	options, err := p.Rates(context.Background(), baseRateRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range options {
		if o.Recommended {
			t.Fatalf("%s was flagged recommended with no carrier recommendation present", o.CourierName)
		}
	}
}

func TestRatesSendsWeightInKilograms(t *testing.T) {
	p, m := ratesProvider(t)
	req := baseRateRequest()
	req.Package.WeightGrams = 750

	if _, err := p.Rates(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// The subsystem carries grams; Shiprocket expects kilograms.
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for key := range m.calls {
		if strings.Contains(key, "/courier/serviceability/") {
			found = true
		}
	}
	if !found {
		t.Fatal("serviceability was not called")
	}
}

func TestRatesRejectsMalformedPincode(t *testing.T) {
	p, m := ratesProvider(t)
	req := baseRateRequest()
	req.DeliveryPincode = "40001"

	_, err := p.Rates(context.Background(), req)
	if shipping.CodeOf(err) != shipping.CodeInvalidPincode {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeInvalidPincode)
	}
	if m.callCount(http.MethodGet, "/courier/serviceability/") != 0 {
		t.Fatal("a malformed pincode must not reach the carrier")
	}
}

func TestRatesReportsNotServiceableOn404(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 404, []byte(`{"message":"no couriers"}`))
	p := newTestProvider(t, m, Config{})

	_, err := p.Rates(context.Background(), baseRateRequest())
	if shipping.CodeOf(err) != shipping.CodeNotServiceable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeNotServiceable)
	}
}

func TestRatesReportsRateUnavailableWhenAllCouriersUnusable(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 200, []byte(`{
	  "data": {"available_courier_companies": [
	    {"courier_company_id": 9, "courier_name": "Blocked", "rate": 50, "blocked": 1}
	  ]}}`))
	p := newTestProvider(t, m, Config{})

	_, err := p.Rates(context.Background(), baseRateRequest())
	if shipping.CodeOf(err) != shipping.CodeRateUnavailable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeRateUnavailable)
	}
}

func TestRatesHandlesMalformedResponse(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 200, []byte(`<html>gateway error</html>`))
	p := newTestProvider(t, m, Config{})

	_, err := p.Rates(context.Background(), baseRateRequest())
	if err == nil {
		t.Fatal("expected an error for an unreadable body")
	}
	if shipping.CodeOf(err) != shipping.CodeRateUnavailable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeRateUnavailable)
	}
}

// ------------------------------------------------------------------ create

func createRequest() shipping.CreateShipmentRequest {
	addr := shipping.ShipmentAddress{
		Name: "A Customer", Line1: "12 Test Road", City: "Mumbai",
		State: "Maharashtra", Pincode: "400001", Country: "India",
		Phone: "9999999999", Email: "buyer@example.test",
	}
	return shipping.CreateShipmentRequest{
		OrderRef:          "MAK-20240701-001",
		OrderDate:         time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC),
		PickupLocation:    "Primary",
		Billing:           addr,
		Shipping:          addr,
		ShippingIsBilling: true,
		Lines: []shipping.ShipmentLine{
			{Name: "Classic Watch", SKU: "sku-1", Units: 1, SellingPrice: 4999},
		},
		SubTotal: 4999,
		Package:  shipping.PackageSpec{WeightGrams: 500, LengthCm: 15, BreadthCm: 10, HeightCm: 8},
	}
}

func TestCreateShipmentSucceeds(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 200, fixture(t, "create_order_success.json"))
	p := newTestProvider(t, m, Config{})

	result, err := p.CreateShipment(context.Background(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The three identifiers must stay distinct.
	if result.ProviderOrderID != "16161616" {
		t.Errorf("providerOrderID = %q, want 16161616", result.ProviderOrderID)
	}
	if result.ProviderShipmentID != "15151515" {
		t.Errorf("providerShipmentID = %q, want 15151515", result.ProviderShipmentID)
	}
	// awb_code is null on a fresh custom order.
	if result.TrackingNumber != "" {
		t.Errorf("trackingNumber = %q, want empty until AWB assignment", result.TrackingNumber)
	}
}

func TestCreateShipmentSendsKilogramsAndOmitsUnknownFields(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 200, fixture(t, "create_order_success.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.CreateShipment(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(m.lastBody(http.MethodPost, "/orders/create/adhoc")), &payload); err != nil {
		t.Fatalf("request body was not valid JSON: %v", err)
	}
	if got := payload["weight"]; got != 0.5 {
		t.Errorf("weight = %v, want 0.5 (kg)", got)
	}
	if got := payload["order_id"]; got != "MAK-20240701-001" {
		t.Errorf("order_id = %v, want the MAK order number", got)
	}
	if got := payload["payment_method"]; got != "Prepaid" {
		t.Errorf("payment_method = %v", got)
	}
	// Fields outside Shiprocket's documented contract stay absent when we have
	// no value for them.
	for _, key := range []string{"customer_gstin", "ewaybill_no"} {
		if _, present := payload[key]; present {
			t.Errorf("%s should be omitted when unknown, not sent empty", key)
		}
	}

	// Fields *inside* the contract are a different matter: Shiprocket
	// validates them with Laravel's `present` rule, so the key must exist and
	// only its value may be empty. This test previously asserted the opposite
	// -- that they be omitted -- which is precisely what produced
	// `billing_last_name: ["validation.present"]` in production. The honest
	// part of the original intent survives: no value is invented, the field is
	// simply empty. See create_order_contract_test.go.
	items, _ := payload["order_items"].([]any)
	if len(items) != 1 {
		t.Fatalf("order_items = %v", payload["order_items"])
	}
	item, _ := items[0].(map[string]any)
	for _, key := range []string{"hsn", "tax", "discount"} {
		v, present := item[key]
		if !present {
			t.Errorf("order item %s must be present for the `present` rule", key)
			continue
		}
		if v != "" {
			t.Errorf("order item %s = %#v, want empty rather than an invented value", key, v)
		}
	}
}

func TestCreateShipmentMapsValidationFailure(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/create/adhoc", 422, fixture(t, "error_422.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.CreateShipment(context.Background(), createRequest())
	if shipping.CodeOf(err) != shipping.CodeShipmentCreationFailed {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeShipmentCreationFailed)
	}
	// The provider's field-level complaint belongs in operator detail only.
	se := shipping.AsError(err)
	if strings.Contains(se.Message, "billing_pincode") {
		t.Fatal("customer-facing message leaked the provider validation detail")
	}
	if !strings.Contains(se.Detail, "billing_pincode") {
		t.Fatalf("operator detail lost the provider reason: %q", se.Detail)
	}
}

func TestCreateShipmentRequiresLineItems(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})

	req := createRequest()
	req.Lines = nil
	if _, err := p.CreateShipment(context.Background(), req); err == nil {
		t.Fatal("expected an error with no line items")
	}
	if m.callCount(http.MethodPost, "/orders/create/adhoc") != 0 {
		t.Fatal("an empty order must not reach the carrier")
	}
}

func TestCreateShipmentRequiresPickupLocation(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})
	p.cfg.PickupLocation = ""

	req := createRequest()
	req.PickupLocation = ""
	if _, err := p.CreateShipment(context.Background(), req); err == nil {
		t.Fatal("expected an error with no pickup location")
	}
}

func TestCreateShipmentIsNotReplayedOnTransportFailure(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// A 500 is transient, but a create must still not be replayed: the carrier
	// may have booked it.
	m.on(http.MethodPost, "/orders/create/adhoc", 500, fixture(t, "error_500.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.CreateShipment(context.Background(), createRequest()); err == nil {
		t.Fatal("expected failure")
	}
	if got := m.callCount(http.MethodPost, "/orders/create/adhoc"); got != 1 {
		t.Fatalf("create was attempted %d times; a booking must never be replayed", got)
	}
}

// ------------------------------------------------------------------ AWB

func TestAssignAWBSucceeds(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/courier/assign/awb", 200, fixture(t, "awb_success.json"))
	p := newTestProvider(t, m, Config{})

	result, err := p.AssignAWB(context.Background(), shipping.AssignAWBRequest{
		ProviderShipmentID: "15151515", CourierID: "51",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TrackingNumber != "1491110022334" {
		t.Errorf("awb = %q", result.TrackingNumber)
	}
	if result.CourierCompanyID != "51" {
		t.Errorf("courierCompanyID = %q", result.CourierCompanyID)
	}
	if result.CourierName != "Delhivery Surface" {
		t.Errorf("courierName = %q", result.CourierName)
	}
	// assigned_date_time arrives as a PHP-style object.
	if result.AssignedAt.IsZero() {
		t.Error("assignedAt was not parsed")
	}
}

func TestAssignAWBFailureIsClassified(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// The failure shape puts a bare string where the success shape has an
	// object, which must not crash the decode.
	m.on(http.MethodPost, "/courier/assign/awb", 200, fixture(t, "awb_failure.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.AssignAWB(context.Background(), shipping.AssignAWBRequest{ProviderShipmentID: "15151515"})
	if shipping.CodeOf(err) != shipping.CodeAWBAssignmentFailed {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeAWBAssignmentFailed)
	}
}

func TestAssignAWBRequiresShipmentID(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})

	if _, err := p.AssignAWB(context.Background(), shipping.AssignAWBRequest{}); err == nil {
		t.Fatal("expected an error without a shipment id")
	}
	if m.callCount(http.MethodPost, "/courier/assign/awb") != 0 {
		t.Fatal("carrier was called without a shipment id")
	}
}

// ------------------------------------------------------------------ tracking

func TestTrackByShipmentID(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/track/shipment/15151515", 200, fixture(t, "tracking_success.json"))
	p := newTestProvider(t, m, Config{})

	tracking, err := p.Track(context.Background(), shipping.ShipmentRef{
		ProviderShipmentID: "15151515", TrackingNumber: "1491110022334",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tracking.Status != shipping.StatusDelivered {
		t.Errorf("status = %q, want %q", tracking.Status, shipping.StatusDelivered)
	}
	if tracking.TrackingNumber != "1491110022334" {
		t.Errorf("awb = %q", tracking.TrackingNumber)
	}
	if len(tracking.Scans) != 3 {
		t.Errorf("scans = %d, want 3", len(tracking.Scans))
	}
	if tracking.DeliveredAt == nil {
		t.Error("deliveredAt was not parsed")
	}
	if tracking.LastEventAt == nil {
		t.Error("lastEventAt was not derived from the scan list")
	}
}

func TestTrackPrefersShipmentIDOverAWB(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/track/shipment/15151515", 200, fixture(t, "tracking_success.json"))
	m.on(http.MethodGet, "/courier/track/awb/1491110022334", 200, fixture(t, "tracking_success.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.Track(context.Background(), shipping.ShipmentRef{
		ProviderShipmentID: "15151515", TrackingNumber: "1491110022334",
	}); err != nil {
		t.Fatal(err)
	}
	if m.callCount(http.MethodGet, "/courier/track/awb/1491110022334") != 0 {
		t.Fatal("AWB endpoint was used while a shipment id was available")
	}
}

func TestTrackFallsBackToAWB(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/track/awb/1491110022334", 200, fixture(t, "tracking_success.json"))
	p := newTestProvider(t, m, Config{})

	if _, err := p.Track(context.Background(), shipping.ShipmentRef{TrackingNumber: "1491110022334"}); err != nil {
		t.Fatal(err)
	}
}

func TestTrackWithNoDataIsClassified(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/track/awb/nope", 200, fixture(t, "tracking_empty.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.Track(context.Background(), shipping.ShipmentRef{TrackingNumber: "nope"})
	if shipping.CodeOf(err) != shipping.CodeTrackingUnavailable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeTrackingUnavailable)
	}
}

func TestTrackRequiresAnIdentifier(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})

	if _, err := p.Track(context.Background(), shipping.ShipmentRef{}); err == nil {
		t.Fatal("expected an error with nothing to track")
	}
}

// ------------------------------------------------------------------ cancel

func TestCancelAcceptsNoContent(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// 204 with no body is the documented success.
	m.on(http.MethodPost, "/orders/cancel", http.StatusNoContent, nil)
	p := newTestProvider(t, m, Config{})

	if err := p.Cancel(context.Background(), shipping.ShipmentRef{ProviderOrderID: "16161616"}); err != nil {
		t.Fatalf("204 must be treated as success, got %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(m.lastBody(http.MethodPost, "/orders/cancel")), &payload); err != nil {
		t.Fatal(err)
	}
	ids, _ := payload["ids"].([]any)
	if len(ids) != 1 || ids[0] != float64(16161616) {
		t.Fatalf("ids = %v, want [16161616]", payload["ids"])
	}
}

func TestCancelTreatsAlreadyCancelledAsSuccess(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/orders/cancel", 400, []byte(`{"message":"Order is already cancelled"}`))
	p := newTestProvider(t, m, Config{})

	if err := p.Cancel(context.Background(), shipping.ShipmentRef{ProviderOrderID: "16161616"}); err != nil {
		t.Fatalf("an already-cancelled order is the desired end state: %v", err)
	}
}

func TestCancelRequiresNumericOrderID(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})

	if err := p.Cancel(context.Background(), shipping.ShipmentRef{ProviderOrderID: "MAK-20240701-001"}); err == nil {
		t.Fatal("expected an error: a MAK order number is not a shiprocket order id")
	}
	if m.callCount(http.MethodPost, "/orders/cancel") != 0 {
		t.Fatal("carrier was called with a non-numeric id")
	}
}

// ------------------------------------------------------------------ label

func TestLabelIsDownloadedAndReturnedAsBytes(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	pdf := []byte("%PDF-1.4 fake label bytes")
	m.on(http.MethodGet, "/labels/15151515.pdf", 200, pdf).headers = map[string]string{
		"Content-Type": "application/pdf",
	}
	// Point the fixture's label_url at the mock so the download is exercised.
	labelBody := strings.ReplaceAll(
		string(fixture(t, "label_success.json")),
		"https://s3-shiprocket.example",
		m.srv.URL,
	)
	m.on(http.MethodPost, "/courier/generate/label", 200, []byte(labelBody))
	p := newTestProvider(t, m, Config{})

	label, err := p.Label(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if err != nil {
		t.Fatal(err)
	}
	if string(label.Data) != string(pdf) {
		t.Errorf("label bytes = %q", string(label.Data))
	}
	if label.ContentType != "application/pdf" {
		t.Errorf("contentType = %q", label.ContentType)
	}
	// The carrier URL must not travel with the document.
	if strings.Contains(label.Filename, "http") {
		t.Error("filename leaked a provider URL")
	}
}

func TestLabelNotCreatedIsClassified(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/courier/generate/label", 200, fixture(t, "label_failure.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.Label(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if shipping.CodeOf(err) != shipping.CodeLabelUnavailable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeLabelUnavailable)
	}
}

func TestLabelRefusesPlainHTTPURL(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/courier/generate/label", 200,
		[]byte(`{"label_created":1,"label_url":"http://insecure.example/label.pdf"}`))
	p := newTestProvider(t, m, Config{})

	_, err := p.Label(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if err == nil {
		t.Fatal("a label served over plain http must be refused")
	}
}

// ------------------------------------------------------------------ pickup

func TestSchedulePickupSucceeds(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/courier/generate/pickup", 200, fixture(t, "pickup_success.json"))
	p := newTestProvider(t, m, Config{})

	pickup, err := p.SchedulePickup(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if err != nil {
		t.Fatal(err)
	}
	if pickup.Token != "PTN9988776655" {
		t.Errorf("token = %q", pickup.Token)
	}
	if pickup.AlreadyScheduled {
		t.Error("a fresh pickup should not report alreadyScheduled")
	}
	if pickup.ScheduledAt == nil {
		t.Error("scheduledAt was not parsed")
	}
}

func TestDuplicatePickupIsReportedAsSuccess(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// Shiprocket reports an existing pickup as an error; for us it is the
	// desired end state, so an admin double-click must not surface a failure.
	m.on(http.MethodPost, "/courier/generate/pickup", 400, fixture(t, "pickup_duplicate.json"))
	p := newTestProvider(t, m, Config{})

	pickup, err := p.SchedulePickup(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if err != nil {
		t.Fatalf("a duplicate pickup must not be an error: %v", err)
	}
	if !pickup.AlreadyScheduled {
		t.Error("alreadyScheduled should be set")
	}
}

func TestPickupServerErrorIsAnAvailabilityFailure(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodPost, "/courier/generate/pickup", 500, fixture(t, "error_500.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.SchedulePickup(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	// A 5xx is the carrier being unavailable, not our request being wrong --
	// the operator should retry rather than go looking for a bad pickup.
	if shipping.CodeOf(err) != shipping.CodeShippingUnavailable {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeShippingUnavailable)
	}
}

func TestPickupRejectionIsClassifiedAsPickupFailed(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	// A 422 is the carrier refusing this specific pickup.
	m.on(http.MethodPost, "/courier/generate/pickup", 422,
		[]byte(`{"message":"Shipment is not ready to be picked up"}`))
	p := newTestProvider(t, m, Config{})

	_, err := p.SchedulePickup(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "15151515"})
	if shipping.CodeOf(err) != shipping.CodePickupFailed {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodePickupFailed)
	}
}

func TestPickupRequiresNumericShipmentID(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	p := newTestProvider(t, m, Config{})

	if _, err := p.SchedulePickup(context.Background(), shipping.ShipmentRef{ProviderShipmentID: "abc"}); err == nil {
		t.Fatal("expected an error for a non-numeric shipment id")
	}
	if m.callCount(http.MethodPost, "/courier/generate/pickup") != 0 {
		t.Fatal("carrier was called with a non-numeric shipment id")
	}
}

func TestPickupLocationsAreListed(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/settings/company/pickup", 200, fixture(t, "pickup_locations.json"))
	p := newTestProvider(t, m, Config{})

	locations, err := p.PickupLocations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 2 {
		t.Fatalf("locations = %d, want 2", len(locations))
	}
	if locations[0].Name != "Shree Ganesh Watch" {
		t.Errorf("name = %q", locations[0].Name)
	}
	if locations[0].Pincode != "360370" {
		t.Errorf("pincode = %q", locations[0].Pincode)
	}
}

// ------------------------------------------------------------------ retries

func TestRateLimitIsRetriedThenSucceeds(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	r := m.on(http.MethodGet, "/courier/serviceability/", 200, nil)
	r.statuses = []int{429, 200}
	r.bodies = [][]byte{fixture(t, "error_429.json"), fixture(t, "serviceability_success.json")}
	p := newTestProvider(t, m, Config{})

	if _, err := p.Rates(context.Background(), baseRateRequest()); err != nil {
		t.Fatalf("a retried read should succeed: %v", err)
	}
	if got := m.callCount(http.MethodGet, "/courier/serviceability/"); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestPersistentRateLimitIsClassified(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 429, fixture(t, "error_429.json"))
	p := newTestProvider(t, m, Config{})

	_, err := p.Rates(context.Background(), baseRateRequest())
	if err == nil {
		t.Fatal("expected failure")
	}
	// Retries are bounded.
	if got := m.callCount(http.MethodGet, "/courier/serviceability/"); got != maxAttempts {
		t.Fatalf("attempts = %d, want %d", got, maxAttempts)
	}
}

func TestRetryAfterHeaderIsHonoured(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	r := m.on(http.MethodGet, "/courier/serviceability/", 200, nil)
	r.statuses = []int{429, 200}
	r.bodies = [][]byte{fixture(t, "error_429.json"), fixture(t, "serviceability_success.json")}
	r.headers = map[string]string{"Retry-After": "2"}
	p := newTestProvider(t, m, Config{})

	var slept []time.Duration
	p.tr.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if _, err := p.Rates(context.Background(), baseRateRequest()); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("backoff = %v, want a single 2s wait from Retry-After", slept)
	}
}

func TestRetryAfterIsClamped(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "100000")
	d, ok := retryAfter(h)
	if !ok {
		t.Fatal("expected the header to parse")
	}
	if d != maxRetryAfter {
		t.Fatalf("delay = %v, want it clamped to %v", d, maxRetryAfter)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	m := newMockShiprocket(t)
	mockAuth(m, t)
	m.on(http.MethodGet, "/courier/serviceability/", 500, fixture(t, "error_500.json"))
	p := newTestProvider(t, m, Config{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Rates(ctx, baseRateRequest()); err == nil {
		t.Fatal("expected the cancelled context to fail the call")
	}
}

// ------------------------------------------------------------------ webhook

func webhookProvider(t *testing.T, secret string) *Provider {
	t.Helper()
	m := newMockShiprocket(t)
	return newTestProvider(t, m, Config{WebhookSecret: secret})
}

func TestWebhookAcceptsValidKey(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	event, err := p.ParseWebhook(
		map[string]string{"X-Api-Key": "shhh-secret"},
		fixture(t, "webhook_valid.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if event.Provider != shipping.ProviderShiprocket {
		t.Errorf("provider = %q", event.Provider)
	}
	if event.TrackingNumber != "1491110022334" {
		t.Errorf("awb = %q", event.TrackingNumber)
	}
	// sr_order_id (the carrier's) and channel_order_id (ours) are different
	// namespaces and must not be conflated.
	if event.ProviderOrderID != "16161616" {
		t.Errorf("providerOrderID = %q, want the sr_order_id", event.ProviderOrderID)
	}
	if event.OrderRef != "MAK-20240701-001" {
		t.Errorf("orderRef = %q, want the channel_order_id", event.OrderRef)
	}
	if event.Status != shipping.StatusDelivered {
		t.Errorf("status = %q", event.Status)
	}
	if event.DeliveredAt == nil {
		t.Error("deliveredAt should be set for a delivery event")
	}
	if event.EventKey == "" {
		t.Error("eventKey is required for duplicate suppression")
	}
}

func TestWebhookRejectsMissingKey(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	if _, err := p.ParseWebhook(map[string]string{}, fixture(t, "webhook_valid.json")); err == nil {
		t.Fatal("a callback with no x-api-key must be rejected")
	}
}

func TestWebhookRejectsWrongKey(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	if _, err := p.ParseWebhook(
		map[string]string{"x-api-key": "wrong"},
		fixture(t, "webhook_valid.json"),
	); err == nil {
		t.Fatal("a callback with the wrong key must be rejected")
	}
}

func TestWebhookRejectsWhenNoSecretConfigured(t *testing.T) {
	p := webhookProvider(t, "")

	// Fail closed: with no configured secret there is no way to tell a real
	// callback from a forged one, so nothing may be trusted.
	if _, err := p.ParseWebhook(
		map[string]string{"x-api-key": "anything"},
		fixture(t, "webhook_valid.json"),
	); err == nil {
		t.Fatal("with no secret configured the callback must be refused")
	}
}

func TestWebhookRejectsMalformedBody(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	if _, err := p.ParseWebhook(
		map[string]string{"x-api-key": "shhh-secret"},
		[]byte(`not json at all`),
	); err == nil {
		t.Fatal("a malformed body must be rejected")
	}
}

func TestWebhookRejectsEmptyBody(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	if _, err := p.ParseWebhook(map[string]string{"x-api-key": "shhh-secret"}, nil); err == nil {
		t.Fatal("an empty body must be rejected")
	}
}

func TestWebhookRejectsPayloadWithoutIdentifier(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	if _, err := p.ParseWebhook(
		map[string]string{"x-api-key": "shhh-secret"},
		fixture(t, "webhook_invalid.json"),
	); err == nil {
		t.Fatal("a payload with no shipment identifier must be rejected")
	}
}

func TestWebhookKeyLookupIsCaseInsensitive(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	for _, header := range []string{"x-api-key", "X-Api-Key", "X-API-KEY"} {
		if _, err := p.ParseWebhook(
			map[string]string{header: "shhh-secret"},
			fixture(t, "webhook_valid.json"),
		); err != nil {
			t.Errorf("header %q was not accepted: %v", header, err)
		}
	}
}

func TestWebhookEventKeysDifferPerEvent(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")
	headers := map[string]string{"x-api-key": "shhh-secret"}

	delivered, err := p.ParseWebhook(headers, fixture(t, "webhook_valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	inTransit, err := p.ParseWebhook(headers, fixture(t, "webhook_in_transit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if delivered.EventKey == inTransit.EventKey {
		t.Fatal("two different events must not share an event key")
	}

	// The same event delivered twice must produce the same key, so a replay is
	// recognizable.
	again, err := p.ParseWebhook(headers, fixture(t, "webhook_valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if again.EventKey != delivered.EventKey {
		t.Fatal("a redelivered event must produce a stable key")
	}
}

// ------------------------------------------------------------------ status mapping

func TestStatusMappingByCode(t *testing.T) {
	cases := map[int]string{
		1:  shipping.StatusProcessing,
		5:  shipping.StatusCancelled,
		6:  shipping.StatusInTransit,
		7:  shipping.StatusDelivered,
		17: shipping.StatusOutForDelivery,
		18: shipping.StatusInTransit,
		21: shipping.StatusUndelivered,
		42: shipping.StatusPickedUp,
		9:  shipping.StatusReturned,
	}
	for code, want := range cases {
		if got := mapStatus(code, ""); got != want {
			t.Errorf("mapStatus(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestStatusMappingByLabelDistinguishesUndelivered(t *testing.T) {
	// "undelivered" contains "delivered": matching the wrong one would report
	// a failed delivery attempt as a completed delivery.
	if got := mapStatus(0, "UNDELIVERED"); got != shipping.StatusUndelivered {
		t.Errorf("UNDELIVERED mapped to %q", got)
	}
	if got := mapStatus(0, "Delivered"); got != shipping.StatusDelivered {
		t.Errorf("Delivered mapped to %q", got)
	}
	if got := mapStatus(0, "Out For Delivery"); got != shipping.StatusOutForDelivery {
		t.Errorf("Out For Delivery mapped to %q", got)
	}
}

func TestUnknownStatusFallsBackWithoutRegressing(t *testing.T) {
	got := mapStatus(9999, "Some Novel Carrier State")
	if got != shipping.StatusProcessing {
		t.Fatalf("unknown status mapped to %q, want %q", got, shipping.StatusProcessing)
	}
	// And the guard must then refuse to walk an in-transit order backwards.
	if shipping.CanAdvance(shipping.StatusInTransit, got) {
		t.Fatal("an unknown status must not be allowed to regress a shipment")
	}
}

// ------------------------------------------------------------------ scalars

func TestFlexScalarsTolerateProviderInconsistency(t *testing.T) {
	var payload struct {
		A flexFloat  `json:"a"`
		B flexFloat  `json:"b"`
		C flexFloat  `json:"c"`
		D flexInt    `json:"d"`
		E flexInt    `json:"e"`
		F flexString `json:"f"`
		G flexString `json:"g"`
	}
	body := []byte(`{"a": 54, "b": "80.00", "c": null, "d": "1", "e": true, "f": 51, "g": null}`)
	if err := decode(body, &payload); err != nil {
		t.Fatalf("mixed scalar shapes must decode: %v", err)
	}
	if payload.A.Float() != 54 || payload.B.Float() != 80 || payload.C.Float() != 0 {
		t.Errorf("floats = %v %v %v", payload.A, payload.B, payload.C)
	}
	if payload.D.Int() != 1 || !payload.E.Bool() {
		t.Errorf("ints = %v %v", payload.D, payload.E)
	}
	if payload.F.String() != "51" || payload.G.String() != "" {
		t.Errorf("strings = %q %q", payload.F, payload.G)
	}
}

func TestFlexTimeAcceptsBothShapes(t *testing.T) {
	var payload struct {
		Plain  flexTime `json:"plain"`
		Object flexTime `json:"object"`
		Empty  flexTime `json:"empty"`
	}
	body := []byte(`{
	  "plain": "2024-07-01 12:30:00",
	  "object": {"date": "2024-07-02 09:00:00.000000", "timezone_type": 3, "timezone": "Asia/Kolkata"},
	  "empty": null
	}`)
	if err := decode(body, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Plain.Valid || payload.Plain.Time.Hour() != 12 {
		t.Errorf("plain = %v", payload.Plain)
	}
	if !payload.Object.Valid || payload.Object.Time.Day() != 2 {
		t.Errorf("object = %v", payload.Object)
	}
	if payload.Empty.Valid {
		t.Error("null must not produce a valid time")
	}
}

func TestLeadingIntParsesCarrierDayStrings(t *testing.T) {
	cases := map[string]int{"4": 4, "3-5 days": 3, "": 0, "N/A": 0, "2 to 4": 2}
	for input, want := range cases {
		if got := leadingInt(input); got != want {
			t.Errorf("leadingInt(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestSafeDetailTruncatesAndFlattens(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := safeDetail(422, []byte("line1\nline2"+long))
	if strings.Contains(got, "\n") {
		t.Error("detail must be single-line for logs")
	}
	if len(got) > 700 {
		t.Errorf("detail was not truncated: %d chars", len(got))
	}
}

// TestWebhookParsesOfficialSamplePayload runs the payload shape Shiprocket
// documents, which differs from the one this integration was first written
// against: `awb` arrives as a JSON *number*, `order_id` is a string, and
// neither `sr_order_id` nor `shipment_id` is present at all. A parser that
// assumed those fields would resolve nothing.
func TestWebhookParsesOfficialSamplePayload(t *testing.T) {
	p := webhookProvider(t, "shhh-secret")

	event, err := p.ParseWebhook(
		map[string]string{"x-api-key": "shhh-secret"},
		fixture(t, "webhook_official_sample.json"),
	)
	if err != nil {
		t.Fatalf("the documented payload must parse: %v", err)
	}

	// A numeric AWB must survive as its digits, not as scientific notation or
	// an empty string.
	if event.TrackingNumber != "59629792084" {
		t.Errorf("awb = %q, want \"59629792084\"", event.TrackingNumber)
	}
	if event.Status != shipping.StatusDelivered {
		t.Errorf("status = %q, want %q", event.Status, shipping.StatusDelivered)
	}
	if event.DeliveredAt == nil {
		t.Error("deliveredAt should be set")
	}
	if event.OccurredAt.Year() != 2021 {
		t.Errorf("occurredAt = %v, want the 2021 current_timestamp", event.OccurredAt)
	}
	if event.CourierName != "Delhivery Surface" {
		t.Errorf("courierName = %q", event.CourierName)
	}
	if len(event.Scans) != 3 {
		t.Errorf("scans = %d, want 3", len(event.Scans))
	}
	// channel_order_id is the merchant reference we sent; order_id in this
	// payload is Shiprocket's own. Confusing the two is how a callback ends up
	// addressing the wrong order.
	if event.OrderRef != "MAK-20210702-001" {
		t.Errorf("orderRef = %q, want the channel_order_id (our own number)", event.OrderRef)
	}
	if event.ProviderOrderID != "13905312" {
		t.Errorf("providerOrderID = %q, want the shiprocket order_id", event.ProviderOrderID)
	}
	if event.EventKey == "" {
		t.Error("eventKey is required for duplicate suppression")
	}
}

// TestModeLabelNeverShowsANumericCode: Shiprocket's `mode` is a code, not a
// label. Returning it verbatim put a bare "1" on the checkout courier card
// beside the price, which means nothing to a customer.
func TestModeLabelNeverShowsANumericCode(t *testing.T) {
	numeric := courierCompany{}
	if err := decode([]byte(`{"mode":1,"is_surface":true}`), &numeric); err != nil {
		t.Fatal(err)
	}
	if got := modeLabel(numeric); got != "Surface" {
		t.Errorf("modeLabel with a numeric mode = %q, want the is_surface label", got)
	}

	numericAir := courierCompany{}
	if err := decode([]byte(`{"mode":2,"is_surface":false}`), &numericAir); err != nil {
		t.Fatal(err)
	}
	if got := modeLabel(numericAir); got != "" {
		t.Errorf("modeLabel = %q, want empty rather than a meaningless code", got)
	}

	// A carrier that does send words is still honoured.
	worded := courierCompany{}
	if err := decode([]byte(`{"mode":"Air","is_surface":false}`), &worded); err != nil {
		t.Fatal(err)
	}
	if got := modeLabel(worded); got != "Air" {
		t.Errorf("modeLabel = %q, want \"Air\"", got)
	}
}
