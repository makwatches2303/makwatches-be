// Package imagefetch downloads and validates candidate product images
// before they're re-uploaded to Firebase Storage. Shared by every
// scraping/backfill tool in cmd/ (seed-catalog, backfill-images,
// apply-product-images, and whatever comes next) so a mistake made and
// fixed once here doesn't get relearned by hand the next time something
// scrapes this catalog.
//
// Two real mistakes this package exists to prevent, both found live on
// makwatches.in and fixed by hand before this package existed:
//
//  1. Uploading a thumbnail and calling it done. A Noise smartwatch product
//     got its images from Amazon's #altImages thumbnail strip, upsized by
//     rewriting the URL's size suffix to a larger value (e.g. "_SL1500_").
//     That trick works on some CDNs (see the SFCC note below) but Amazon's
//     thumbnail strip images are a genuinely separate, smaller asset from
//     the listing's real photos -- requesting a bigger size on a thumbnail
//     URL just re-serves the same small image, it does not upscale. The
//     result shipped live as a visibly blurry 500x500 image. Fetch enforces
//     MinWidth/MinHeight so this class of mistake fails loudly instead of
//     shipping.
//
//  2. Mixing images from different product variants into one gallery. Two
//     products' galleries ended up with photos from different color/strap
//     options blended together (e.g. one "black dial" listing showed two
//     black-dial angles plus a blue-dial photo and a white-dial photo --
//     different physical variants, not the same watch from different
//     angles). This happens when a source page's image gallery includes
//     swatch thumbnails for *other* purchasable variants alongside the
//     current one, and every image on the page gets scraped indiscriminately
//     instead of being scoped to the one variant/SKU being sourced. This
//     package can't detect that mechanically (a decoder can't tell "same
//     watch, different angle" from "different watch, similar framing") --
//     the fix is procedural: source every image for one product from a
//     single page/gallery that is itself scoped to one exact SKU or ASIN
//     (see "Sourcing correctly" below), never from a general listing/search
//     page that mixes multiple products or variants.
//
// # Getting real full-resolution images, by source
//
//   - Amazon: do NOT read the #altImages thumbnail strip's own <img src>.
//     Read the page's `data-old-hires` attribute on the main image, or --
//     more reliably, since data-old-hires is only populated for whichever
//     thumbnail is currently selected -- parse the `colorImages` JSON
//     embedded in a <script> tag on the product page (each entry has a
//     `hiRes` field). Those `hiRes` URLs point at different image IDs than
//     the thumbnail strip and are the seller's actual uploaded originals,
//     often 3-8x the thumbnail strip's own pixel dimensions.
//   - Salesforce Commerce Cloud sites (Titan/Fastrack/Sonata's platform):
//     a demandware image URL's `?sw=NNN&sh=NNN` query params can be safely
//     raised (e.g. to 1000) and the CDN serves that size for real -- this
//     one genuinely is just a URL rewrite, unlike Amazon's thumbnail strip.
//   - Any other source, or when unsure: fetch and check with this package
//     before trusting a URL's apparent size, rather than assuming a
//     bigger-looking query parameter means a bigger real image.
//
// # Sourcing one product's gallery correctly
//
// Every image passed to Fetch for one product should come from a page
// scoped to that exact SKU/ASIN/variant -- a single product detail page,
// not a category listing, search-results grid, or a variant-selector page
// showing swatches for options other than the one being sourced. If a
// source page shows color/material swatches for other purchasable
// variants, only take the gallery images belonging to the currently
// selected/target variant.
package imagefetch

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

// browserUserAgent works around retail/manufacturer CDNs and WAFs (SFCC's
// demandware, some site protections) that reject a bare Go http.Client's
// default User-Agent even though the image asset itself isn't behind the
// same bot-wall as the site's HTML pages.
const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// MinWidth and MinHeight are the catalog's floor for "a real product photo"
// rather than a thumbnail. 800px is comfortably above every thumbnail-strip
// size seen so far (Amazon's altImages thumbnails top out around 500px) and
// well below every genuine product-photo original seen so far (SFCC/Amazon
// full-size originals run 1000-2560px) -- there's a wide gap between the two
// classes of asset, this sits in it.
const (
	MinWidth  = 800
	MinHeight = 800
)

// Result is one successfully fetched and validated candidate image.
type Result struct {
	Body   []byte
	Ext    string // ".jpg" / ".png" / ".webp", from the URL
	Width  int
	Height int
}

// Fetch downloads srcURL (or reads it from local disk for a "file://" path
// -- an escape hatch for CDNs that block a plain HTTP client on TLS/bot-
// detection grounds even with a browser User-Agent, where the only option
// left is saving the image from an actual browser session first) and
// validates it decodes as an image of at least MinWidth x MinHeight.
//
// A failure here is a signal to go find the source's actual full-resolution
// asset instead -- see the package doc comment for where to look per
// source -- not to fall back to uploading the small image anyway.
func Fetch(ctx context.Context, srcURL string) (*Result, error) {
	var body []byte

	if localPath, ok := strings.CutPrefix(srcURL, "file://"); ok {
		var err error
		body, err = os.ReadFile(localPath)
		if err != nil {
			return nil, fmt.Errorf("reading local file: %w", err)
		}
	} else {
		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
		if err != nil {
			return nil, fmt.Errorf("bad request: %w", err)
		}
		req.Header.Set("User-Agent", browserUserAgent)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download returned %s", resp.Status)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading body failed: %w", err)
		}
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("not a decodable image: %w", err)
	}
	if cfg.Width < MinWidth || cfg.Height < MinHeight {
		return nil, fmt.Errorf(
			"image too small: %dx%d, need at least %dx%d -- this is likely a thumbnail; find the source's full-resolution original instead (see internal/imagefetch's doc comment for how, per source site)",
			cfg.Width, cfg.Height, MinWidth, MinHeight,
		)
	}

	return &Result{Body: body, Ext: extFromURL(srcURL), Width: cfg.Width, Height: cfg.Height}, nil
}

func extFromURL(url string) string {
	u := strings.SplitN(url, "?", 2)[0]
	switch {
	case strings.HasSuffix(strings.ToLower(u), ".png"):
		return ".png"
	case strings.HasSuffix(strings.ToLower(u), ".webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

// Slugify makes a string safe for use as (part of) a Firebase Storage object
// name: lowercase alphanumeric runs separated by single dashes, capped at 60
// characters.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
