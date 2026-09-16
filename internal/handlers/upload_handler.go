package handlers

import (
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
)

// UploadHandler handles multipart image uploads into Firebase Storage.
//
// The Firebase client is shared through a Provider rather than rebuilt per
// request; see internal/firebase/provider.go.
type UploadHandler struct {
	Config   *config.Config
	Firebase *firebase.Provider
	// Media is the shared bucket inventory. Every object written here is
	// recorded in it, or the reader would treat this upload as missing until
	// the inventory's TTL expires and blank it out of the catalog. See
	// mediaindex.Index.Note.
	Media *mediaindex.Index
}

// NewUploadHandler creates an upload handler bound to the shared Firebase
// provider and bucket inventory.
func NewUploadHandler(cfg *config.Config, fb *firebase.Provider, media *mediaindex.Index) *UploadHandler {
	return &UploadHandler{Config: cfg, Firebase: fb, Media: media}
}

// Upload stores every file in the "images" multipart field and returns their
// canonical public URLs.
//
// Firebase Storage is the single source of truth for images in every
// environment. There is deliberately no local-filesystem fallback: it used to
// persist request-host URLs (c.BaseURL()+"/uploads/...") into the database,
// permanently coupling image URLs to whichever domain served the upload.
// Failing loudly here keeps stored references canonical.
func (h *UploadHandler) Upload(c *fiber.Ctx) error {
	form, err := c.MultipartForm()
	if err != nil {
		log.Printf("[UPLOAD] Multipart form error: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid multipart form",
			"error":   err.Error(),
		})
	}

	files := form.File["images"]
	if len(files) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "No images provided",
		})
	}

	ctx := c.Context()
	fbClient, err := h.Firebase.Client(ctx)
	if err != nil {
		log.Printf("[UPLOAD] Firebase client unavailable: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to init Firebase client",
			"error":   err.Error(),
		})
	}

	urls := make([]string, 0, len(files))
	for i, f := range files {
		log.Printf("[UPLOAD] Processing file %d/%d: %s", i+1, len(files), f.Filename)

		file, err := f.Open()
		if err != nil {
			log.Printf("[UPLOAD] Failed to open file %s: %v", f.Filename, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to open file",
				"error":   err.Error(),
			})
		}

		url, err := fbClient.UploadFile(ctx, file, f.Filename)
		// Closed explicitly rather than deferred: a deferred close inside this
		// loop would hold every handle open until the whole batch finished.
		file.Close()
		if err != nil {
			log.Printf("[UPLOAD] Failed to upload %s to Firebase: %v", f.Filename, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to upload to Firebase",
				"error":   err.Error(),
			})
		}

		urls = append(urls, url)
	}

	// Before answering: the client is about to store these URLs on a product,
	// and the read path drops references the inventory has not seen.
	h.Media.Note(urls...)

	log.Printf("[UPLOAD] Uploaded %d file(s) successfully", len(urls))
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Upload successful",
		"data":    fiber.Map{"urls": urls},
	})
}
