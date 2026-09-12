package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
)

// newRoutedApp builds the real route table over a DB client that is never
// usable.
//
// Every assertion here is about *routing and middleware*, not about handler
// output: a route that gets past authentication reaches its handler, panics on
// the nil database, and is recovered into a 500 by the recover middleware
// SetupRoutes installs. So "not 401" is the signal, and no MongoDB is needed.
func newRoutedApp(t *testing.T) *fiber.App {
	t.Helper()

	app := fiber.New()
	// Fields are exported and both are nil: any handler that touches the
	// database panics, which is the intended outcome above.
	db := &database.DBClient{}
	cfg := &config.Config{JWTSecret: "test-secret"}

	SetupRoutes(app, db, cfg)
	return app
}

func statusFor(t *testing.T, app *fiber.App, method, path string) int {
	t.Helper()

	// -1 disables the request timeout: handlers that reach a nil database panic
	// rather than return, and the default deadline would race the recover.
	resp, err := app.Test(httptest.NewRequest(method, path, nil), -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestPublicRoutesAreNotAuthenticated is the regression test for the Phase 1
// routing bug: SetupRoutes built its protected group as
// `app.Group("", middleware.Auth(...))` before registering any routes.
//
// Fiber's Group(prefix, handlers...) registers those handlers as Use middleware
// on the prefix, and "" matches every path -- so every route registered
// afterwards inherited the auth check and the entire public API answered 401,
// health probes and catalog included.
//
// go build, go vet and the handler unit tests all passed while this was broken,
// because none of them exercise the route table. This test does.
func TestPublicRoutesAreNotAuthenticated(t *testing.T) {
	app := newRoutedApp(t)

	publicRoutes := []struct{ method, path string }{
		{fiber.MethodGet, "/health"},
		{fiber.MethodGet, "/welcome"},
		{fiber.MethodGet, "/products"},
		{fiber.MethodGet, "/catalog/products"},
		{fiber.MethodGet, "/catalog/filters"},
		{fiber.MethodGet, "/categories"},
		{fiber.MethodGet, "/home-content"},
		{fiber.MethodGet, "/api/v1/health"},
		{fiber.MethodGet, "/api/v1/catalog/products"},
		{fiber.MethodGet, "/api/v1/collections"},
		{fiber.MethodGet, "/api/v1/search"},
		{fiber.MethodGet, "/shipping/check-pincode/360370"},
	}

	for _, route := range publicRoutes {
		status := statusFor(t, app, route.method, route.path)
		if status == fiber.StatusUnauthorized {
			t.Errorf("%s %s returned 401; this route must be reachable without a token",
				route.method, route.path)
		}
	}
}

// TestHealthEndpointsRespondOK pins the two probes that must work with no
// database at all. They are the endpoints a load balancer calls.
func TestHealthEndpointsRespondOK(t *testing.T) {
	app := newRoutedApp(t)

	for _, path := range []string{"/health", "/welcome", "/api/v1/health"} {
		if status := statusFor(t, app, fiber.MethodGet, path); status != fiber.StatusOK {
			t.Errorf("GET %s: got %d, want %d", path, status, fiber.StatusOK)
		}
	}
}

// TestProtectedRoutesRequireAuthentication is the other half of the contract:
// the per-group auth must actually be attached. A refactor that dropped it
// would otherwise expose customer data, and the test above alone would not
// notice.
func TestProtectedRoutesRequireAuthentication(t *testing.T) {
	app := newRoutedApp(t)

	protectedRoutes := []struct{ method, path string }{
		{fiber.MethodGet, "/me"},
		{fiber.MethodGet, "/cart/000000000000000000000000"},
		{fiber.MethodGet, "/cart"},
		{fiber.MethodPost, "/cart"},
		{fiber.MethodPut, "/cart"},
		{fiber.MethodGet, "/wishlist"},
		{fiber.MethodGet, "/orders/user/000000000000000000000000"},
		{fiber.MethodPost, "/checkout"},
		{fiber.MethodGet, "/account/overview"},
		{fiber.MethodGet, "/account/orders"},
		{fiber.MethodGet, "/profiles/"},
		{fiber.MethodGet, "/addresses"},
		{fiber.MethodGet, "/recommendations"},
		{fiber.MethodPost, "/reviews"},
		{fiber.MethodGet, "/shipping/track/order/abc"},
		{fiber.MethodPost, "/payments/razorpay/order"},
	}

	for _, route := range protectedRoutes {
		status := statusFor(t, app, route.method, route.path)
		if status != fiber.StatusUnauthorized {
			t.Errorf("%s %s returned %d; this route must require authentication",
				route.method, route.path, status)
		}
	}
}

// TestAdminRoutesRequireAuthentication covers the /admin surface the existing
// admin application depends on.
func TestAdminRoutesRequireAuthentication(t *testing.T) {
	app := newRoutedApp(t)

	adminRoutes := []struct{ method, path string }{
		{fiber.MethodGet, "/admin/accounts"},
		{fiber.MethodGet, "/admin/settings"},
		{fiber.MethodGet, "/admin/categories"},
		{fiber.MethodGet, "/admin/home-content/hero-slides"},
		{fiber.MethodGet, "/admin/subscribers"},
		{fiber.MethodGet, "/admin/analytics/summary"},
		{fiber.MethodPost, "/upload"},
	}

	for _, route := range adminRoutes {
		status := statusFor(t, app, route.method, route.path)
		if status != fiber.StatusUnauthorized {
			t.Errorf("%s %s returned %d; this route must require authentication",
				route.method, route.path, status)
		}
	}
}
