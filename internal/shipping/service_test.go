package shipping

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// testService builds a service with no database.
//
// Every method exercised here is pure, so a nil handle is correct rather than a
// shortcut: it also proves these paths cannot quietly reach Mongo.
func testService() *Service {
	return NewService(nil, ServiceConfig{
		Primary:       ProviderShiprocket,
		PickupPincode: "360370",
		PickupLocations: map[string]string{
			ProviderShiprocket: "Shree Ganesh Watch",
			ProviderDelhivery:  "Shree Ganesh Watch",
		},
		Package: PackageSpec{
			WeightGrams: 500, LengthCm: 15, BreadthCm: 10, HeightCm: 8, Defaults: true,
		},
	}, NewQuoter("secret"))
}

func codOrder() *models.Order {
	return &models.Order{
		ID:          primitive.NewObjectID(),
		OrderNumber: "MAK-20240701-001",
		UserID:      primitive.NewObjectID(),
		Total:       4999,
		Status:      "processing",
		// The vocabulary the orders collection already uses.
		PaymentStatus: "unpaid",
		PaymentInfo:   models.PaymentInfo{Method: "cod"},
		ShippingAddress: models.Address{
			Name: "A Customer", Street: "12 Test Road", City: "Mumbai",
			State: "Maharashtra", ZipCode: "400001", Country: "India",
			Phone: "9999999999",
		},
		Items: []models.OrderItem{
			{ProductID: primitive.NewObjectID(), ProductName: "Classic Watch", Price: 4999, Quantity: 1, Subtotal: 4999},
		},
		CreatedAt: time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC),
	}
}

// ------------------------------------------------------------------ the COD invariant

// TestCarrierEventNeverTouchesPaymentStatus is the regression test for the
// audit's most serious finding.
//
// The old webhook set payment_status="paid" whenever a COD order reported
// delivered -- on an endpoint that required no authentication at all. Anyone
// who knew a waybill could settle an invoice against which no money had been
// collected.
func TestCarrierEventNeverTouchesPaymentStatus(t *testing.T) {
	sh := &models.Shipment{
		ID:             primitive.NewObjectID(),
		OrderID:        primitive.NewObjectID(),
		Provider:       ProviderShiprocket,
		Status:         StatusOutForDelivery,
		TrackingNumber: "1491110022334",
	}
	delivered := time.Date(2024, 7, 5, 15, 42, 0, 0, time.UTC)

	// Every status a carrier can report, including the one that used to settle
	// payment.
	for _, status := range []string{
		StatusDelivered, StatusPickedUp, StatusInTransit, StatusOutForDelivery,
		StatusReturned, StatusCancelled, StatusUndelivered, StatusFailed,
	} {
		_, orderSet := buildStatusUpdate(sh, statusUpdate{
			Status:      status,
			OccurredAt:  delivered,
			DeliveredAt: &delivered,
		}, time.Now())

		for key := range orderSet {
			if strings.Contains(key, "payment") {
				t.Fatalf("status %q produced a payment field: %q", status, key)
			}
		}
	}
}

func TestDeliveredEventSetsFulfillmentFields(t *testing.T) {
	sh := &models.Shipment{
		ID:             primitive.NewObjectID(),
		OrderID:        primitive.NewObjectID(),
		Provider:       ProviderShiprocket,
		Status:         StatusOutForDelivery,
		TrackingNumber: "1491110022334",
	}
	at := time.Date(2024, 7, 5, 15, 42, 0, 0, time.UTC)

	shipmentSet, orderSet := buildStatusUpdate(sh, statusUpdate{
		Status:       StatusDelivered,
		StatusDetail: "Delivered",
		Location:     "Jetpur",
		OccurredAt:   at,
		DeliveredAt:  &at,
		EventKey:     "abc123",
	}, time.Now())

	if shipmentSet["status"] != StatusDelivered {
		t.Errorf("shipment status = %v", shipmentSet["status"])
	}
	if orderSet["status"] != "delivered" {
		t.Errorf("order status = %v, want delivered", orderSet["status"])
	}
	if orderSet["shipping_info.shipment_status"] != StatusDelivered {
		t.Errorf("shipping_info.shipment_status = %v", orderSet["shipping_info.shipment_status"])
	}
	if orderSet["shipping_info.delivered_at"] != at {
		t.Errorf("delivered_at = %v", orderSet["shipping_info.delivered_at"])
	}
	if orderSet["shipping_info.current_location"] != "Jetpur" {
		t.Errorf("current_location = %v", orderSet["shipping_info.current_location"])
	}
	if shipmentSet["last_event_key"] != "abc123" {
		t.Errorf("last_event_key = %v", shipmentSet["last_event_key"])
	}
}

// TestStatusUpdateWritesBothWaybillFields guards backward compatibility: every
// existing reader (storefront, admin, the old track-by-waybill route) looks at
// `waybill`, so a new shipment must populate it as well as tracking_number.
func TestStatusUpdateWritesBothWaybillFields(t *testing.T) {
	sh := &models.Shipment{
		ID:             primitive.NewObjectID(),
		OrderID:        primitive.NewObjectID(),
		Provider:       ProviderShiprocket,
		TrackingNumber: "1491110022334",
	}

	_, orderSet := buildStatusUpdate(sh, statusUpdate{Status: StatusInTransit}, time.Now())
	if orderSet["shipping_info.waybill"] != "1491110022334" {
		t.Error("legacy waybill field was not written")
	}
	if orderSet["shipping_info.tracking_number"] != "1491110022334" {
		t.Error("tracking_number was not written")
	}
}

func TestStatusUpdateOmitsStatusWhenSuppressed(t *testing.T) {
	sh := &models.Shipment{
		ID:       primitive.NewObjectID(),
		OrderID:  primitive.NewObjectID(),
		Provider: ProviderShiprocket,
		Status:   StatusDelivered,
	}
	// ApplyWebhookEvent blanks Status for a backward transition; the rest of
	// the event must still be recorded.
	shipmentSet, orderSet := buildStatusUpdate(sh, statusUpdate{
		Status:   "",
		Location: "Ahmedabad",
	}, time.Now())

	if _, present := shipmentSet["status"]; present {
		t.Error("a suppressed status must not be written to the shipment")
	}
	if _, present := orderSet["status"]; present {
		t.Error("a suppressed status must not be written to the order")
	}
	if orderSet["shipping_info.current_location"] != "Ahmedabad" {
		t.Error("the rest of the event should still be recorded")
	}
}

// ------------------------------------------------------------------ order mapping

// TestBuildCreateRequestUsesOrderNumberAsCarrierReference pins the behaviour
// the two divergent implementations disagreed on: the checkout path used the
// order number, the admin retry path used the raw ObjectID, so the same order
// produced different carrier references depending on which booked it.
func TestBuildCreateRequestUsesOrderNumberAsCarrierReference(t *testing.T) {
	s := testService()
	order := codOrder()

	req := s.buildCreateRequest(order, "Shree Ganesh Watch", s.cfg.Package, 54, "51")

	if req.OrderRef != "MAK-20240701-001" {
		t.Errorf("orderRef = %q, want the human-readable order number", req.OrderRef)
	}
}

func TestBuildCreateRequestFallsBackToObjectID(t *testing.T) {
	s := testService()
	order := codOrder()
	order.OrderNumber = ""

	req := s.buildCreateRequest(order, "Primary", s.cfg.Package, 0, "")
	if req.OrderRef != order.ID.Hex() {
		t.Errorf("orderRef = %q, want the object id fallback", req.OrderRef)
	}
}

func TestBuildCreateRequestDefaultsCountry(t *testing.T) {
	s := testService()
	order := codOrder()
	order.ShippingAddress.Country = ""

	req := s.buildCreateRequest(order, "Primary", s.cfg.Package, 0, "")
	if req.Billing.Country != "India" {
		t.Errorf("country = %q, want India", req.Billing.Country)
	}
}

func TestBuildCreateRequestSplitsShippingChargeOutOfTotal(t *testing.T) {
	s := testService()
	order := codOrder() // total 4999

	req := s.buildCreateRequest(order, "Primary", s.cfg.Package, 54, "51")
	// sub_total + shipping_charges must reconstruct the order total, or the
	// carrier's declared value double-counts the freight.
	if got := req.SubTotal + req.ShippingCharge; got != 4999 {
		t.Errorf("subTotal+charge = %v, want 4999", got)
	}
	if req.SubTotal != 4945 {
		t.Errorf("subTotal = %v, want 4945", req.SubTotal)
	}
}

func TestBuildCreateRequestSetsCODFromPaymentMethod(t *testing.T) {
	s := testService()

	cod := s.buildCreateRequest(codOrder(), "Primary", s.cfg.Package, 0, "")
	if !cod.COD {
		t.Error("a cod order must be marked COD")
	}
	if cod.CODAmount != 4999 {
		t.Errorf("codAmount = %v, want the order total", cod.CODAmount)
	}

	prepaidOrder := codOrder()
	prepaidOrder.PaymentInfo.Method = "razorpay"
	prepaid := s.buildCreateRequest(prepaidOrder, "Primary", s.cfg.Package, 0, "")
	if prepaid.COD {
		t.Error("a razorpay order must not be marked COD")
	}
	if prepaid.CODAmount != 0 {
		t.Errorf("codAmount = %v, want 0 for a prepaid order", prepaid.CODAmount)
	}
}

func TestBuildCreateRequestDoesNotInventSKUOrHSN(t *testing.T) {
	s := testService()
	order := codOrder()

	req := s.buildCreateRequest(order, "Primary", s.cfg.Package, 0, "")
	if len(req.Lines) != 1 {
		t.Fatalf("lines = %d", len(req.Lines))
	}
	line := req.Lines[0]
	// The product id is the only stable per-line identifier the order has.
	if line.SKU != order.Items[0].ProductID.Hex() {
		t.Errorf("sku = %q, want the product id", line.SKU)
	}
	// The order carries neither, so neither may be fabricated.
	if line.HSN != "" {
		t.Errorf("hsn = %q, want empty", line.HSN)
	}
	if line.Tax != 0 || line.Discount != 0 {
		t.Errorf("tax/discount = %v/%v, want zero", line.Tax, line.Discount)
	}
}

func TestBuildCreateRequestPrefersCustomerContactOverAddress(t *testing.T) {
	s := testService()
	order := codOrder()
	order.CustomerName = "Explicit Name"
	order.CustomerPhone = "8888888888"
	order.CustomerEmail = "explicit@example.test"

	req := s.buildCreateRequest(order, "Primary", s.cfg.Package, 0, "")
	if req.Billing.Name != "Explicit Name" {
		t.Errorf("name = %q", req.Billing.Name)
	}
	if req.Billing.Phone != "8888888888" {
		t.Errorf("phone = %q", req.Billing.Phone)
	}
	if req.Billing.Email != "explicit@example.test" {
		t.Errorf("email = %q", req.Billing.Email)
	}
}

func TestBuildCreateRequestMarksPackageDefaults(t *testing.T) {
	s := testService()

	req := s.buildCreateRequest(codOrder(), "Primary", s.cfg.Package, 0, "")
	if !req.Package.Defaults {
		t.Error("the parcel figures came from configuration and must be flagged as defaults")
	}
	if req.Package.WeightGrams != 500 {
		t.Errorf("weight = %v grams", req.Package.WeightGrams)
	}
}

// ------------------------------------------------------------------ provider registry

func TestProviderResolutionFallsBackToPrimary(t *testing.T) {
	s := testService()
	// No providers registered, so resolution must fail rather than return nil.
	if _, err := s.Provider(""); err == nil {
		t.Fatal("expected an error with no providers registered")
	}
	if _, err := s.Provider("fedex"); err == nil {
		t.Fatal("an unknown provider must not resolve")
	}
}

func TestPrimaryProviderDefaults(t *testing.T) {
	s := NewService(nil, ServiceConfig{}, NewQuoter("x"))
	if s.PrimaryProviderName() != ProviderShiprocket {
		t.Fatalf("primary = %q, want %q", s.PrimaryProviderName(), ProviderShiprocket)
	}
}

// ------------------------------------------------------------------ legacy compatibility

// TestFindShipmentSynthesizesFromLegacyOrder covers a historical Delhivery
// order: it has no document in the shipments collection, and migrating it is
// explicitly out of scope, so the record must be derived from the order's own
// embedded ShippingInfo.
func TestShippingInfoAWBFallsBackToWaybill(t *testing.T) {
	// Exactly the shape written by the original integration.
	legacy := &models.ShippingInfo{
		Provider:       "delhivery",
		Waybill:        "1491110022334",
		ShipmentStatus: "in_transit",
	}
	if got := legacy.AWB(); got != "1491110022334" {
		t.Fatalf("AWB() = %q, want the legacy waybill", got)
	}
	if !legacy.HasShipment() {
		t.Fatal("a legacy order with a waybill has a shipment")
	}
	if got := legacy.ProviderName(); got != "delhivery" {
		t.Fatalf("provider = %q", got)
	}

	// A document so old it has no provider field at all predates Shiprocket
	// by definition.
	older := &models.ShippingInfo{Waybill: "999"}
	if got := older.ProviderName(); got != "delhivery" {
		t.Fatalf("provider = %q, want delhivery for a pre-shiprocket document", got)
	}

	modern := &models.ShippingInfo{
		Provider:       "shiprocket",
		TrackingNumber: "SR123",
		Waybill:        "SR123",
	}
	if got := modern.AWB(); got != "SR123" {
		t.Fatalf("AWB() = %q", got)
	}

	var absent *models.ShippingInfo
	if absent.AWB() != "" || absent.HasShipment() {
		t.Fatal("a nil ShippingInfo must be safe to interrogate")
	}
}

func TestShippingInfoHasShipmentRecognizesShiprocketIdentifiers(t *testing.T) {
	// A Shiprocket order created but not yet AWB-assigned still has a
	// shipment, and must not look empty to the tracking endpoints.
	created := &models.ShippingInfo{
		Provider:           "shiprocket",
		ProviderOrderID:    "16161616",
		ProviderShipmentID: "15151515",
	}
	if !created.HasShipment() {
		t.Fatal("a booked-but-unassigned shipment must count as having a shipment")
	}
	if created.AWB() != "" {
		t.Fatalf("AWB should still be empty, got %q", created.AWB())
	}
}
