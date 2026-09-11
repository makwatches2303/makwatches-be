package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// These tests prove the ownership checks on the shipping surface with two real
// users and real JWTs, rather than inferring them from the code.
//
// Gated on SHIPPING_TEST_MONGO_URI and deliberately NOT reading MONGO_URI, so
// the suite can never be one environment variable away from touching
// production orders. Each run uses a throwaway database and drops it.

const idorJWTSecret = "idor-test-secret"

func idorDB(t *testing.T) *database.DBClient {
	t.Helper()

	uri := os.Getenv("SHIPPING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set SHIPPING_TEST_MONGO_URI to a disposable MongoDB to run the IDOR tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Atlas caps database names at 38 bytes.
	db := client.Database("idortest_" + primitive.NewObjectID().Hex()[:14])
	t.Cleanup(func() {
		cleanupCtx, cc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cc()
		dropTestDatabase(cleanupCtx, t, db)
		_ = client.Disconnect(cleanupCtx)
	})
	return &database.DBClient{MongoDB: db}
}

// dropTestDatabase removes a throwaway database and confirms it is gone.
//
// A plain Drop is not enough. SetupRoutes creates the shipment indexes during
// app construction, and that write can land just after the drop, leaving the
// database behind with a lone `shipments` collection -- which is how earlier
// runs left debris on a shared cluster. So the drop is verified and retried,
// and a database that still will not go reports itself rather than lingering
// silently.
func dropTestDatabase(ctx context.Context, t *testing.T, db *mongo.Database) {
	t.Helper()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := db.Drop(ctx); err != nil {
			t.Errorf("could not drop the test database %q: %v", db.Name(), err)
			return
		}
		names, err := db.ListCollectionNames(ctx, bson.M{})
		if err != nil {
			// Cannot confirm; the drop itself reported success.
			return
		}
		if len(names) == 0 {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Errorf("test database %q still exists after three drops -- drop it manually", db.Name())
}

// tokenFor mints a JWT in the exact shape middleware.Auth expects.
func tokenFor(t *testing.T, userID primitive.ObjectID, role string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"userId": userID.Hex(),
		"role":   role,
		"exp":    time.Now().Add(time.Hour).Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(idorJWTSecret))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

// idorApp builds the real route table over a throwaway database.
func idorApp(t *testing.T, db *database.DBClient) *fiber.App {
	t.Helper()
	app := fiber.New()
	SetupRoutes(app, db, &config.Config{
		JWTSecret: idorJWTSecret,
		// No carrier credentials: every assertion here is about
		// authorization, which is decided before any carrier call.
		ShippingProvider: "shiprocket",
		// The parcel defaults LoadConfig would supply. They matter because a
		// quote is bound to the parcel weight, so a zero here would make every
		// quote fail verification for the wrong reason.
		PackageDefaultWeightGrams: 500,
		PackageDefaultLengthCm:    15,
		PackageDefaultBreadthCm:   10,
		PackageDefaultHeightCm:    8,
	})
	return app
}

func statusWithToken(t *testing.T, app *fiber.App, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// seedShippedOrder inserts an order owned by userID that already has a
// shipment, so the authorization check is the only thing standing between the
// caller and the data.
func seedShippedOrder(t *testing.T, db *database.DBClient, userID primitive.ObjectID) primitive.ObjectID {
	t.Helper()
	order := models.Order{
		ID:            primitive.NewObjectID(),
		OrderNumber:   "MAK-20240701-001",
		UserID:        userID,
		Total:         4999,
		Status:        "processing",
		PaymentStatus: "paid",
		PaymentInfo:   models.PaymentInfo{Method: "razorpay"},
		ShippingAddress: models.Address{
			Name: "Owner", Street: "1 Test Rd", City: "Mumbai",
			State: "Maharashtra", ZipCode: "400001", Country: "India", Phone: "9999999999",
		},
		Items: []models.OrderItem{
			{ProductID: primitive.NewObjectID(), ProductName: "Watch", Price: 4999, Quantity: 1, Subtotal: 4999},
		},
		ShippingInfo: &models.ShippingInfo{
			Provider:           "shiprocket",
			Waybill:            "1491110022334",
			TrackingNumber:     "1491110022334",
			ProviderShipmentID: "15151515",
			ShipmentStatus:     "in_transit",
		},
		CreatedAt: time.Now(),
	}
	if _, err := db.MongoDB.Collection("orders").InsertOne(context.Background(), order); err != nil {
		t.Fatalf("seeding order: %v", err)
	}
	return order.ID
}

// TestOtherUserCannotReachAnotherCustomersShipment is the IDOR test.
//
// User B holds a perfectly valid session. Every shipping route scoped to User
// A's order must refuse them, and must refuse in a way that does not confirm
// the order exists -- otherwise order ids become enumerable.
func TestOtherUserCannotReachAnotherCustomersShipment(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)

	userA := primitive.NewObjectID()
	userB := primitive.NewObjectID()
	orderID := seedShippedOrder(t, db, userA).Hex()

	tokenB := tokenFor(t, userB, "user")

	scoped := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/shipping/orders/" + orderID + "/tracking"},
		{http.MethodGet, "/api/v1/shipping/orders/" + orderID + "/label"},
		{http.MethodGet, "/shipping/track/order/" + orderID},
	}
	for _, route := range scoped {
		got := statusWithToken(t, app, route.method, route.path, tokenB)
		if got == http.StatusOK {
			t.Errorf("%s %s: User B read User A's shipment (200)", route.method, route.path)
			continue
		}
		if got != http.StatusNotFound && got != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 404 or 403", route.method, route.path, got)
		}
	}

	// A non-existent order must answer the same way as someone else's, or the
	// difference reveals which ids are real.
	ghost := primitive.NewObjectID().Hex()
	for _, route := range scoped {
		othersPath := route.path
		ghostPath := replaceOnce(route.path, orderID, ghost)
		a := statusWithToken(t, app, route.method, othersPath, tokenB)
		b := statusWithToken(t, app, route.method, ghostPath, tokenB)
		if a != b {
			t.Errorf("%s: someone else's order answers %d but a non-existent one answers %d; "+
				"the difference makes order ids enumerable", route.path, a, b)
		}
	}
}

// TestNonAdminCannotRunFulfillmentOperations covers the admin-only surface
// with a genuine non-admin session.
func TestNonAdminCannotRunFulfillmentOperations(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)

	owner := primitive.NewObjectID()
	orderID := seedShippedOrder(t, db, owner).Hex()
	// The owner of the order -- still not an admin.
	ownerToken := tokenFor(t, owner, "user")

	adminOnly := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/create"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/awb"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/cancel"},
		{http.MethodPost, "/api/v1/shipping/orders/" + orderID + "/pickup"},
		{http.MethodGet, "/api/v1/shipping/pickup-locations"},
		{http.MethodPost, "/admin/shipping/orders/" + orderID + "/retry"},
		{http.MethodPost, "/admin/shipping/orders/" + orderID + "/cancel"},
		{http.MethodGet, "/admin/shipping/orders/" + orderID + "/label"},
		{http.MethodPost, "/admin/shipping/bulk-track"},
		{http.MethodPost, "/admin/shipping/request-pickup"},
	}
	for _, route := range adminOnly {
		got := statusWithToken(t, app, route.method, route.path, ownerToken)
		if got != http.StatusForbidden && got != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 403/401 for a non-admin", route.method, route.path, got)
		}
	}
}

// TestOwnerCanReachTheirOwnShipment is the counterpart: the checks must not be
// so strict that the actual customer is locked out.
func TestOwnerCanReachTheirOwnShipment(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)

	owner := primitive.NewObjectID()
	orderID := seedShippedOrder(t, db, owner).Hex()
	ownerToken := tokenFor(t, owner, "user")

	// No carrier credentials are configured, so live tracking cannot succeed;
	// the handler falls back to the stored status. What matters here is that
	// the caller is not refused as unauthorized.
	got := statusWithToken(t, app, http.MethodGet, "/shipping/track/order/"+orderID, ownerToken)
	if got == http.StatusUnauthorized || got == http.StatusForbidden || got == http.StatusNotFound {
		t.Fatalf("the order's owner was refused their own tracking: status = %d", got)
	}
}

// TestAdminCanReachAnyShipment confirms the admin path still works.
func TestAdminCanReachAnyShipment(t *testing.T) {
	db := idorDB(t)
	app := idorApp(t, db)

	owner := primitive.NewObjectID()
	orderID := seedShippedOrder(t, db, owner).Hex()
	adminToken := tokenFor(t, primitive.NewObjectID(), "admin")

	got := statusWithToken(t, app, http.MethodGet, "/shipping/track/order/"+orderID, adminToken)
	if got == http.StatusUnauthorized || got == http.StatusForbidden || got == http.StatusNotFound {
		t.Fatalf("an admin was refused: status = %d", got)
	}
}

// replaceOnce swaps the first occurrence of old with new.
func replaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}
