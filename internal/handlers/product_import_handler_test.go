package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/scrape"
)

// importApp mounts the import handler with no authentication, so these tests
// exercise the handler itself; that the real routes are behind the admin guard
// is asserted separately in routes_test.go.
func importApp(t *testing.T) *fiber.App {
	t.Helper()

	app := fiber.New()
	handler := &ProductImportHandler{
		Scraper: scrape.NewService(""),
		Fetcher: scrape.NewFetcher(),
	}
	app.Post("/scrape", handler.ScrapeProduct)
	return app
}

func postImportJSON(t *testing.T, app *fiber.App, path string, body any) (int, map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

const pastedProductPage = `<html><head>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"Product","name":"Fastrack Uptown Retreat Analog Watch",
 "brand":{"@type":"Brand","name":"Fastrack"},
 "image":["https://cdn.example.com/watch-1.jpg"],
 "additionalProperty":[{"name":"Dial Colour","value":"Brown"},{"name":"Strap Material","value":"Metal"}],
 "offers":{"@type":"Offer","price":"4395","priceCurrency":"INR"}}
</script></head><body><h1>Fastrack Uptown Retreat</h1></body></html>`

// A pasted page is the path for sites that refuse a server outright, and it
// must go through the same extractors and produce the same shape of draft as a
// fetched one -- including the attribute guesses the admin then confirms.
func TestScrapeProductReadsPastedPageSource(t *testing.T) {
	app := importApp(t)

	status, body := postImportJSON(t, app, "/scrape", fiber.Map{
		"url":  "https://www.fastrack.in/product/3254sl01/",
		"html": pastedProductPage,
	})
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}

	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("no draft in response: %v", body)
	}
	if data["name"] != "Fastrack Uptown Retreat Analog Watch" {
		t.Errorf("name = %v", data["name"])
	}
	if data["brand"] != "Fastrack" {
		t.Errorf("brand = %v", data["brand"])
	}
	if data["price"] != float64(4395) {
		t.Errorf("price = %v", data["price"])
	}
	attributes, _ := data["attributes"].(map[string]any)
	if attributes["dialColor"] != "Brown" || attributes["strapMaterial"] != "Metal" {
		t.Errorf("attributes = %v", attributes)
	}
}

// An empty request must not reach the network, and must say which field is
// missing rather than answering with a bare 400.
func TestScrapeProductRefusesAnEmptyRequest(t *testing.T) {
	app := importApp(t)

	status, body := postImportJSON(t, app, "/scrape", fiber.Map{"url": "  "})
	if status != fiber.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["code"] != "INVALID_REQUEST" {
		t.Errorf("code = %v", body["code"])
	}
	if _, ok := body["fieldErrors"].(map[string]any); !ok {
		t.Errorf("no fieldErrors in %v", body)
	}
}

// The destination is chosen by whoever is signed in, and this process can
// reach the instance metadata endpoint and the database. A URL pointing inside
// the network has to be refused before any request is made.
func TestScrapeProductRefusesAnAddressInsideTheNetwork(t *testing.T) {
	app := importApp(t)

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:27017/",
		"file:///etc/passwd",
	} {
		status, body := postImportJSON(t, app, "/scrape", fiber.Map{"url": target})
		if status < 400 {
			t.Errorf("POST /scrape %q = %d (%v); it must be refused", target, status, body)
			continue
		}
		code, _ := body["code"].(string)
		if code != scrape.CodeBlockedAddress && code != scrape.CodeInvalidURL {
			t.Errorf("POST /scrape %q returned code %q, want a refusal code", target, code)
		}
	}
}

// recordingUploader stands in for Firebase Storage.
type recordingUploader struct {
	names []string
}

func (u *recordingUploader) UploadObject(ctx context.Context, file io.Reader, objectName string) (string, error) {
	return u.UploadFile(ctx, file, objectName)
}

func (u *recordingUploader) UploadFile(ctx context.Context, file io.Reader, filename string) (string, error) {
	body, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		t := "empty upload"
		return "", &uploadError{message: t}
	}
	u.names = append(u.names, filename)
	return "https://storage.googleapis.com/bucket/" + filename, nil
}

type uploadError struct{ message string }

func (e *uploadError) Error() string { return e.message }

// An image that cannot be reached must be reported per image rather than
// failing the whole import: one dead URL out of six must not cost the admin
// the other five.
func TestImportImagesReportsPerImageFailures(t *testing.T) {
	uploader := &recordingUploader{}
	imported, rejected := scrape.ImportImages(
		context.Background(),
		scrape.NewFetcher(),
		uploader,
		[]string{"http://127.0.0.1:9/never.jpg"},
		"https://example.com/p",
		"Some Watch",
	)
	if len(imported) != 0 {
		t.Errorf("imported = %v, want none", imported)
	}
	if len(rejected) != 1 || rejected[0].Reason == "" {
		t.Fatalf("rejected = %v, want one entry with a reason", rejected)
	}
}

func TestNewProductImportHandlerUsesConfiguredProxy(t *testing.T) {
	handler := NewProductImportHandler(&config.Config{ScrapeProxyTemplate: "https://reader.test/{url}"}, nil, nil, nil, nil)
	if handler.Scraper == nil || handler.Fetcher == nil {
		t.Fatal("handler was built without a scraper or fetcher")
	}
}

// The job is where every image is named and vetted, before anything is
// fetched: an address inside the network is refused here, the same
// photograph at two sizes is one image, and each gets the object name it
// will be stored under so a retried message never makes a second copy.
func TestNewImportJobNamesAndVetsEveryImage(t *testing.T) {
	app := fiber.New()
	c := app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(c)

	job := newImportJob(importImagesRequest{
		Name:      "Noise Pulse 2 Max",
		SourceURL: "https://www.amazon.in/dp/B0B6BPTFT5",
		Images: []string{
			"https://m.media-amazon.com/images/I/71abc._AC_SX679_.jpg",
			"https://m.media-amazon.com/images/I/71abc._AC_SL1500_.jpg", // same photo, other size
			"http://169.254.169.254/latest/meta-data/",
			"https://m.media-amazon.com/images/I/82def.jpg",
		},
	}, c)

	if len(job.Images) != 3 {
		t.Fatalf("got %d images, want 3 (one duplicate folded): %+v", len(job.Images), job.Images)
	}
	if job.Status != models.ImportJobRunning {
		t.Errorf("status = %q, want running", job.Status)
	}

	seen := map[string]bool{}
	for i, image := range job.Images {
		if image.ObjectName == "" || seen[image.ObjectName] {
			t.Errorf("image %d has no distinct object name: %q", i, image.ObjectName)
		}
		seen[image.ObjectName] = true
		if !strings.Contains(image.ObjectName, "noise-pulse-2-max") {
			t.Errorf("object name %q does not carry the product's name", image.ObjectName)
		}
	}

	metadata := job.Images[1]
	if metadata.Status != models.ImportImageFailed || metadata.Reason == "" {
		t.Errorf("the metadata address was not refused up front: %+v", metadata)
	}
	if job.Images[0].Status != models.ImportImagePending || job.Images[2].Status != models.ImportImagePending {
		t.Errorf("ordinary images should be pending: %+v", job.Images)
	}

	stored, failed, pending := job.Counts()
	if stored != 0 || failed != 1 || pending != 2 {
		t.Errorf("counts = %d/%d/%d, want 0 stored, 1 failed, 2 pending", stored, failed, pending)
	}
}

// The view is what the panel polls: its counts and its URL list must agree
// with the images they summarise, and a job with nothing pending reads as
// complete once the reader marks it so.
func TestJobViewSummarisesTheImages(t *testing.T) {
	job := &models.ImageImportJob{
		Status: models.ImportJobRunning,
		Images: []models.ImportJobImage{
			{SourceURL: "a", Status: models.ImportImageStored, URL: "https://bucket/a.jpg"},
			{SourceURL: "b", Status: models.ImportImageFailed, Reason: "too small"},
			{SourceURL: "c", Status: models.ImportImagePending},
		},
	}
	view := jobView(job)
	if view["total"] != 3 || view["stored"] != 1 || view["failed"] != 1 || view["pending"] != 1 {
		t.Errorf("view counts = %v", view)
	}
	urls, _ := view["urls"].([]string)
	if len(urls) != 1 || urls[0] != "https://bucket/a.jpg" {
		t.Errorf("urls = %v, want only the stored one", urls)
	}
}
