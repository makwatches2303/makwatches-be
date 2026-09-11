package handlers

import (
	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
)

// registerV1Routes wires the versioned /api/v1 surface.
//
// This is additive. Every legacy flat route stays mounted and unchanged, so the
// existing storefront and the admin application keep working while the rebuilt
// frontend migrates onto /api/v1 route by route.
//
// Read routes here go through the catalog service rather than querying Mongo
// inline, which is the pattern the remaining domains move to in later phases.
func registerV1Routes(d *routeDeps) {
	v1 := d.app.Group("/api/v1")

	catalog := v1.Group("/catalog")
	catalog.Get("/products", d.catalogV1.ListProducts)
	// Registered before "/products/:id" so the literal segment wins the match
	// rather than being captured as an id.
	catalog.Get("/products/slug/:slug", d.catalogV1.GetProductBySlug)
	catalog.Get("/products/:id", d.catalogV1.GetProductByID)
	catalog.Get("/filters", d.product.GetCatalogFilters)

	v1.Get("/collections", d.catalogV1.ListCollections)
	v1.Get("/search", d.catalogV1.Search)

	// The admin-managed presentation layer. Public: the storefront reads it on
	// every render.
	v1.Get("/storefront", d.storefront.GetStorefront)

	v1.Get("/health", HealthHandler)

	registerV1ShippingRoutes(d, v1)
}

// registerV1ShippingRoutes wires the provider-neutral shipping surface.
//
// No route here names a carrier, and none exposes a carrier endpoint or URL to
// the browser: the same paths serve a Shiprocket order and a Delhivery order,
// and switching the primary provider changes nothing about them.
func registerV1ShippingRoutes(d *routeDeps, v1 fiber.Router) {
	// Public: the checkout page needs delivery options before an account
	// exists. Read-only -- quoting a rate never books a parcel.
	shipPublic := v1.Group("/shipping")
	shipPublic.Post("/serviceability", d.shippingV1.Serviceability)
	shipPublic.Post("/rates", d.shippingV1.Serviceability)
	shipPublic.Get("/serviceability", d.shippingV1.Serviceability)

	// The checkout courier picker. Authenticated, because the quotes it issues
	// are bound to the caller and their cart -- and only such a quote can
	// price an order.
	checkout := v1.Group("/checkout", middleware.Auth(d.cfg.JWTSecret))
	checkout.Post("/shipping-options", d.shippingV1.CheckoutShippingOptions)

	// Authenticated: tracking and the label are scoped to the order's owner,
	// or to an admin.
	ship := v1.Group("/shipping", middleware.Auth(d.cfg.JWTSecret))
	ship.Get("/orders/:orderID/tracking", d.shippingV1.Tracking)
	ship.Get("/orders/:orderID/label", d.shippingV1.Label)

	// Fulfillment operations are admin-only. The role check is inside the
	// handler as well, so a routing mistake cannot expose them.
	admin := ship.Group("/", middleware.Role("admin"))
	admin.Post("/orders/:orderID/create", d.shippingV1.CreateShipment)
	admin.Post("/orders/:orderID/awb", d.shippingV1.AssignAWB)
	admin.Post("/orders/:orderID/cancel", d.shippingV1.CancelShipment)
	admin.Post("/orders/:orderID/pickup", d.shippingV1.SchedulePickup)
	admin.Get("/pickup-locations", d.shippingV1.PickupLocations)
}
