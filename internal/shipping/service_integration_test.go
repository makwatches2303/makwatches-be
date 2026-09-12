package shipping

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// These tests exercise the parts of ShippingService that are only meaningful
// against a real database: the unique-index claim that makes shipment creation
// idempotent, and the webhook ordering guards.
//
// They are gated on SHIPPING_TEST_MONGO_URI and deliberately do NOT read
// MONGO_URI. The application's MONGO_URI points at the live Atlas cluster, and
// a test suite must never be one environment variable away from writing to
// production orders. Run them against a throwaway instance:
//
//	SHIPPING_TEST_MONGO_URI=mongodb://localhost:27017 go test ./internal/shipping/
//
// Each test uses its own randomly-named database and drops it afterwards.
//
// Running the whole module against a shared cluster needs `go test ./... -p 1`.
// Every test here opens its own client, and Go builds packages in parallel by
// default, which is enough concurrent connections to exhaust a small Atlas tier
// and fail with "server selection timeout" -- an infrastructure limit, not a
// defect in the code under test.

func testDB(t *testing.T) *mongo.Database {
	t.Helper()

	uri := os.Getenv("SHIPPING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set SHIPPING_TEST_MONGO_URI to a disposable MongoDB to run shipping integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("pinging the test database: %v", err)
	}

	// Atlas caps database names at 38 bytes, so the suffix is trimmed rather
	// than using a full 24-character ObjectID hex.
	name := "srtest_" + primitive.NewObjectID().Hex()[:16]
	db := client.Database(name)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		dropVerified(cleanupCtx, t, db)
		_ = client.Disconnect(cleanupCtx)
	})
	return db
}

// dropVerified removes a throwaway database and confirms it is gone.
//
// A single Drop can race an index creation still settling and leave the
// database behind, which is how earlier runs left debris on a shared cluster.
// Verified and retried, and reported if it still will not go.
func dropVerified(ctx context.Context, t *testing.T, db *mongo.Database) {
	t.Helper()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := db.Drop(ctx); err != nil {
			t.Errorf("could not drop the test database %q: %v", db.Name(), err)
			return
		}
		names, err := db.ListCollectionNames(ctx, bson.M{})
		if err != nil || len(names) == 0 {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Errorf("test database %q still exists after three drops -- drop it manually", db.Name())
}

// fakeProvider is a carrier stand-in that counts what the service asked it to
// do, so double-booking is directly observable.
type fakeProvider struct {
	name string

	creates  int32
	awbs     int32
	pickups  int32
	cancels  int32
	tracks   int32
	failNext atomic.Bool

	// delay lets a test hold the carrier call open while a second caller
	// arrives, which is how the concurrency guard is exercised.
	delay time.Duration

	mu       sync.Mutex
	lastReq  CreateShipmentRequest
	awbUnsup bool
}

func newFakeProvider(name string) *fakeProvider { return &fakeProvider{name: name} }

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Rates(context.Context, RateRequest) ([]RateOption, error) {
	return []RateOption{{
		ID: OptionID(f.name, "1"), Provider: f.name, ProviderCourierID: "1",
		CourierName: "Fake Courier", Charge: 54, CODAvailable: true,
	}}, nil
}

func (f *fakeProvider) CreateShipment(_ context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error) {
	n := atomic.AddInt32(&f.creates, 1)
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.failNext.Load() {
		return nil, Errf(CodeShipmentCreationFailed, "fake carrier refused the booking")
	}
	return &CreateShipmentResult{
		ProviderOrderID:    "order-" + itoa(int(n)),
		ProviderShipmentID: "shipment-" + itoa(int(n)),
		Status:             StatusManifested,
	}, nil
}

func (f *fakeProvider) AssignAWB(_ context.Context, req AssignAWBRequest) (*AssignAWBResult, error) {
	if f.awbUnsup {
		return nil, ErrUnsupported
	}
	n := atomic.AddInt32(&f.awbs, 1)
	return &AssignAWBResult{
		TrackingNumber:   "AWB-" + itoa(int(n)),
		CourierCompanyID: "1",
		CourierName:      "Fake Courier",
		AssignedAt:       time.Now(),
	}, nil
}

func (f *fakeProvider) Track(context.Context, ShipmentRef) (*Tracking, error) {
	atomic.AddInt32(&f.tracks, 1)
	return &Tracking{Provider: f.name, Status: StatusInTransit}, nil
}

func (f *fakeProvider) Cancel(context.Context, ShipmentRef) error {
	atomic.AddInt32(&f.cancels, 1)
	return nil
}

func (f *fakeProvider) Label(context.Context, ShipmentRef) (*Label, error) {
	return &Label{ContentType: "application/pdf", Data: []byte("%PDF fake")}, nil
}

func (f *fakeProvider) SchedulePickup(context.Context, ShipmentRef) (*Pickup, error) {
	n := atomic.AddInt32(&f.pickups, 1)
	return &Pickup{Token: "PTN-" + itoa(int(n)), ScheduledDate: "2024-07-02"}, nil
}

func (f *fakeProvider) PickupLocations(context.Context) ([]PickupLocation, error) {
	return []PickupLocation{{Name: "Shree Ganesh Watch"}}, nil
}

func (f *fakeProvider) ParseWebhook(map[string]string, []byte) (*WebhookEvent, error) {
	return nil, Errf(CodeInvalidRequest, "not used in these tests")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// integrationService wires the service over a real database and a fake carrier.
func integrationService(t *testing.T, db *mongo.Database, provider *fakeProvider) *Service {
	t.Helper()
	svc := NewService(db, ServiceConfig{
		Primary:         provider.Name(),
		PickupPincode:   "360370",
		PickupLocations: map[string]string{provider.Name(): "Shree Ganesh Watch"},
		Package:         PackageSpec{WeightGrams: 500, LengthCm: 15, BreadthCm: 10, HeightCm: 8, Defaults: true},
	}, NewQuoter("secret"), provider)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := svc.EnsureIndexes(ctx); err != nil {
		t.Fatalf("creating indexes: %v", err)
	}
	return svc
}

func seedOrder(t *testing.T, db *mongo.Database, order *models.Order) {
	t.Helper()
	if _, err := db.Collection("orders").InsertOne(context.Background(), order); err != nil {
		t.Fatalf("seeding order: %v", err)
	}
}

// ------------------------------------------------------------------ idempotency

// TestConcurrentCreateBooksExactlyOneShipment is the core safety property: the
// checkout goroutine and an admin retry arriving together must not put two
// parcels on the same order.
func TestConcurrentCreateBooksExactlyOneShipment(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	provider.delay = 150 * time.Millisecond // hold the carrier call open
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)

	const callers = 8
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.CreateShipmentForOrder(context.Background(), order, CreateOptions{AssignAWB: true})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&provider.creates); got != 1 {
		t.Fatalf("carrier was asked to create %d shipments; want exactly 1", got)
	}
	count, err := db.Collection("shipments").CountDocuments(context.Background(),
		bson.M{"order_id": order.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("shipment documents = %d, want 1", count)
	}
}

// TestRepeatedCreateReturnsExistingShipment covers the retried browser POST
// and the admin double-click.
func TestRepeatedCreateReturnsExistingShipment(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)
	ctx := context.Background()

	first, err := svc.CreateShipmentForOrder(ctx, order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateShipmentForOrder(ctx, order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&provider.creates); got != 1 {
		t.Fatalf("carrier creates = %d, want 1", got)
	}
	if first.ProviderShipmentID != second.ProviderShipmentID {
		t.Fatalf("second call returned a different shipment: %q vs %q",
			first.ProviderShipmentID, second.ProviderShipmentID)
	}
}

// TestFailedCreateRemainsRetryable: a booking that failed must not permanently
// block the order from ever being shipped.
func TestFailedCreateRemainsRetryable(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)
	ctx := context.Background()

	provider.failNext.Store(true)
	if _, err := svc.CreateShipmentForOrder(ctx, order, CreateOptions{}); err == nil {
		t.Fatal("expected the first booking to fail")
	}

	// The failure is recorded on the order.
	var failed models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&failed); err != nil {
		t.Fatal(err)
	}
	if failed.ShippingInfo == nil || failed.ShippingInfo.ShipmentError == "" {
		t.Fatal("the failure was not recorded on the order")
	}

	// The claim is stale-but-failed, so a retry may take it over.
	provider.failNext.Store(false)
	shipment, err := svc.CreateShipmentForOrder(ctx, order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatalf("retry after a failure should succeed: %v", err)
	}
	if shipment.ProviderShipmentID == "" {
		t.Fatal("retry produced no shipment id")
	}
	// And there is still only one shipment record.
	count, _ := db.Collection("shipments").CountDocuments(ctx, bson.M{"order_id": order.ID})
	if count != 1 {
		t.Fatalf("shipment documents = %d, want 1", count)
	}

	// The success must clear the stale error text.
	var healed models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&healed); err != nil {
		t.Fatal(err)
	}
	if healed.ShippingInfo.ShipmentError != "" {
		t.Errorf("stale error text survived a successful retry: %q", healed.ShippingInfo.ShipmentError)
	}
}

func TestCreateMirrorsBothWaybillFieldsOntoOrder(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)

	shipment, err := svc.CreateShipmentForOrder(context.Background(), order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatal(err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(context.Background(), bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.ShippingInfo.TrackingNumber != shipment.TrackingNumber {
		t.Errorf("tracking_number = %q", stored.ShippingInfo.TrackingNumber)
	}
	// Backward compatibility: every existing reader looks at `waybill`.
	if stored.ShippingInfo.Waybill != shipment.TrackingNumber {
		t.Errorf("legacy waybill = %q, want %q", stored.ShippingInfo.Waybill, shipment.TrackingNumber)
	}
	if stored.ShippingInfo.ProviderShipmentID != shipment.ProviderShipmentID {
		t.Errorf("provider_shipment_id = %q", stored.ShippingInfo.ProviderShipmentID)
	}
}

// TestAWBUnsupportedIsNotAFailure covers Delhivery, which issues the waybill
// during manifestation and has no assignment step.
func TestAWBUnsupportedIsNotAFailure(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	provider.awbUnsup = true
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)

	shipment, err := svc.CreateShipmentForOrder(context.Background(), order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatalf("an unsupported AWB step must not fail the booking: %v", err)
	}
	if shipment.ErrorCode != "" {
		t.Errorf("errorCode = %q, want empty for an unsupported step", shipment.ErrorCode)
	}
}

func TestPickupIsScheduledOnlyOnce(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)
	ctx := context.Background()

	if _, err := svc.CreateShipmentForOrder(ctx, order, CreateOptions{AssignAWB: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := svc.SchedulePickup(ctx, order); err != nil {
			t.Fatalf("pickup %d: %v", i, err)
		}
	}
	// Repeated admin clicks must not queue the parcel four times.
	if got := atomic.LoadInt32(&provider.pickups); got != 1 {
		t.Fatalf("carrier pickups = %d, want 1", got)
	}
}

// ------------------------------------------------------------------ webhooks

func bookedOrder(t *testing.T, db *mongo.Database, svc *Service) (*models.Order, *models.Shipment) {
	t.Helper()
	order := codOrder()
	seedOrder(t, db, order)
	shipment, err := svc.CreateShipmentForOrder(context.Background(), order, CreateOptions{AssignAWB: true})
	if err != nil {
		t.Fatal(err)
	}
	return order, shipment
}

func TestWebhookAdvancesOrderStatus(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		TrackingNumber:     shipment.TrackingNumber,
		Status:             StatusInTransit,
		OccurredAt:         time.Now(),
		EventKey:           "event-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status != "shipped" {
		t.Errorf("order status = %q, want shipped", stored.Status)
	}
	if stored.ShippingInfo.ShipmentStatus != StatusInTransit {
		t.Errorf("shipment status = %q", stored.ShippingInfo.ShipmentStatus)
	}
}

// TestDeliveredWebhookLeavesCODPaymentUnpaid is the security regression test
// for the audit's most serious finding, end to end.
func TestDeliveredWebhookLeavesCODPaymentUnpaid(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	delivered := time.Now()
	err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		TrackingNumber:     shipment.TrackingNumber,
		Status:             StatusDelivered,
		OccurredAt:         delivered,
		DeliveredAt:        &delivered,
		EventKey:           "event-delivered",
	})
	if err != nil {
		t.Fatal(err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	// Fulfillment advanced...
	if stored.Status != "delivered" {
		t.Errorf("order status = %q, want delivered", stored.Status)
	}
	// ...and payment did not. A shipping callback is not a payment event.
	if stored.PaymentStatus != "unpaid" {
		t.Fatalf("payment status = %q; a carrier callback must never settle a COD invoice", stored.PaymentStatus)
	}
}

func TestDuplicateWebhookIsAppliedOnce(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	event := &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		TrackingNumber:     shipment.TrackingNumber,
		Status:             StatusInTransit,
		Location:           "Ahmedabad",
		OccurredAt:         time.Now(),
		EventKey:           "same-key",
	}
	for i := 0; i < 3; i++ {
		if err := svc.ApplyWebhookEvent(ctx, event); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.ShippingInfo.ShipmentStatus != StatusInTransit {
		t.Errorf("status = %q", stored.ShippingInfo.ShipmentStatus)
	}
}

// TestStaleWebhookDoesNotRewindOrder covers an out-of-order delivery: a carrier
// resending an older "in transit" event must not un-deliver the order.
func TestStaleWebhookDoesNotRewindOrder(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	now := time.Now()
	delivered := now
	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		Status:             StatusDelivered,
		OccurredAt:         now,
		DeliveredAt:        &delivered,
		EventKey:           "delivered",
	}); err != nil {
		t.Fatal(err)
	}

	// An older event arriving late.
	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		Status:             StatusInTransit,
		OccurredAt:         now.Add(-2 * time.Hour),
		EventKey:           "in-transit-late",
	}); err != nil {
		t.Fatalf("a stale event should be absorbed, not error: %v", err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status != "delivered" {
		t.Fatalf("order status = %q; a stale event rewound the order", stored.Status)
	}
	if stored.ShippingInfo.ShipmentStatus != StatusDelivered {
		t.Fatalf("shipment status = %q; a stale event rewound the shipment",
			stored.ShippingInfo.ShipmentStatus)
	}
}

// TestBackwardStatusIsRefusedEvenWhenNewer: a *newer* event carrying an earlier
// state must still not walk the order back.
func TestBackwardStatusIsRefusedEvenWhenNewer(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	now := time.Now()
	delivered := now
	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		Status:             StatusDelivered,
		OccurredAt:         now,
		DeliveredAt:        &delivered,
		EventKey:           "delivered",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: shipment.ProviderShipmentID,
		Status:             StatusOutForDelivery,
		Location:           "Jetpur",
		OccurredAt:         now.Add(time.Hour),
		EventKey:           "ofd-after",
	}); err != nil {
		t.Fatal(err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.ShippingInfo.ShipmentStatus != StatusDelivered {
		t.Fatalf("status = %q, want it held at delivered", stored.ShippingInfo.ShipmentStatus)
	}
	// The rest of the event is still recorded.
	if stored.ShippingInfo.CurrentLocation != "Jetpur" {
		t.Errorf("location = %q, want it updated even when the status is held",
			stored.ShippingInfo.CurrentLocation)
	}
}

// TestRTOIsAllowedAfterDelivery: a return genuinely follows a delivery.
func TestRTOIsAllowedAfterDelivery(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	now := time.Now()
	delivered := now
	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider: "fake", ProviderShipmentID: shipment.ProviderShipmentID,
		Status: StatusDelivered, OccurredAt: now, DeliveredAt: &delivered, EventKey: "d",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider: "fake", ProviderShipmentID: shipment.ProviderShipmentID,
		Status: StatusReturned, OccurredAt: now.Add(48 * time.Hour), EventKey: "r",
	}); err != nil {
		t.Fatal(err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status != "returned" {
		t.Fatalf("order status = %q, want returned", stored.Status)
	}
}

// TestWebhookCannotAddressAnotherProvidersShipment: a Shiprocket callback must
// not be able to move a Delhivery shipment that happens to share a number.
func TestWebhookCannotAddressAnotherProvidersShipment(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	_, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           "some-other-carrier",
		ProviderShipmentID: shipment.ProviderShipmentID,
		TrackingNumber:     shipment.TrackingNumber,
		Status:             StatusDelivered,
		OccurredAt:         time.Now(),
		EventKey:           "cross-provider",
	})
	if err == nil {
		t.Fatal("a callback from one provider must not resolve another provider's shipment")
	}
	if CodeOf(err) != CodeShipmentNotFound {
		t.Fatalf("code = %s, want %s", CodeOf(err), CodeShipmentNotFound)
	}
}

// TestWebhookForUnknownShipmentIsRejected: an authenticated callback naming
// something we do not have must not silently succeed.
func TestWebhookForUnknownShipmentIsRejected(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	err := svc.ApplyWebhookEvent(context.Background(), &WebhookEvent{
		Provider:           "fake",
		ProviderShipmentID: "does-not-exist",
		TrackingNumber:     "nope",
		Status:             StatusDelivered,
		OccurredAt:         time.Now(),
		EventKey:           "ghost",
	})
	if CodeOf(err) != CodeShipmentNotFound {
		t.Fatalf("code = %s, want %s", CodeOf(err), CodeShipmentNotFound)
	}
}

// TestWebhookResolvesHistoricalDelhiveryOrder covers an order created by the
// original integration: it has no document in the shipments collection, and
// migrating it is out of scope, so it must still be addressable by its legacy
// waybill alone.
func TestWebhookResolvesHistoricalDelhiveryOrder(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider(ProviderDelhivery)
	svc := integrationService(t, db, provider)
	ctx := context.Background()

	// Exactly the shape the old integration wrote: provider + waybill, and
	// nothing else.
	legacy := codOrder()
	legacy.Status = "processing"
	legacy.ShippingInfo = &models.ShippingInfo{
		Provider:       ProviderDelhivery,
		Waybill:        "1491110022334",
		ShipmentStatus: "manifested",
		TrackingURL:    "https://www.delhivery.com/track/package/1491110022334",
	}
	seedOrder(t, db, legacy)

	if err := svc.ApplyWebhookEvent(ctx, &WebhookEvent{
		Provider:           ProviderDelhivery,
		ProviderShipmentID: "1491110022334",
		TrackingNumber:     "1491110022334",
		Status:             StatusInTransit,
		OccurredAt:         time.Now(),
		EventKey:           "legacy-1",
	}); err != nil {
		t.Fatalf("a historical Delhivery order must remain addressable: %v", err)
	}

	var stored models.Order
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": legacy.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.ShippingInfo.ShipmentStatus != StatusInTransit {
		t.Errorf("status = %q", stored.ShippingInfo.ShipmentStatus)
	}
	// No shipment document was invented for a historical order.
	count, _ := db.Collection("shipments").CountDocuments(ctx, bson.M{"order_id": legacy.ID})
	if count != 0 {
		t.Errorf("shipment documents = %d; historical orders must not be migrated", count)
	}
	// And the original waybill is untouched.
	if stored.ShippingInfo.Waybill != "1491110022334" {
		t.Errorf("the legacy waybill was rewritten to %q", stored.ShippingInfo.Waybill)
	}
}

// TestLegacyOrderStaysTrackableAndCancellable: every downstream operation must
// work for a historical order with no shipment document.
func TestLegacyOrderStaysTrackableAndCancellable(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider(ProviderDelhivery)
	svc := integrationService(t, db, provider)
	ctx := context.Background()

	legacy := codOrder()
	legacy.ShippingInfo = &models.ShippingInfo{
		Provider:       ProviderDelhivery,
		Waybill:        "1491110022334",
		ShipmentStatus: "manifested",
	}
	seedOrder(t, db, legacy)

	found, err := svc.FindShipment(ctx, legacy)
	if err != nil {
		t.Fatalf("FindShipment on a legacy order: %v", err)
	}
	if found.TrackingNumber != "1491110022334" {
		t.Errorf("tracking number = %q", found.TrackingNumber)
	}
	if found.Provider != ProviderDelhivery {
		t.Errorf("provider = %q", found.Provider)
	}
	if !found.ID.IsZero() {
		t.Error("a synthesized record must not claim a shipments _id")
	}

	if _, err := svc.Track(ctx, legacy); err != nil {
		t.Errorf("Track on a legacy order: %v", err)
	}
	if _, err := svc.Label(ctx, legacy); err != nil {
		t.Errorf("Label on a legacy order: %v", err)
	}
	if err := svc.Cancel(ctx, legacy); err != nil {
		t.Errorf("Cancel on a legacy order: %v", err)
	}
	if got := atomic.LoadInt32(&provider.cancels); got != 1 {
		t.Errorf("carrier cancels = %d, want 1", got)
	}
}

func TestCancelIfCancellableRespectsCarrierWindow(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)
	order, shipment := bookedOrder(t, db, svc)
	ctx := context.Background()

	// Once the parcel is in transit, only the carrier's RTO flow applies.
	if _, err := db.Collection("shipments").UpdateOne(ctx,
		bson.M{"_id": shipment.ID},
		bson.M{"$set": bson.M{"status": StatusInTransit}}); err != nil {
		t.Fatal(err)
	}

	cancelled, err := svc.CancelIfCancellable(ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("an in-transit parcel must not be reported as withdrawn")
	}
	if got := atomic.LoadInt32(&provider.cancels); got != 0 {
		t.Fatalf("carrier cancels = %d, want 0", got)
	}
}

func TestCancelIfCancellableIgnoresOrdersWithNoShipment(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	svc := integrationService(t, db, provider)

	order := codOrder()
	seedOrder(t, db, order)

	cancelled, err := svc.CancelIfCancellable(context.Background(), order)
	if err != nil {
		t.Fatalf("an order with no shipment is not an error: %v", err)
	}
	if cancelled {
		t.Fatal("nothing was booked, so nothing can be withdrawn")
	}
}

// TestUniqueIndexExists proves the idempotency mechanism is actually in place
// rather than merely intended.
func TestUniqueIndexExists(t *testing.T) {
	db := testDB(t)
	provider := newFakeProvider("fake")
	_ = integrationService(t, db, provider)

	cursor, err := db.Collection("shipments").Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var indexes []bson.M
	if err := cursor.All(context.Background(), &indexes); err != nil {
		t.Fatal(err)
	}

	for _, idx := range indexes {
		if idx["name"] == "uniq_order_provider" {
			if unique, _ := idx["unique"].(bool); !unique {
				t.Fatal("uniq_order_provider exists but is not unique")
			}
			return
		}
	}
	t.Fatal("the uniq_order_provider index was not created")
}
