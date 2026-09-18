package handlers

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/scrape"
)

// ProductImportHandler turns a product page somewhere on the web into a draft
// an admin can review, and copies the images they keep into our own storage.
//
// Deliberately two steps rather than one. The scrape reads someone else's page
// and writes nothing: prices, names and photographs from a marketplace are
// claims, not facts about our catalogue, and a person has to look at them
// before they become a product. The import step then runs only on what that
// person actually approved -- which is also why the bucket does not fill with
// images from every listing anyone ever pasted in.
type ProductImportHandler struct {
	Scraper  *scrape.Service
	Fetcher  *scrape.Fetcher
	Firebase *firebase.Provider
	// Media is the bucket inventory. Anything written here has to be recorded
	// in it or the read path treats it as missing; see mediaindex.Index.Note.
	Media *mediaindex.Index
}

// NewProductImportHandler builds the handler from the shared providers.
func NewProductImportHandler(cfg *config.Config, fb *firebase.Provider, media *mediaindex.Index) *ProductImportHandler {
	return &ProductImportHandler{
		Scraper:  scrape.NewService(cfg.ScrapeProxyTemplate),
		Fetcher:  scrape.NewFetcher(),
		Firebase: fb,
		Media:    media,
	}
}

// contextWithTimeout gives outbound work its own budget.
//
// Detached from the request context on purpose: a scrape involves someone
// else's server, and an admin closing the tab mid-request should not cancel an
// upload that is already half-written to the bucket. The ceiling sits inside
// API Gateway's own 30s so the caller still hears an answer.
func contextWithTimeout(c *fiber.Ctx, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.UserContext()), d)
}

// scrapeRequest is the body of POST /admin/products/scrape.
type scrapeRequest struct {
	URL string `json:"url"`
	// HTML, when present, is the page source the admin copied out of their own
	// browser, for sites that refuse a server outright. The URL is still sent
	// alongside it so relative image paths resolve and the draft records where
	// it came from.
	HTML string `json:"html"`
}

// ScrapeProduct reads a product page and returns a draft.
//
// Never returns a partial success as a failure or the other way round: a draft
// with gaps comes back 200 carrying warnings, because a name and four
// photographs with no price still saves an admin ten minutes of typing. Only a
// page we could not read at all is an error.
func (h *ProductImportHandler) ScrapeProduct(c *fiber.Ctx) error {
	var req scrapeRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    "INVALID_REQUEST",
			"message": "We could not read that request.",
			"detail":  err.Error(),
		})
	}

	if strings.TrimSpace(req.URL) == "" && strings.TrimSpace(req.HTML) == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success":     false,
			"code":        "INVALID_REQUEST",
			"message":     "Paste the link to the product page first.",
			"fieldErrors": fiber.Map{"url": "Enter a product page link."},
		})
	}

	// Detached from the request context with its own budget: reading a page
	// can involve a redirect chain and a second attempt through the reader
	// proxy, and an admin's browser giving up first should not leave that
	// work half-done. The ceiling is well inside API Gateway's 30s.
	ctx, cancel := contextWithTimeout(c, 25*time.Second)
	defer cancel()

	var draft *scrape.Draft
	var err error
	if strings.TrimSpace(req.HTML) != "" {
		draft, err = h.Scraper.ScrapeHTML(ctx, req.URL, req.HTML)
	} else {
		draft, err = h.Scraper.Scrape(ctx, req.URL)
	}
	if err != nil {
		var scrapeErr *scrape.Error
		if errors.As(err, &scrapeErr) {
			status := fiber.StatusUnprocessableEntity
			switch scrapeErr.Code {
			case scrape.CodePageBlocked:
				// Not our failure and not the admin's: the far end refused.
				status = fiber.StatusBadGateway
			case scrape.CodeFetchFailed:
				status = fiber.StatusBadGateway
			}
			log.Printf("[IMPORT] scrape failed url=%q code=%s detail=%s", req.URL, scrapeErr.Code, scrapeErr.Detail)
			return c.Status(status).JSON(fiber.Map{
				"success": false,
				"code":    scrapeErr.Code,
				"message": scrapeErr.Message,
				"detail":  scrapeErr.Detail,
			})
		}
		log.Printf("[IMPORT] scrape failed url=%q err=%v", req.URL, err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success": false,
			"code":    "SCRAPE_FAILED",
			"message": "We could not read that page.",
			"detail":  err.Error(),
		})
	}

	log.Printf("[IMPORT] scraped url=%q via=%s images=%d price=%v",
		draft.SourceURL, draft.Extractor, len(draft.Images), draft.Price)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Draft ready for review.",
		"data":    draft,
	})
}

// importImagesRequest is the body of POST /admin/products/import-images.
type importImagesRequest struct {
	Images []string `json:"images"`
	// SourceURL is sent as the referer, which some image CDNs require before
	// they will serve the real asset.
	SourceURL string `json:"sourceUrl"`
	// Name seeds the stored object names, so the bucket stays legible.
	Name string `json:"name"`
}

// ImportImages copies approved images into our own storage and returns our
// URLs, which are what the product create call then stores.
func (h *ProductImportHandler) ImportImages(c *fiber.Ctx) error {
	var req importImagesRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    "INVALID_REQUEST",
			"message": "We could not read that request.",
			"detail":  err.Error(),
		})
	}

	if len(req.Images) == 0 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success":     false,
			"code":        "INVALID_REQUEST",
			"message":     "Choose at least one image to import.",
			"fieldErrors": fiber.Map{"images": "Select the photos to keep."},
		})
	}

	ctx, cancel := contextWithTimeout(c, 25*time.Second)
	defer cancel()

	client, err := h.Firebase.Client(ctx)
	if err != nil {
		log.Printf("[IMPORT] storage unavailable: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "STORAGE_UNAVAILABLE",
			"message": "Image storage is not configured, so images cannot be imported.",
			"detail":  err.Error(),
		})
	}

	started := time.Now()
	imported, rejected := scrape.ImportImages(ctx, h.Fetcher, client, req.Images, req.SourceURL, req.Name)
	elapsed := time.Since(started)

	urls := make([]string, 0, len(imported))
	for _, image := range imported {
		urls = append(urls, image.URL)
	}
	// The read path drops references the inventory has not seen, so a product
	// saved with these URLs would come back with no imagery until the
	// inventory's TTL lapsed.
	h.Media.Note(urls...)

	if len(imported) == 0 {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success":  false,
			"code":     "IMAGE_IMPORT_FAILED",
			"message":  "None of those images could be imported.",
			"rejected": rejected,
		})
	}

	// Timed because this is the one request an admin waits on, and the cost is
	// latency to two other services rather than anything done here -- so when
	// it gets slow, the number of images is what explains it.
	log.Printf("[IMPORT] imported %d image(s), rejected %d, in %v (%v each) from %q",
		len(imported), len(rejected), elapsed.Round(time.Millisecond),
		(elapsed / time.Duration(max(len(req.Images), 1))).Round(time.Millisecond), req.SourceURL)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Images imported.",
		"data": fiber.Map{
			"images":   imported,
			"urls":     urls,
			"rejected": rejected,
		},
	})
}
