package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/revalidate"
)

// StorefrontHandler serves the admin-managed storefront presentation layer.
//
// One document, read publicly by the storefront and written only by an admin.
// See models.StorefrontContent for why presentation is kept separate from
// operational Settings.
type StorefrontHandler struct {
	DB     *database.DBClient
	Config *config.Config
	// Revalidator pushes a cache purge to the storefront after a write, so an
	// edit is visible on the live site immediately rather than when the
	// storefront's own cache expires. Nil when unconfigured, and every call on
	// it is then a no-op.
	Revalidator *revalidate.Notifier
}

// NewStorefrontHandler builds the storefront content handler.
func NewStorefrontHandler(db *database.DBClient, cfg *config.Config) *StorefrontHandler {
	return &StorefrontHandler{DB: db, Config: cfg}
}

// WithRevalidator attaches the storefront cache notifier.
//
// Separate from the constructor so every existing call site -- including the
// route tests, which build handlers without any network dependency -- keeps
// compiling and behaving exactly as before.
func (h *StorefrontHandler) WithRevalidator(n *revalidate.Notifier) *StorefrontHandler {
	h.Revalidator = n
	return h
}

const storefrontCacheKey = "storefront:content"

func (h *StorefrontHandler) collection() *mongo.Collection {
	return h.DB.MongoDB.Collection("storefront")
}

// load reads the single storefront document, falling back to defaults.
//
// A missing document is not an error: a fresh install has never been
// configured, and the storefront must still render. The defaults are truthful
// -- every section that would assert a business fact is disabled.
func (h *StorefrontHandler) load(ctx context.Context) (models.StorefrontContent, error) {
	var content models.StorefrontContent

	err := h.collection().FindOne(ctx, bson.M{}).Decode(&content)
	if err == mongo.ErrNoDocuments {
		return models.DefaultStorefrontContent(), nil
	}
	if err != nil {
		return models.StorefrontContent{}, err
	}

	// A document written before navigation or category tiles existed decodes
	// with those sections empty. Filling them from the defaults keeps an older
	// stored configuration rendering a complete storefront -- no migration, and
	// an API rollback stays safe.
	return content.WithDefaults(), nil
}

// GetStorefront handles GET /api/v1/storefront (public).
func (h *StorefrontHandler) GetStorefront(c *fiber.Ctx) error {
	ctx := c.Context()

	var cached models.StorefrontContent
	if err := h.DB.CacheGet(ctx, storefrontCacheKey, &cached); err == nil {
		return c.JSON(fiber.Map{
			"success": true,
			"message": "Storefront content retrieved from cache",
			"data":    cached,
		})
	}

	content, err := h.load(ctx)
	if err != nil {
		return internalError(c, "Failed to load storefront content", err)
	}

	// Only renderable menu entries leave the API. A disabled or destination-less
	// item is never shipped to the browser at all.
	content.Navigation = content.Navigation.PublicNavigation()

	// Short TTL: merchandising edits should appear quickly without a restart.
	_ = h.DB.CacheSet(ctx, storefrontCacheKey, content, 60*time.Second)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Storefront content retrieved successfully",
		"data":    content,
	})
}

// GetAdminStorefront handles GET /admin/storefront.
//
// Bypasses the cache so an admin always edits the current stored document
// rather than a copy that may be up to a minute stale, and returns the whole
// configuration -- disabled entries included, since those are what the admin is
// there to toggle.
//
// Also returns validation warnings for category tiles whose reference no longer
// resolves. Those tiles are silently omitted from the storefront, so without
// this the admin would have no way to notice one had broken.
func (h *StorefrontHandler) GetAdminStorefront(c *fiber.Ctx) error {
	ctx := c.Context()

	content, err := h.load(ctx)
	if err != nil {
		return internalError(c, "Failed to load storefront content", err)
	}

	warnings := h.tileWarnings(ctx, content)

	return c.JSON(fiber.Map{
		"success":  true,
		"message":  "Storefront content retrieved successfully",
		"data":     content,
		"warnings": warnings,
	})
}

// tileWarnings resolves the configured tiles against the live category tree.
//
// Never fails the request: an unreadable category list means the warnings are
// simply unknown, which must not stop the admin from loading the page they
// would use to fix things.
func (h *StorefrontHandler) tileWarnings(
	ctx context.Context,
	content models.StorefrontContent,
) []models.TileWarning {
	cursor, err := h.DB.Collections().Categories.Find(ctx, bson.M{})
	if err != nil {
		return []models.TileWarning{}
	}
	defer cursor.Close(ctx)

	var categories []models.Category
	if err := cursor.All(ctx, &categories); err != nil {
		return []models.TileWarning{}
	}

	_, warnings := models.ResolveCategoryTiles(content.CategoryTiles, categories)
	if warnings == nil {
		return []models.TileWarning{}
	}
	return warnings
}

// UpdateStorefront handles PUT /admin/storefront.
//
// Replaces the whole document. The admin UI loads the current content, edits
// it, and writes it back; a whole-document write means removing a rail or a
// stat is expressible, which a merge-patch could not do.
func (h *StorefrontHandler) UpdateStorefront(c *fiber.Ctx) error {
	ctx := c.Context()

	var incoming models.StorefrontContent
	if err := c.BodyParser(&incoming); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid storefront content",
			"error":   err.Error(),
		})
	}

	incoming.UpdatedAt = time.Now()
	if user, ok := c.Locals("user").(*middleware.TokenMetadata); ok && user != nil {
		incoming.UpdatedBy = user.UserID.Hex()
	}

	update := bson.M{"$set": bson.M{
		"navigation":     incoming.Navigation,
		"category_tiles": incoming.CategoryTiles,
		"listings":       incoming.Listings,
		"hero":           incoming.Hero,
		"trust":          incoming.Trust,
		"stats":          incoming.Stats,
		"craft":          incoming.Craft,
		"house":          incoming.House,
		"poster":         incoming.Poster,
		"boutique":       incoming.Boutique,
		"footer":         incoming.Footer,
		"marquee":        incoming.Marquee,
		"policies":       incoming.Policies,
		"rails":          incoming.Rails,
		"updated_at":     incoming.UpdatedAt,
		"updated_by":     incoming.UpdatedBy,
	}}

	opts := options.Update().SetUpsert(true)
	if _, err := h.collection().UpdateOne(ctx, bson.M{}, update, opts); err != nil {
		return internalError(c, "Failed to save storefront content", err)
	}

	// Drop the cache so the change is visible on the next storefront request
	// rather than after the TTL.
	h.DB.CacheDel(ctx, storefrontCacheKey)

	// ...and tell the storefront to drop its own copy. Clearing only the line
	// above leaves the rendered site serving the previous heading until Next's
	// data and route caches expire, which is what made saved edits look like
	// they had not been saved. Fire and forget: the write is already committed
	// and an unreachable storefront must not fail this response.
	h.Revalidator.Invalidate(revalidate.TagStorefront)

	saved, err := h.load(ctx)
	if err != nil {
		return internalError(c, "Saved, but failed to reload storefront content", err)
	}

	// Warnings travel with the save as well as the read. Without them the
	// editor clears its warning banner the moment an admin saves, which reads
	// as "fixed" when the broken tile is still broken.
	return c.JSON(fiber.Map{
		"success":  true,
		"message":  "Storefront content updated successfully",
		"data":     saved,
		"warnings": h.tileWarnings(ctx, saved),
	})
}

// ResetStorefront handles POST /admin/storefront/reset.
//
// Restores the shipped defaults. Useful when an edit has left the storefront in
// a broken state and the admin wants a known-good baseline.
func (h *StorefrontHandler) ResetStorefront(c *fiber.Ctx) error {
	ctx := c.Context()

	defaults := models.DefaultStorefrontContent()
	update := bson.M{"$set": bson.M{
		"navigation":     defaults.Navigation,
		"category_tiles": defaults.CategoryTiles,
		"listings":       defaults.Listings,
		"hero":           defaults.Hero,
		"trust":          defaults.Trust,
		"stats":          defaults.Stats,
		"craft":          defaults.Craft,
		"house":          defaults.House,
		"poster":         defaults.Poster,
		"boutique":       defaults.Boutique,
		"footer":         defaults.Footer,
		"marquee":        defaults.Marquee,
		"policies":       defaults.Policies,
		"rails":          defaults.Rails,
		"updated_at":     time.Now(),
	}}

	if _, err := h.collection().UpdateOne(ctx, bson.M{}, update, options.Update().SetUpsert(true)); err != nil {
		return internalError(c, "Failed to reset storefront content", err)
	}

	h.DB.CacheDel(ctx, storefrontCacheKey)
	h.Revalidator.Invalidate(revalidate.TagStorefront)

	return c.JSON(fiber.Map{
		"success":  true,
		"message":  "Storefront content reset to defaults",
		"data":     defaults,
		"warnings": h.tileWarnings(ctx, defaults),
	})
}
