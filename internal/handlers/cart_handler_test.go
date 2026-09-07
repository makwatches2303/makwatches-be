package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
)

// newCartAuthzApp mounts GetCart behind a stub auth middleware that injects the
// given token metadata, mirroring how middleware.Auth populates c.Locals("user").
//
// Every case exercised here is rejected by the authorization guard before any
// database access, so the handler's nil DB is never dereferenced. A test that
// starts failing with a nil-pointer panic means the guard stopped running first
// -- which is exactly the regression this file exists to catch.
func newCartAuthzApp(tok *middleware.TokenMetadata) *fiber.App {
	app := fiber.New()
	h := &CartHandler{}

	// Registered first so it actually wraps the route: a request that clears
	// authorization goes on to touch the nil DB, and that panic becomes a 500
	// rather than escaping the test.
	app.Use(recoverToStatus())
	app.Use(func(c *fiber.Ctx) error {
		if tok != nil {
			c.Locals("user", tok)
		}
		return c.Next()
	})
	app.Get("/cart/:userID", h.GetCart)
	app.Get("/cart", h.GetCart)

	return app
}

func requestStatus(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatalf("app.Test(%q): %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var decoded struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &decoded)

	return resp.StatusCode, decoded.Message
}

// TestGetCartRejectsOtherUsersCart is the regression test for the IDOR fixed in
// Phase 1: GET /cart/:userID used to read the id straight out of the path, so
// any authenticated caller could read any other user's cart.
func TestGetCartRejectsOtherUsersCart(t *testing.T) {
	attacker := &middleware.TokenMetadata{
		UserID: primitive.NewObjectID(),
		Role:   "user",
	}
	victimID := primitive.NewObjectID()

	app := newCartAuthzApp(attacker)

	status, msg := requestStatus(t, app, "/cart/"+victimID.Hex())
	if status != fiber.StatusForbidden {
		t.Fatalf("reading another user's cart: got status %d (%q), want %d",
			status, msg, fiber.StatusForbidden)
	}
}

// TestGetCartAllowsAdmin preserves the existing RemoveFromCart rule: an admin
// may address another user's cart explicitly. Reaching the database is the pass
// condition here, so the nil DB panics -- recovered and asserted below.
func TestGetCartAllowsAdmin(t *testing.T) {
	admin := &middleware.TokenMetadata{
		UserID: primitive.NewObjectID(),
		Role:   "admin",
	}
	victimID := primitive.NewObjectID()

	app := newCartAuthzApp(admin)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/cart/"+victimID.Hex(), nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// The guard must not short-circuit an admin. Anything but 403 means the
	// request got past authorization, which is the behaviour under test.
	if resp.StatusCode == fiber.StatusForbidden {
		t.Fatal("admin was denied access to another user's cart; want the guard to pass admins through")
	}
}

// TestGetCartRequiresAuthentication guards the unauthenticated path: without
// token metadata in the context the handler must refuse rather than fall back
// to whatever id the URL carries.
func TestGetCartRequiresAuthentication(t *testing.T) {
	app := newCartAuthzApp(nil)

	status, msg := requestStatus(t, app, "/cart/"+primitive.NewObjectID().Hex())
	if status != fiber.StatusUnauthorized {
		t.Fatalf("unauthenticated cart read: got status %d (%q), want %d",
			status, msg, fiber.StatusUnauthorized)
	}
}

// TestGetCartRejectsMalformedUserID keeps the malformed-id response a 400 and
// not a silent fallback to the token's own cart.
func TestGetCartRejectsMalformedUserID(t *testing.T) {
	app := newCartAuthzApp(&middleware.TokenMetadata{
		UserID: primitive.NewObjectID(),
		Role:   "user",
	})

	status, msg := requestStatus(t, app, "/cart/not-an-object-id")
	if status != fiber.StatusBadRequest {
		t.Fatalf("malformed user id: got status %d (%q), want %d",
			status, msg, fiber.StatusBadRequest)
	}
}

// recoverToStatus turns the nil-DB panic that follows a successful
// authorization check into a plain 500, so the admin case can assert "not 403"
// without the panic escaping the test.
func recoverToStatus() fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = c.SendStatus(fiber.StatusInternalServerError)
			}
		}()
		return c.Next()
	}
}
