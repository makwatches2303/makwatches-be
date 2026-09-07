package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Product represents a product in the system
type Product struct {
	ID           primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name         string             `json:"name" bson:"name"`
	Brand        string             `json:"brand,omitempty" bson:"brand,omitempty"`
	Description  string             `json:"description" bson:"description"`
	Price        float64            `json:"price" bson:"price"`
	Category     string             `json:"category" bson:"category"`
	MainCategory string             `json:"mainCategory,omitempty" bson:"main_category,omitempty"`
	Subcategory  string             `json:"subcategory,omitempty" bson:"subcategory,omitempty"`
	ImageURL     string             `json:"imageUrl" bson:"image_url"` // Main image (legacy support)
	Images       []string           `json:"images" bson:"images"`      // Multiple S3 image URLs
	Stock        int                `json:"stock" bson:"stock"`
	// Optional filterable attributes (for dynamic filters)
	Gender        string `json:"gender,omitempty" bson:"gender,omitempty"`
	DialColor     string `json:"dialColor,omitempty" bson:"dial_color,omitempty"`
	DialShape     string `json:"dialShape,omitempty" bson:"dial_shape,omitempty"`
	DialType      string `json:"dialType,omitempty" bson:"dial_type,omitempty"`
	StrapColor    string `json:"strapColor,omitempty" bson:"strap_color,omitempty"`
	StrapMaterial string `json:"strapMaterial,omitempty" bson:"strap_material,omitempty"`
	Style         string `json:"style,omitempty" bson:"style,omitempty"`
	DialThickness string `json:"dialThickness,omitempty" bson:"dial_thickness,omitempty"`
	// Discount fields (optional)
	DiscountPercentage *float64   `json:"discountPercentage,omitempty" bson:"discount_percentage,omitempty"` // Percentage discount (0-100)
	DiscountAmount     *float64   `json:"discountAmount,omitempty" bson:"discount_amount,omitempty"`         // Fixed amount discount
	DiscountStartDate  *time.Time `json:"discountStartDate,omitempty" bson:"discount_start_date,omitempty"`  // When discount starts
	DiscountEndDate    *time.Time `json:"discountEndDate,omitempty" bson:"discount_end_date,omitempty"`      // When discount ends

	// ── Reconstruction fields (Phase 1) ─────────────────────────────────────
	// Every field below is additive and optional. Existing records predate them
	// and decode with zero values; `omitempty` keeps them out of both the BSON
	// written back and the JSON sent to clients, so an un-migrated product is
	// indistinguishable from its former self on the wire. Nothing here is ever
	// populated with a fabricated value -- an unknown specification stays unset
	// so the storefront renders nothing rather than inventing a claim.

	Slug             string     `json:"slug,omitempty" bson:"slug,omitempty"`                          // URL identity for /product/[slug]
	SKU              string     `json:"sku,omitempty" bson:"sku,omitempty"`                            // Merchant stock-keeping unit
	Collection       string     `json:"collection,omitempty" bson:"collection,omitempty"`              // Editorial grouping, distinct from Category
	CompareAtPrice   *float64   `json:"compareAtPrice,omitempty" bson:"compare_at_price,omitempty"`    // Struck-through reference price
	ShortDescription string     `json:"shortDescription,omitempty" bson:"short_description,omitempty"` // One-line summary for cards
	Media            []MediaRef `json:"media,omitempty" bson:"media,omitempty"`                        // Structured media, superseding Images[]
	Specs            *Specs     `json:"specs,omitempty" bson:"specs,omitempty"`                        // Watch specifications
	Status           string     `json:"status,omitempty" bson:"status,omitempty"`                      // ProductStatus*; empty is treated as published
	Featured         bool       `json:"featured,omitempty" bson:"featured,omitempty"`                  // Homepage feature flag
	NewArrival       bool       `json:"newArrival,omitempty" bson:"new_arrival,omitempty"`             // New-arrival flag
	Bestseller       bool       `json:"bestseller,omitempty" bson:"bestseller,omitempty"`              // Bestseller flag
	SEO              *SEO       `json:"seo,omitempty" bson:"seo,omitempty"`                            // Per-product metadata overrides

	CreatedAt time.Time `json:"createdAt" bson:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" bson:"updated_at"`
}

// Product publication states. An empty Status means the record predates this
// field, and is treated as published so existing catalog behaviour is unchanged.
const (
	ProductStatusDraft     = "draft"
	ProductStatusPublished = "published"
	ProductStatusArchived  = "archived"
)

// IsPublished reports whether the product should be visible on the storefront.
func (p *Product) IsPublished() bool {
	return p.Status == "" || p.Status == ProductStatusPublished
}

// MediaRef is a structured reference to a stored asset.
//
// It carries the storage object key rather than only a resolved URL, so the
// storage backend can change without rewriting records. URL remains present for
// records written before this model existed and for direct rendering.
type MediaRef struct {
	URL    string `json:"url" bson:"url"`                         // Canonical public URL
	Key    string `json:"key,omitempty" bson:"key,omitempty"`     // Storage object name within the bucket
	Alt    string `json:"alt,omitempty" bson:"alt,omitempty"`     // Accessible description
	Kind   string `json:"kind,omitempty" bson:"kind,omitempty"`   // "image" | "video"; empty means image
	Width  int    `json:"width,omitempty" bson:"width,omitempty"` // Intrinsic size, when known
	Height int    `json:"height,omitempty" bson:"height,omitempty"`
}

// Specs holds watch specifications.
//
// Every field is a pointer: nil means "not recorded", which is deliberately
// distinct from an empty string. The storefront omits nil fields entirely
// rather than rendering a blank row or a placeholder value.
type Specs struct {
	Movement        *string  `json:"movement,omitempty" bson:"movement,omitempty"`
	Case            *string  `json:"case,omitempty" bson:"case,omitempty"`
	Crystal         *string  `json:"crystal,omitempty" bson:"crystal,omitempty"`
	Dial            *string  `json:"dial,omitempty" bson:"dial,omitempty"`
	Strap           *string  `json:"strap,omitempty" bson:"strap,omitempty"`
	WaterResistance *string  `json:"waterResistance,omitempty" bson:"water_resistance,omitempty"`
	Dimensions      *string  `json:"dimensions,omitempty" bson:"dimensions,omitempty"`
	Warranty        *string  `json:"warranty,omitempty" bson:"warranty,omitempty"`
	BoxContents     []string `json:"boxContents,omitempty" bson:"box_contents,omitempty"`
}

// SEO holds per-product metadata overrides. Anything unset falls back to the
// product's own name and description at render time.
type SEO struct {
	Title       string `json:"title,omitempty" bson:"title,omitempty"`
	Description string `json:"description,omitempty" bson:"description,omitempty"`
	OGImage     string `json:"ogImage,omitempty" bson:"og_image,omitempty"`
}

// IsDiscountActive checks if the product has an active discount
func (p *Product) IsDiscountActive() bool {
	now := time.Now()

	// Check if discount exists
	if p.DiscountPercentage == nil && p.DiscountAmount == nil {
		return false
	}

	// Check date range if specified
	if p.DiscountStartDate != nil && now.Before(*p.DiscountStartDate) {
		return false
	}
	if p.DiscountEndDate != nil && now.After(*p.DiscountEndDate) {
		return false
	}

	return true
}

// GetFinalPrice returns the price after applying active discount
func (p *Product) GetFinalPrice() float64 {
	if !p.IsDiscountActive() {
		return p.Price
	}

	// Apply percentage discount first if exists
	if p.DiscountPercentage != nil && *p.DiscountPercentage > 0 {
		discount := p.Price * (*p.DiscountPercentage / 100.0)
		return p.Price - discount
	}

	// Apply fixed amount discount
	if p.DiscountAmount != nil && *p.DiscountAmount > 0 {
		finalPrice := p.Price - *p.DiscountAmount
		if finalPrice < 0 {
			return 0
		}
		return finalPrice
	}

	return p.Price
}

// GetDiscountAmount returns the discount amount applied
func (p *Product) GetDiscountAmount() float64 {
	if !p.IsDiscountActive() {
		return 0
	}
	return p.Price - p.GetFinalPrice()
}

// ProductFilters represents filters for product queries
type ProductFilters struct {
	Category string   `query:"category"`
	MinPrice *float64 `query:"minPrice"`
	MaxPrice *float64 `query:"maxPrice"`
	SortBy   string   `query:"sortBy"`
	Order    string   `query:"order"`
	Page     int      `query:"page"`
	Limit    int      `query:"limit"`
}
