package scrape

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// Shopify powers a large share of watch brands' own stores, and every one of
// them exposes the product as JSON at the page URL with ".js" appended. That
// endpoint is the authoritative record behind the page -- title, vendor,
// full-resolution images, price in paise, SKU -- so when a URL looks like a
// Shopify product it is worth one cheap request before parsing any markup.

// shopifyProduct is the subset of the .js payload worth reading.
type shopifyProduct struct {
	Title       string   `json:"title"`
	Handle      string   `json:"handle"`
	Description string   `json:"description"`
	Vendor      string   `json:"vendor"`
	Type        string   `json:"type"`
	Tags        []string `json:"tags"`
	// Prices are integers in the shop's smallest currency unit.
	Price          int64    `json:"price"`
	CompareAtPrice *int64   `json:"compare_at_price"`
	Images         []string `json:"images"`
	FeaturedImage  string   `json:"featured_image"`
	Variants       []struct {
		SKU            string `json:"sku"`
		Title          string `json:"title"`
		Price          int64  `json:"price"`
		CompareAtPrice *int64 `json:"compare_at_price"`
		Available      bool   `json:"available"`
	} `json:"variants"`
}

// shopifyEndpoint returns the .js URL for a Shopify product page, or "" when
// the URL is not shaped like one.
func shopifyEndpoint(target *url.URL) string {
	segments := strings.Split(strings.Trim(target.Path, "/"), "/")
	for i, segment := range segments {
		if segment == "products" && i+1 < len(segments) {
			handle := segments[i+1]
			if handle == "" || strings.HasSuffix(handle, ".js") {
				return ""
			}
			endpoint := *target
			endpoint.Path = "/" + strings.Join(append(segments[:i+1:i+1], handle), "/") + ".js"
			endpoint.RawQuery = ""
			endpoint.Fragment = ""
			return endpoint.String()
		}
	}
	return ""
}

// extractShopify fetches and converts the JSON product record.
//
// Returns nil for anything that is not a Shopify store: the endpoint either
// 404s or answers with the HTML page, and both are normal for the roughly
// nine in ten URLs that are not Shopify.
func extractShopify(ctx context.Context, fetcher *Fetcher, target *url.URL) *Draft {
	endpoint := shopifyEndpoint(target)
	if endpoint == "" {
		return nil
	}

	page, err := fetcher.GetJSON(ctx, endpoint)
	if err != nil || page.Status != 200 {
		return nil
	}
	if !strings.Contains(strings.ToLower(page.ContentType), "json") {
		return nil
	}

	var product shopifyProduct
	if err := json.Unmarshal(page.Body, &product); err != nil || product.Title == "" {
		return nil
	}

	draft := &Draft{
		Extractor:   "shopify",
		Name:        product.Title,
		Brand:       product.Vendor,
		Description: cleanHTMLText(product.Description),
		Category:    product.Type,
		Price:       float64(product.Price) / 100,
		Currency:    "INR",
	}
	if product.CompareAtPrice != nil {
		draft.MRP = float64(*product.CompareAtPrice) / 100
	}

	for _, image := range append([]string{product.FeaturedImage}, product.Images...) {
		if image == "" {
			continue
		}
		draft.Images = append(draft.Images, UpgradeImageURL(absolute(target, image)))
	}

	// A single-variant product is the common case for a watch, and its SKU is
	// the manufacturer's model number. Multi-variant products are left
	// without one on purpose: picking the first would attach the wrong code
	// to whichever colourway the admin ends up publishing.
	if len(product.Variants) == 1 {
		draft.SKU = product.Variants[0].SKU
		if draft.Price == 0 {
			draft.Price = float64(product.Variants[0].Price) / 100
		}
	} else if len(product.Variants) > 1 {
		draft.Warnings = append(draft.Warnings,
			"This page sells several variants. Check which one these details and photos belong to.")
	}

	if draft.Empty() {
		return nil
	}
	return draft
}
