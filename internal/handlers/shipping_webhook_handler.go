package handlers

import (
	"log"

	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// ShippingWebhookHandler receives carrier status callbacks.
//
// Every callback is authenticated before any state is read or written. The
// previous Delhivery endpoint was completely unauthenticated: anyone who knew
// a waybill could drive an order to "delivered", and -- because the same
// handler also settled COD payment -- mark an unpaid order as paid.
type ShippingWebhookHandler struct {
	Service *shipping.Service
}

// NewShippingWebhookHandler builds the webhook handler.
func NewShippingWebhookHandler(svc *shipping.Service) *ShippingWebhookHandler {
	return &ShippingWebhookHandler{Service: svc}
}

// requestHeaders flattens fiber's multi-value header map for the providers.
func requestHeaders(c *fiber.Ctx) map[string]string {
	raw := c.GetReqHeaders()
	out := make(map[string]string, len(raw)+1)
	for k, values := range raw {
		if len(values) > 0 {
			out[k] = values[0]
		}
	}
	// Delhivery lets the callback URL be configured freely but sends no
	// headers of our choosing, so a query parameter is accepted as an
	// equivalent carrier of the shared secret.
	if token := c.Query("token"); token != "" {
		if _, exists := out["token"]; !exists {
			out["token"] = token
		}
	}
	return out
}

// ShiprocketWebhook handles POST /webhooks/shipping/shiprocket.
//
// Shiprocket presents the configured security token as x-api-key.
func (h *ShippingWebhookHandler) ShiprocketWebhook(c *fiber.Ctx) error {
	return h.handle(c, shipping.ProviderShiprocket)
}

// DelhiveryWebhook handles POST /webhooks/delhivery.
//
// Same path as before so a configured carrier callback keeps working, but it
// now requires the shared secret.
func (h *ShippingWebhookHandler) DelhiveryWebhook(c *fiber.Ctx) error {
	return h.handle(c, shipping.ProviderDelhivery)
}

// handle authenticates, normalizes and applies a carrier callback.
//
// The order of operations is the security property: authenticate, parse,
// resolve against identifiers we ourselves persisted, then apply. Nothing is
// read from the database until the caller has proved it holds the secret.
func (h *ShippingWebhookHandler) handle(c *fiber.Ctx, providerName string) error {
	provider, err := h.Service.Provider(providerName)
	if err != nil {
		log.Printf("[WEBHOOK] %s is not registered", providerName)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"success": false,
			"message": "Shipping provider unavailable",
		})
	}

	event, err := provider.ParseWebhook(requestHeaders(c), c.Body())
	if err != nil {
		se := shipping.AsError(err)
		// The reason stays in the log. Telling an unauthenticated caller
		// whether the secret was missing, wrong, or the body malformed would
		// help them work out which.
		log.Printf("[WEBHOOK] rejected %s callback: %s", providerName, se.Detail)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Webhook rejected",
		})
	}

	// A callback may only ever speak for its own provider.
	if event.Provider != providerName {
		log.Printf("[WEBHOOK] provider mismatch: endpoint=%s event=%s", providerName, event.Provider)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Webhook rejected",
		})
	}

	if err := h.Service.ApplyWebhookEvent(c.UserContext(), event); err != nil {
		se := shipping.AsError(err)
		if se.Code == shipping.CodeShipmentNotFound {
			// Authenticated but addressed to something we do not have. Worth
			// surfacing rather than silently accepting: with the endpoint now
			// authenticated, this is a real signal, not background noise.
			log.Printf("[WEBHOOK] %s event for unknown shipment (awb=%q shipment=%q order=%q)",
				providerName, event.TrackingNumber, event.ProviderShipmentID, event.ProviderOrderID)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Unknown shipment",
			})
		}
		log.Printf("[WEBHOOK] failed to apply %s event: %s", providerName, se.Detail)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Webhook could not be processed",
		})
	}

	// 200 only once the event has been safely applied. Duplicate and
	// out-of-order events are absorbed by ApplyWebhookEvent and also land
	// here, which is correct: the carrier should stop retrying them.
	return c.JSON(fiber.Map{"success": true})
}
