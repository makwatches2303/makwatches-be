package handlers

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services/catalog"
)

// CatalogV1Handler serves the /api/v1 catalog surface.
//
// It is a thin HTTP adapter: query parsing and response shaping live here,
// while every Mongo query lives in the catalog service. The legacy /products
// and /catalog handlers are untouched and still registered.
type CatalogV1Handler struct {
	Catalog *catalog.Service
	Config  *config.Config
}

// NewCatalogV1Handler builds the v1 catalog handler.
func NewCatalogV1Handler(db *database.DBClient, cfg *config.Config, media *mediaindex.Index) *CatalogV1Handler {
	return &CatalogV1Handler{
		Catalog: catalog.New(db, cfg.FirebaseBucketName, media),
		Config:  cfg,
	}
}

// ListProducts handles GET /api/v1/catalog/products
func (h *CatalogV1Handler) ListProducts(c *fiber.Ctx) error {
	q := catalog.Query{
		Category:     c.Query("category"),
		MainCategory: c.Query("mainCategory"),
		Subcategory:  c.Query("subcategory"),
		Collection:   c.Query("collection"),
		Gender:       c.Query("gender"),
		Search:       c.Query("q"),

		Brands:        queryList(c, "brand"),
		DialColor:     c.Query("dialColor"),
		DialShape:     c.Query("dialShape"),
		DialType:      c.Query("dialType"),
		StrapColor:    c.Query("strapColor"),
		StrapMaterial: c.Query("strapMaterial"),
		Style:         c.Query("style"),
		DialThickness: c.Query("dialThickness"),
		MinPrice:      queryFloat(c, "minPrice"),
		MaxPrice:      queryFloat(c, "maxPrice"),
		InStock:       queryBool(c, "inStock"),
		Featured:      queryBool(c, "featured"),
		NewArrival:    queryBool(c, "newArrival"),
		Bestseller:    queryBool(c, "bestseller"),
		SortBy:        c.Query("sortBy"),
		Order:         c.Query("order"),
		Page:          queryInt(c, "page", 1),
		Limit:         queryInt(c, "limit", 24),
	}

	page, err := h.Catalog.List(c.Context(), q)
	if err != nil {
		return internalError(c, "Failed to list products", err)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Products retrieved successfully",
		"data":    page.Items,
		"meta": fiber.Map{
			"page":  page.Page,
			"limit": page.Limit,
			"total": page.Total,
			"pages": page.Pages,
		},
	})
}

// GetProductByID handles GET /api/v1/catalog/products/:id
func (h *CatalogV1Handler) GetProductByID(c *fiber.Ctx) error {
	product, err := h.Catalog.GetByID(c.Context(), c.Params("id"))
	if errors.Is(err, catalog.ErrNotFound) {
		return notFound(c, "Product not found")
	}
	if err != nil {
		return internalError(c, "Failed to retrieve product", err)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Product retrieved successfully",
		"data":    product,
	})
}

// GetProductBySlug handles GET /api/v1/catalog/products/slug/:slug
func (h *CatalogV1Handler) GetProductBySlug(c *fiber.Ctx) error {
	product, err := h.Catalog.GetBySlug(c.Context(), c.Params("slug"))
	if errors.Is(err, catalog.ErrNotFound) {
		return notFound(c, "Product not found")
	}
	if err != nil {
		return internalError(c, "Failed to retrieve product", err)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Product retrieved successfully",
		"data":    product,
	})
}

// ListCollections handles GET /api/v1/collections
func (h *CatalogV1Handler) ListCollections(c *fiber.Ctx) error {
	collections, err := h.Catalog.ListCollections(c.Context())
	if err != nil {
		return internalError(c, "Failed to list collections", err)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Collections retrieved successfully",
		"data":    collections,
	})
}

// Search handles GET /api/v1/search?q=
func (h *CatalogV1Handler) Search(c *fiber.Ctx) error {
	result, err := h.Catalog.Search(c.Context(), c.Query("q"), queryInt(c, "limit", 12))
	if err != nil {
		return internalError(c, "Search failed", err)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Search completed",
		"data":    result,
	})
}

// ── query parsing helpers ───────────────────────────────────────────────────

func queryInt(c *fiber.Ctx, key string, fallback int) int {
	if raw := c.Query(key); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			return v
		}
	}
	return fallback
}

func queryFloat(c *fiber.Ctx, key string) *float64 {
	raw := c.Query(key)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return &v
}

// queryList parses a comma-separated multi-select parameter, trimming blanks.
func queryList(c *fiber.Ctx, key string) []string {
	raw := c.Query(key)
	if raw == "" {
		return nil
	}

	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func queryBool(c *fiber.Ctx, key string) bool {
	raw := strings.ToLower(c.Query(key))
	return raw == "1" || raw == "true" || raw == "yes"
}

// ── shared error responses ──────────────────────────────────────────────────

func notFound(c *fiber.Ctx, message string) error {
	return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
		"success": false,
		"message": message,
	})
}

func internalError(c *fiber.Ctx, message string, err error) error {
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
		"success": false,
		"message": message,
		"error":   err.Error(),
	})
}
