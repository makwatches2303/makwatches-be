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

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
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
	handler := NewProductImportHandler(&config.Config{ScrapeProxyTemplate: "https://reader.test/{url}"}, nil, nil)
	if handler.Scraper == nil || handler.Fetcher == nil {
		t.Fatal("handler was built without a scraper or fetcher")
	}
}
