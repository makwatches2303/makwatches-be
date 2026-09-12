package handlers

// registerAdminRoutes wires everything under /admin. The group already carries
// JWT auth plus the admin role check (see SetupRoutes), so nothing here repeats
// that middleware.
//
// This is the surface the existing admin application talks to. Paths and
// handlers are unchanged from the pre-refactor route table.
func registerAdminRoutes(d *routeDeps) {
	registerAdminAccountRoutes(d)
	registerAdminSettingsRoutes(d)
	registerAdminHomeContentRoutes(d)
	registerAdminCategoryRoutes(d)
	registerAdminShippingRoutes(d)
	registerAdminStorefrontRoutes(d)
	registerAdminAnalyticsRoutes(d)
	registerAdminReviewRoutes(d)
	registerAdminSecurityRoutes(d)
}

// registerAdminSecurityRoutes wires the admin dashboard's Security tab:
// changing your own password, and recent admin sign-ins.
func registerAdminSecurityRoutes(d *routeDeps) {
	security := d.admin.Group("/security")
	security.Put("/password", d.auth.ChangePassword)
	security.Get("/login-activity", d.auth.GetLoginActivity)
}

// registerAdminReviewRoutes wires review moderation: browsing every review
// across products (not just one product's, or one user's own) and deleting
// any of them.
func registerAdminReviewRoutes(d *routeDeps) {
	reviews := d.admin.Group("/reviews")
	reviews.Get("/", d.review.GetAllReviews)
	reviews.Delete("/:id", d.review.DeleteReviewAdmin)
}

// registerAdminAnalyticsRoutes wires the admin dashboard's aggregate
// figures -- stat tiles, order status breakdown, recent orders and orders
// needing attention, computed server-side instead of by fetching every
// order into the browser.
func registerAdminAnalyticsRoutes(d *routeDeps) {
	d.admin.Get("/analytics/summary", d.analytics.GetDashboardSummary)
}

// registerAdminStorefrontRoutes wires the storefront presentation layer: the
// hero copy, section toggles and product rails the admin controls.
func registerAdminStorefrontRoutes(d *routeDeps) {
	storefront := d.admin.Group("/storefront")
	storefront.Get("/", d.storefront.GetAdminStorefront)
	storefront.Put("/", d.storefront.UpdateStorefront)
	storefront.Post("/reset", d.storefront.ResetStorefront)
}

func registerAdminAccountRoutes(d *routeDeps) {
	d.admin.Get("/accounts", d.adminAcct.GetAllAccounts)
	d.admin.Delete("/accounts/:id", d.adminAcct.DeleteAccount)
}

func registerAdminSettingsRoutes(d *routeDeps) {
	d.admin.Get("/settings", d.settings.GetSettings())
	d.admin.Put("/settings", d.settings.UpdateSettings())
	d.admin.Post("/settings/logo", d.settings.UploadLogo())
}

// registerAdminHomeContentRoutes wires the homepage CMS: hero slides, category
// cards, collection features, tech showcase and gallery.
func registerAdminHomeContentRoutes(d *routeDeps) {
	home := d.admin.Group("/home-content")

	home.Get("/hero-slides", d.homeContent.ListHeroSlides)
	home.Post("/hero-slides", d.homeContent.CreateHeroSlide)
	home.Put("/hero-slides/:id", d.homeContent.UpdateHeroSlide)
	home.Delete("/hero-slides/:id", d.homeContent.DeleteHeroSlide)

	home.Get("/categories", d.homeContent.ListCategoryCards)
	home.Post("/categories", d.homeContent.CreateCategoryCard)
	home.Put("/categories/:id", d.homeContent.UpdateCategoryCard)
	home.Delete("/categories/:id", d.homeContent.DeleteCategoryCard)

	home.Get("/collections", d.homeContent.ListCollectionFeatures)
	home.Post("/collections", d.homeContent.CreateCollectionFeature)
	home.Put("/collections/:id", d.homeContent.UpdateCollectionFeature)
	home.Delete("/collections/:id", d.homeContent.DeleteCollectionFeature)

	home.Get("/tech-cards", d.homeContent.ListTechCards)
	home.Post("/tech-cards", d.homeContent.CreateTechCard)
	home.Put("/tech-cards/:id", d.homeContent.UpdateTechCard)
	home.Delete("/tech-cards/:id", d.homeContent.DeleteTechCard)

	home.Get("/tech-highlight", d.homeContent.GetTechHighlight)
	home.Put("/tech-highlight", d.homeContent.UpsertTechHighlight)
	home.Delete("/tech-highlight", d.homeContent.DeleteTechHighlight)

	home.Get("/gallery", d.homeContent.ListGalleryImages)
	home.Post("/gallery", d.homeContent.CreateGalleryImage)
	home.Put("/gallery/:id", d.homeContent.UpdateGalleryImage)
	home.Delete("/gallery/:id", d.homeContent.DeleteGalleryImage)
}

func registerAdminCategoryRoutes(d *routeDeps) {
	categories := d.admin.Group("/categories")

	categories.Get("/", d.category.GetCategories)
	categories.Post("/", d.category.CreateCategory)
	categories.Post("/:id/subcategories", d.category.AddSubcategory)
	categories.Patch("/:id", d.category.UpdateCategoryName)
	categories.Patch("/:categoryId/subcategories/:subId", d.category.UpdateSubcategoryName)
	categories.Delete("/:id", d.category.DeleteCategory)
	categories.Delete("/:categoryId/subcategories/:subId", d.category.DeleteSubcategory)

	categories.Put("/:id/discount", d.category.UpdateCategoryDiscount)
	categories.Put("/:id/subcategories/:subId/discount", d.category.UpdateSubcategoryDiscount)
}

func registerAdminShippingRoutes(d *routeDeps) {
	shipping := d.admin.Group("/shipping")

	shipping.Post("/orders/:orderID/retry", d.shipping.RetryShipment)
	shipping.Post("/orders/:orderID/cancel", d.shipping.CancelShipment)
	shipping.Get("/orders/:orderID/label", d.shipping.GetShippingLabel)
	shipping.Post("/bulk-track", d.shipping.BulkTrackShipments)
	shipping.Post("/request-pickup", d.shipping.RequestPickup)
}
