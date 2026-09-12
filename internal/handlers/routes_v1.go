package handlers

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
	catalog.Get("/variants", d.catalogV1.ListVariants)

	v1.Get("/collections", d.catalogV1.ListCollections)
	v1.Get("/search", d.catalogV1.Search)

	// The admin-managed presentation layer. Public: the storefront reads it on
	// every render.
	v1.Get("/storefront", d.storefront.GetStorefront)

	// Marketing & WhatsApp Lead Capture
	v1.Post("/subscribers/whatsapp", d.subscriber.SubscribeWhatsApp)
	v1.Post("/subscribers", d.subscriber.SubscribeWhatsApp)

	// Cart activity tracking & abandoned recovery
	v1.Post("/cart/track", d.cartTracker.TrackCart)
	v1.Post("/cart/check-abandoned", d.cartTracker.CheckAndSendAbandonedCartReminders)

	// Admin subscribers list
	d.admin.Get("/subscribers", d.subscriber.ListSubscribers)

	// Coupon validation
	v1.Post("/coupons/validate", d.coupon.ValidateCoupon)

	v1.Get("/health", HealthHandler)
}
