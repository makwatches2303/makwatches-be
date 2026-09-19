package handlers

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
)

// Authorization on the dispatch-approval endpoint.
//
// Approving an order books a real parcel with a real carrier and costs real
// money, so who may call it is not something to infer from reading the route
// table. These tests exercise the assembled table with real JWTs over a
// database that is never usable: every assertion here is decided by
// middleware and the handler's own role check, both of which run before any
// database access. A request that gets through panics on the nil client and
// the recover middleware turns that into a 500 -- so "500" means "it was
// allowed through", and 401/403 mean it was refused.

const approvePath = "/admin/orders/507f1f77bcf86cd799439011/approve"

func approvalApp(t *testing.T) *fiber.App {
	t.Helper()
	app := fiber.New()
	SetupRoutes(app, &database.DBClient{}, &config.Config{
		JWTSecret: idorJWTSecret,
		// No carrier credentials. Nothing here reaches a provider.
		ShippingProvider:          "shiprocket",
		PackageDefaultWeightGrams: 500,
		PackageDefaultLengthCm:    15,
		PackageDefaultBreadthCm:   10,
		PackageDefaultHeightCm:    8,
	})
	return app
}

// An anonymous caller must not be able to dispatch anything.
func TestApproveOrderRequiresAuthentication(t *testing.T) {
	app := approvalApp(t)

	if got := statusWithToken(t, app, http.MethodPost, approvePath, ""); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an anonymous caller", got)
	}
}

// A signed-in customer must not be able to dispatch anything either -- not
// their own order, and not anyone else's. This is the check that stops the new
// endpoint from becoming a way for any account holder to book parcels on the
// shop's carrier accounts.
func TestApproveOrderRefusesANonAdmin(t *testing.T) {
	app := approvalApp(t)
	customer := tokenFor(t, primitive.NewObjectID(), "customer")

	got := statusWithToken(t, app, http.MethodPost, approvePath, customer)
	if got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a signed-in non-admin", got)
	}
}

// And the route exists at all: an admin token gets past authorization and
// reaches the handler, which then fails on the unusable database. Without this
// a typo in the path would make the two tests above pass for the wrong reason
// -- an unregistered route also refuses everybody.
func TestApproveOrderIsReachableByAnAdmin(t *testing.T) {
	app := approvalApp(t)
	admin := tokenFor(t, primitive.NewObjectID(), "admin")

	got := statusWithToken(t, app, http.MethodPost, approvePath, admin)
	switch got {
	case http.StatusUnauthorized, http.StatusForbidden:
		t.Fatalf("status = %d: an admin was refused, so the route is mis-wired", got)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		t.Fatalf("status = %d: POST %s is not registered", got, approvePath)
	}
}

// The other two doors into a carrier booking stay admin-only. They are now the
// retry paths for an order that has already been approved, and a regression
// that opened either of them would bypass the approval gate entirely.
func TestShipmentCreationEndpointsStayAdminOnly(t *testing.T) {
	app := approvalApp(t)
	customer := tokenFor(t, primitive.NewObjectID(), "customer")

	paths := []string{
		"/admin/shipping/orders/507f1f77bcf86cd799439011/retry",
		"/api/v1/shipping/orders/507f1f77bcf86cd799439011/create",
	}
	for _, path := range paths {
		if got := statusWithToken(t, app, http.MethodPost, path, ""); got != http.StatusUnauthorized {
			t.Errorf("POST %s anonymously: status = %d, want 401", path, got)
		}
		if got := statusWithToken(t, app, http.MethodPost, path, customer); got != http.StatusForbidden {
			t.Errorf("POST %s as a customer: status = %d, want 403", path, got)
		}
	}
}
