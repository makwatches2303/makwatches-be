package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/queue"
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
//
// The import is a job, not a request. A whole variant group is dozens of
// photographs, each a download from the source and an upload to the bucket,
// and the API gateway allows a request thirty seconds -- which is how a
// nineteen-photograph import came within a second of timing out, and why the
// number of images used to be capped. Now the request records the job and
// hands each image to the worker; the panel polls for progress and gets every
// stored URL as it lands.
type ProductImportHandler struct {
	Scraper  *scrape.Service
	Fetcher  *scrape.Fetcher
	DB       *database.DBClient
	Firebase *firebase.Provider
	// Media is the bucket inventory. Anything written by this process has to
	// be recorded in it; anything written by the worker is found by probe.
	Media *mediaindex.Index
	// Queue hands images to the worker. Nil outside Lambda (local
	// development), in which case the images are stored inline and the job
	// is returned already complete.
	Queue *queue.Queue
}

// NewProductImportHandler builds the handler from the shared providers.
func NewProductImportHandler(cfg *config.Config, db *database.DBClient, fb *firebase.Provider, media *mediaindex.Index, q *queue.Queue) *ProductImportHandler {
	return &ProductImportHandler{
		Scraper:  scrape.NewService(cfg.ScrapeProxyTemplate),
		Fetcher:  scrape.NewFetcher(),
		DB:       db,
		Firebase: fb,
		Media:    media,
		Queue:    q,
	}
}

// maxImagesPerJob is a guard against a runaway request, not a working limit:
// a variant group of a dozen colourways with a full gallery each fits well
// inside it.
const maxImagesPerJob = 120

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
			case scrape.CodePageBlocked, scrape.CodeFetchFailed:
				// Not our failure and not the admin's: the far end refused.
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

	log.Printf("[IMPORT] scraped url=%q via=%s images=%d variants=%d price=%v",
		draft.SourceURL, draft.Extractor, len(draft.Images), len(draft.Variants), draft.Price)

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

// ImportImages records an import job and hands its images to the worker.
//
// Answers at once with the job, which the panel then polls. Every image is
// validated here -- a URL pointing inside the network is refused before it
// is ever fetched -- and each gets its object name now, so a retried message
// stores the same object rather than a second copy.
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
	if len(req.Images) > maxImagesPerJob {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"code":    "TOO_MANY_IMAGES",
			"message": fmt.Sprintf("That is %d images; one import handles up to %d.", len(req.Images), maxImagesPerJob),
		})
	}

	job := newImportJob(req, c)

	ctx, cancel := contextWithTimeout(c, 25*time.Second)
	defer cancel()

	if h.Queue == nil {
		// No worker to hand to -- local development. Do the work here and
		// return the job already finished, so the panel's polling loop sees
		// the same shape it would in production.
		h.storeInline(ctx, &job)
		job.Status = models.ImportJobCompleted
	}

	result, err := h.DB.Collections().ImageImportJobs.InsertOne(ctx, job)
	if err != nil {
		log.Printf("[IMPORT] could not record job: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "JOB_NOT_RECORDED",
			"message": "We could not start the import. Try again.",
			"detail":  err.Error(),
		})
	}
	job.ID = result.InsertedID.(primitive.ObjectID)

	if h.Queue != nil {
		var envelopes []queue.Envelope
		for index, image := range job.Images {
			if image.Status == models.ImportImagePending {
				envelopes = append(envelopes, queue.Envelope{
					Kind:  queue.KindImageImport,
					JobID: job.ID.Hex(),
					Index: index,
				})
			}
		}
		sent, err := h.Queue.SendAll(ctx, envelopes)
		if err != nil {
			// Whatever was not queued will never be processed. Say so on the
			// job, image by image, rather than leaving them pending forever.
			log.Printf("[IMPORT] job %s: queued %d of %d, then: %v", job.ID.Hex(), sent, len(envelopes), err)
			const reason = "could not be queued for storing; try the import again"
			for _, envelope := range envelopes[sent:] {
				h.markImage(ctx, job.ID, envelope.Index, bson.M{"status": models.ImportImageFailed, "reason": reason})
				job.Images[envelope.Index].Status = models.ImportImageFailed
				job.Images[envelope.Index].Reason = reason
			}
		}
		log.Printf("[IMPORT] job %s: %d image(s) queued for %q", job.ID.Hex(), sent, req.Name)
	}

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"success": true,
		"message": "Import started.",
		"data":    jobView(&job),
	})
}

// newImportJob builds the job document: one entry per distinct image, each
// named up front, with anything unsafe to fetch already marked failed.
func newImportJob(req importImagesRequest, c *fiber.Ctx) models.ImageImportJob {
	now := time.Now()
	slug := scrape.Slugify(req.Name)
	if slug == "" {
		slug = "product"
	}
	// The timestamp keeps two imports of the same watch from sharing names,
	// the way the upload path's own prefix does.
	prefix := fmt.Sprintf("%d-%s", now.UnixNano(), slug)

	job := models.ImageImportJob{
		CreatedAt: now,
		UpdatedAt: now,
		SourceURL: req.SourceURL,
		Name:      req.Name,
		Status:    models.ImportJobRunning,
		Images:    make([]models.ImportJobImage, 0, len(req.Images)),
	}
	if actor, ok := c.Locals("user").(*middleware.TokenMetadata); ok && actor != nil {
		job.CreatedBy = actor.UserID.Hex()
	}

	seen := map[string]bool{}
	for _, raw := range req.Images {
		key := scrape.ImageIdentity(raw)
		if seen[key] {
			continue
		}
		seen[key] = true

		image := models.ImportJobImage{
			SourceURL:  raw,
			ObjectName: fmt.Sprintf("%s-%d", prefix, len(job.Images)+1),
			Status:     models.ImportImagePending,
		}
		// Refused now rather than in the worker: an address inside the
		// network must never reach a fetch, and the admin should see why this
		// one will not appear, immediately.
		if _, err := scrape.ParseTarget(raw); err != nil {
			image.Status = models.ImportImageFailed
			image.Reason = err.Error()
		}
		job.Images = append(job.Images, image)
	}
	return job
}

// GetImportJob reports a job's progress and, as they land, its stored URLs.
//
// Completion is decided here rather than by the worker: the last image to
// finish has no cheap way of knowing it is last, but the reader can count.
func (h *ProductImportHandler) GetImportJob(c *fiber.Ctx) error {
	id, err := primitive.ObjectIDFromHex(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"code":    "JOB_NOT_FOUND",
			"message": "No such import.",
		})
	}

	ctx := c.UserContext()
	var job models.ImageImportJob
	if err := h.DB.Collections().ImageImportJobs.FindOne(ctx, bson.M{"_id": id}).Decode(&job); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"code":    "JOB_NOT_FOUND",
				"message": "No such import.",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "JOB_UNAVAILABLE",
			"message": "We could not read the import's progress.",
			"detail":  err.Error(),
		})
	}

	if _, _, pending := job.Counts(); pending == 0 && job.Status != models.ImportJobCompleted {
		job.Status = models.ImportJobCompleted
		_, _ = h.DB.Collections().ImageImportJobs.UpdateByID(ctx, id, bson.M{
			"$set": bson.M{"status": models.ImportJobCompleted, "updated_at": time.Now()},
		})
	}

	return c.JSON(fiber.Map{"success": true, "data": jobView(&job)})
}

// ListImportJobs returns the most recent imports, newest first, so the panel
// can show what is running and what just finished without keeping its own
// list -- which would be lost on reload and invisible to a second admin.
func (h *ProductImportHandler) ListImportJobs(c *fiber.Ctx) error {
	ctx := c.UserContext()
	cursor, err := h.DB.Collections().ImageImportJobs.Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(12))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "JOBS_UNAVAILABLE",
			"message": "We could not read recent imports.",
			"detail":  err.Error(),
		})
	}
	defer cursor.Close(ctx)

	var jobs []models.ImageImportJob
	if err := cursor.All(ctx, &jobs); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "JOBS_UNAVAILABLE",
			"message": "We could not read recent imports.",
			"detail":  err.Error(),
		})
	}

	views := make([]fiber.Map, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		if _, _, pending := job.Counts(); pending == 0 && job.Status != models.ImportJobCompleted {
			job.Status = models.ImportJobCompleted
		}
		view := jobView(job)
		view["name"] = job.Name
		view["createdAt"] = job.CreatedAt
		// The image list is the heavy part and the list view only needs counts.
		delete(view, "images")
		views = append(views, view)
	}
	return c.JSON(fiber.Map{"success": true, "data": views})
}

// storeInline does the worker's job in-process, for environments without one.
func (h *ProductImportHandler) storeInline(ctx context.Context, job *models.ImageImportJob) {
	client, err := h.Firebase.Client(ctx)
	if err != nil {
		for i := range job.Images {
			if job.Images[i].Status == models.ImportImagePending {
				job.Images[i].Status = models.ImportImageFailed
				job.Images[i].Reason = "image storage is not configured"
			}
		}
		return
	}
	for i := range job.Images {
		image := &job.Images[i]
		if image.Status != models.ImportImagePending {
			continue
		}
		stored, err := scrape.StoreImage(ctx, h.Fetcher, client, image.SourceURL, job.SourceURL, image.ObjectName)
		if err != nil {
			image.Status = models.ImportImageFailed
			image.Reason = err.Error()
			continue
		}
		image.Status = models.ImportImageStored
		image.URL = stored.URL
		image.Width = stored.Width
		image.Height = stored.Height
		image.Warning = stored.Warning
		h.Media.Note(stored.URL)
		for _, rendition := range stored.Renditions {
			h.Media.Note(rendition)
		}
	}
}

// markImage sets fields on one image of a job.
func (h *ProductImportHandler) markImage(ctx context.Context, jobID primitive.ObjectID, index int, fields bson.M) {
	set := bson.M{"updated_at": time.Now()}
	for key, value := range fields {
		set[fmt.Sprintf("images.%d.%s", index, key)] = value
	}
	if _, err := h.DB.Collections().ImageImportJobs.UpdateByID(ctx, jobID, bson.M{"$set": set}); err != nil {
		log.Printf("[IMPORT] job %s: could not mark image %d: %v", jobID.Hex(), index, err)
	}
}

// jobView is the job as the panel sees it: the images plus the counts it
// draws a progress figure from.
func jobView(job *models.ImageImportJob) fiber.Map {
	stored, failed, pending := job.Counts()
	urls := make([]string, 0, stored)
	for _, image := range job.Images {
		if image.Status == models.ImportImageStored && image.URL != "" {
			urls = append(urls, image.URL)
		}
	}
	return fiber.Map{
		"jobId":   job.ID.Hex(),
		"status":  job.Status,
		"total":   len(job.Images),
		"stored":  stored,
		"failed":  failed,
		"pending": pending,
		"images":  job.Images,
		"urls":    urls,
	}
}
