package scrape

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"regexp"
	"strings"

	// Registered for their decoders only: this package never renders, it only
	// needs to know that the bytes really are an image and how big it is.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Uploader stores an image and returns its public URL. Implemented by the
// Firebase client; an interface so this package can be tested without a
// bucket, and so the storage backend stays swappable.
type Uploader interface {
	UploadFile(ctx context.Context, file io.Reader, filename string) (string, error)
}

// Import limits.
const (
	// maxImageBytes is above every real product photograph seen so far and
	// well under Lambda's memory headroom for a handful of them.
	maxImageBytes = 12 << 20
	// minImageEdge rejects thumbnails outright. A thumbnail cannot be
	// upscaled by asking the CDN for a bigger size (Amazon's strip images are
	// a separate, smaller asset), so one that arrives small stays small and
	// ships as a visibly blurry product photo -- which this catalogue has
	// done once already. See internal/imagefetch's package comment.
	minImageEdge = 500
	// preferredEdge is the catalogue's standard. Between this and
	// minImageEdge the image is accepted with a warning: better an admin
	// decides than an import silently drops the only photo a page had.
	preferredEdge = 800
	// maxImagesPerImport caps a single approval.
	maxImagesPerImport = 12
)

// ImportedImage is one image copied into our own storage.
type ImportedImage struct {
	// SourceURL is where it came from, so the panel can map results back to
	// what the admin selected.
	SourceURL string `json:"sourceUrl"`
	URL       string `json:"url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	// Warning is set when the image was accepted but is below the
	// catalogue's preferred size.
	Warning string `json:"warning,omitempty"`
}

// RejectedImage is one that could not be imported, with the reason in words
// an admin can act on.
type RejectedImage struct {
	SourceURL string `json:"sourceUrl"`
	Reason    string `json:"reason"`
}

// ImportImages copies images into our own storage.
//
// Product images are never hot-linked from the source. A marketplace CDN can
// change or remove an asset at any time, often blocks requests that carry no
// referer of its own, and a catalogue whose photographs live on a competitor's
// server is one policy change away from an empty shop. Copying also means the
// bytes have been decoded here at least once, so what reaches the bucket is an
// image and not a renamed archive.
//
// Failures are reported per image rather than aborting: one dead URL out of
// six must not cost the admin the other five.
func ImportImages(ctx context.Context, fetcher *Fetcher, uploader Uploader, urls []string, referer, nameHint string) ([]ImportedImage, []RejectedImage) {
	if fetcher == nil {
		fetcher = NewFetcher()
	}

	var imported []ImportedImage
	var rejected []RejectedImage

	slug := slugify(nameHint)
	if slug == "" {
		slug = "product"
	}

	seen := make(map[string]bool, len(urls))
	for index, raw := range urls {
		if len(imported) >= maxImagesPerImport {
			rejected = append(rejected, RejectedImage{
				SourceURL: raw,
				Reason:    fmt.Sprintf("Only %d images can be imported at once.", maxImagesPerImport),
			})
			continue
		}
		key := imageIdentity(raw)
		if seen[key] {
			continue
		}
		seen[key] = true

		result, err := importOne(ctx, fetcher, uploader, raw, referer, fmt.Sprintf("%s-%d", slug, index+1))
		if err != nil {
			rejected = append(rejected, RejectedImage{SourceURL: raw, Reason: err.Error()})
			continue
		}
		imported = append(imported, *result)
	}

	return imported, rejected
}

func importOne(ctx context.Context, fetcher *Fetcher, uploader Uploader, raw, referer, basename string) (*ImportedImage, error) {
	page, err := fetcher.GetBinary(ctx, raw, maxImageBytes, referer)
	if err != nil {
		return nil, fmt.Errorf("could not download: %w", err)
	}
	if page.Status != 200 {
		return nil, fmt.Errorf("the image server answered %d", page.Status)
	}

	// Decode before trusting anything the server said about the file. A
	// Content-Type header is a claim; a successful decode is evidence.
	config, format, err := image.DecodeConfig(bytes.NewReader(page.Body))
	if err != nil {
		return nil, fmt.Errorf("that file is not an image we can read")
	}
	if config.Width < minImageEdge || config.Height < minImageEdge {
		return nil, fmt.Errorf("too small at %dx%d -- that is a thumbnail, not a product photo", config.Width, config.Height)
	}

	extension := "." + format
	if format == "jpeg" {
		extension = ".jpg"
	}

	url, err := uploader.UploadFile(ctx, bytes.NewReader(page.Body), basename+extension)
	if err != nil {
		return nil, fmt.Errorf("could not store the image: %w", err)
	}

	result := &ImportedImage{SourceURL: raw, URL: url, Width: config.Width, Height: config.Height}
	if config.Width < preferredEdge || config.Height < preferredEdge {
		result.Warning = fmt.Sprintf("Only %dx%d; below the %dpx the catalogue prefers.", config.Width, config.Height, preferredEdge)
	}
	return result, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a product name into a filename stem, so objects in the bucket
// are identifiable rather than a wall of timestamps.
func slugify(s string) string {
	slug := nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	return slug
}
