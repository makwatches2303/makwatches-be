package handlers

import (
	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
)

// registerCatalogRoutes wires the public product, category and home-content
// reads, plus the admin product writes that hang off the same /products group.
func registerCatalogRoutes(d *routeDeps) {
	// Legacy product routes.
	products := d.app.Group("/products")
	products.Get("/", d.product.GetProducts)
	products.Get("/:id", d.product.GetProductByID)
	products.Get("/:productId/reviews", d.review.GetProductReviews)

	// Public catalog (reduced payload) routes.
	catalog := d.app.Group("/catalog")
	catalog.Get("/products", d.product.GetPublicProducts)
	catalog.Get("/products/:id", d.product.GetPublicProductByID)
	catalog.Get("/filters", d.product.GetCatalogFilters)

	// Public, read-only category routes for the storefront.
	d.app.Get("/categories", d.category.GetPublicCategories)
	d.app.Get("/categories/:name/subcategories", d.category.GetPublicSubcategories)

	// Public home content.
	d.app.Get("/home-content", d.homeContent.GetHomeContent)
	d.app.Get("/home-content/product/:productId", d.homeContent.GetHomeContentByProductID)

	// Admin product writes. Authentication runs before the role check.
	adminProducts := products.Group("/", middleware.Auth(d.cfg.JWTSecret), middleware.Role("admin"))
	adminProducts.Post("/", d.product.CreateProduct)
	adminProducts.Put("/:id", d.product.UpdateProduct)
	adminProducts.Delete("/:id", d.product.DeleteProduct)
}

// registerMediaRoutes wires image upload and the legacy static asset mount.
//
// The /uploads mount is retained deliberately. Firebase Storage is the only
// write target for new media, but ./uploads still holds files written before
// that was true, and legacy records may still reference them. Removing the
// mount is a separate, verified step -- not a Phase 1 side effect.
func registerMediaRoutes(d *routeDeps) {
	d.app.Static("/uploads", "./uploads")
	d.app.Get("/uploads/*", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNotFound)
	})

	d.app.Post("/upload",
		middleware.Auth(d.cfg.JWTSecret),
		middleware.Role("admin"),
		d.upload.Upload,
	)
}

// registerReviewRoutes wires authenticated review writes. Public product
// reviews are read through the /products group in registerCatalogRoutes.
func registerReviewRoutes(d *routeDeps) {
	reviews := d.protectedGroup("/reviews")
	reviews.Post("/", d.review.CreateReview)
	reviews.Put("/:id", d.review.UpdateReview)
	reviews.Delete("/:id", d.review.DeleteReview)
	reviews.Post("/:id/helpful", d.review.MarkReviewHelpful)
}
