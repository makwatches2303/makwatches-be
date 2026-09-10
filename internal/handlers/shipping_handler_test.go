package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
)

// newDelhiveryWebhookApp mounts DelhiveryWebhook with the given configured
// token. DB and DelhiveryService are left nil: every case here is decided by
// the token check at the top of the handler, before either is ever touched.
func newDelhiveryWebhookApp(token string) *fiber.App {
	app := fiber.New()
	h := &ShippingHandler{Config: &config.Config{DelhiveryWebhookToken: token}}
	app.Post("/webhooks/delhivery", h.DelhiveryWebhook)
	return app
}

// TestDelhiveryWebhookRejectsWhenTokenNotConfigured is the regression test
// for the critical fix in shipping_handler.go: this endpoint used to trust
// any anonymous POST with no verification at all. An unconfigured token must
// fail closed (503), not silently accept everything.
func TestDelhiveryWebhookRejectsWhenTokenNotConfigured(t *testing.T) {
	app := newDelhiveryWebhookApp("")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/webhooks/delhivery", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("unconfigured webhook token: got status %d, want %d", resp.StatusCode, fiber.StatusServiceUnavailable)
	}
}

// TestDelhiveryWebhookRejectsMissingToken covers a configured secret with no
// token presented on the request at all.
func TestDelhiveryWebhookRejectsMissingToken(t *testing.T) {
	app := newDelhiveryWebhookApp("correct-secret")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/webhooks/delhivery", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("missing webhook token: got status %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

// TestDelhiveryWebhookRejectsWrongToken covers a configured secret with an
// incorrect token presented via the query param.
func TestDelhiveryWebhookRejectsWrongToken(t *testing.T) {
	app := newDelhiveryWebhookApp("correct-secret")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/webhooks/delhivery?token=wrong", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("wrong webhook token: got status %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

// TestDelhiveryWebhookAcceptsCorrectToken proves the correct token clears the
// auth check and reaches payload parsing. The request body is empty, so
// ParseWebhook fails before the handler ever touches the (nil, in this test)
// database -- asserting 400 here, rather than 401/503, is what proves the
// token check itself passed.
func TestDelhiveryWebhookAcceptsCorrectToken(t *testing.T) {
	app := newDelhiveryWebhookApp("correct-secret")

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/webhooks/delhivery?token=correct-secret", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusUnauthorized || resp.StatusCode == fiber.StatusServiceUnavailable {
		t.Fatalf("correct webhook token was rejected: got status %d", resp.StatusCode)
	}
}

// TestDelhiveryWebhookAcceptsTokenViaBearerHeader covers the Authorization
// header form of presenting the token, not just the query param.
func TestDelhiveryWebhookAcceptsTokenViaBearerHeader(t *testing.T) {
	app := newDelhiveryWebhookApp("correct-secret")

	req := httptest.NewRequest(fiber.MethodPost, "/webhooks/delhivery", nil)
	req.Header.Set("Authorization", "Bearer correct-secret")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusUnauthorized || resp.StatusCode == fiber.StatusServiceUnavailable {
		t.Fatalf("correct bearer token was rejected: got status %d", resp.StatusCode)
	}
}
