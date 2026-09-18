package scrape

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"regexp"
	"strings"
	"sync"

	// Registered for their decoders only: this package never renders, it only
	// needs to know that the bytes really are an image and how big it is.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/shivam-mishra-20/mak-watches-be/internal/imageproc"
)

// Uploader stores an image and returns its public URL. Implemented by the
// Firebase client; an interface so this package can be tested without a
// bucket, and so the storage backend stays swappable.
//
// UploadFile names the object itself (it adds a timestamp, so two imports of
// "watch-1.jpg" cannot collide); UploadObject stores under exactly the name
// given, which is what the derived rendition names require.
type Uploader interface {
	UploadFile(ctx context.Context, file io.Reader, filename string) (string, error)
	imageproc.Uploader
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
	// importConcurrency is how many images are fetched, resized and stored at
	// once. Each one is mostly waiting -- on the source CDN, then on the
	// bucket -- so running them one after another made a twelve-photo import
	// take the best part of a minute while the admin watched a spinner. Eight
	// is comfortably within what both ends serve without complaint, and keeps
	// a whole gallery inside the API gateway's own timeout.
	importConcurrency = 8
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
	// Renditions are the smaller sizes stored beside the original, keyed by
	// width. Absent when the source was already small enough, or when
	// resizing failed -- which is not an error: the original always works.
	Renditions map[string]string `json:"renditions,omitempty"`
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

	slug := slugify(nameHint)
	if slug == "" {
		slug = "product"
	}

	// Deduplicate and cap before doing any work, so the concurrent pass below
	// is a straight map over a settled list.
	type job struct {
		index    int
		url      string
		basename string
	}
	var jobs []job
	rejected := make([]RejectedImage, 0)
	seen := make(map[string]bool, len(urls))

	for index, raw := range urls {
		key := imageIdentity(raw)
		if seen[key] {
			continue
		}
		seen[key] = true

		if len(jobs) >= maxImagesPerImport {
			rejected = append(rejected, RejectedImage{
				SourceURL: raw,
				Reason:    fmt.Sprintf("Only %d images can be imported at once.", maxImagesPerImport),
			})
			continue
		}
		jobs = append(jobs, job{index: index, url: raw, basename: fmt.Sprintf("%s-%d", slug, index+1)})
	}

	// Results are collected by position, not by whichever goroutine finishes
	// first: the order the admin arranged the photographs in is the order the
	// gallery is stored in, and the first one is the product's main image.
	results := make([]*ImportedImage, len(jobs))
	failures := make([]*RejectedImage, len(jobs))

	var wg sync.WaitGroup
	tokens := make(chan struct{}, importConcurrency)

	for i, j := range jobs {
		wg.Add(1)
		go func(slot int, j job) {
			defer wg.Done()
			tokens <- struct{}{}
			defer func() { <-tokens }()

			// One image failing must not take the others with it, and a panic
			// in a decoder on a malformed file must not take the process down.
			defer func() {
				if r := recover(); r != nil {
					failures[slot] = &RejectedImage{SourceURL: j.url, Reason: "that image could not be processed"}
				}
			}()

			result, err := importOne(ctx, fetcher, uploader, j.url, referer, j.basename)
			if err != nil {
				failures[slot] = &RejectedImage{SourceURL: j.url, Reason: err.Error()}
				return
			}
			results[slot] = result
		}(i, j)
	}
	wg.Wait()

	imported := make([]ImportedImage, 0, len(jobs))
	for i := range jobs {
		if results[i] != nil {
			imported = append(imported, *results[i])
		}
		if failures[i] != nil {
			rejected = append(rejected, *failures[i])
		}
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
	name := basename + extension

	url, err := uploader.UploadFile(ctx, bytes.NewReader(page.Body), name)
	if err != nil {
		return nil, fmt.Errorf("could not store the image: %w", err)
	}

	result := &ImportedImage{SourceURL: raw, URL: url, Width: config.Width, Height: config.Height}
	if config.Width < preferredEdge || config.Height < preferredEdge {
		result.Warning = fmt.Sprintf("Only %dx%d; below the %dpx the catalogue prefers.", config.Width, config.Height, preferredEdge)
	}

	// Smaller sizes for the grid, stored beside the original under a derived
	// name. Best effort on purpose: the product is perfectly serviceable with
	// only its original, so a resize that fails must not fail the import. The
	// read path checks whether a rendition exists before using one.
	// Renditions are named after the object the bucket actually stored, not
	// after the name we asked for: the two differ, since UploadFile adds a
	// timestamp, and a rendition named after the wrong one is never found.
	result.Renditions = imageproc.Store(ctx, uploader, page.Body, objectNameOf(url))

	return result, nil
}

// objectNameOf recovers the stored object name from a bucket URL.
func objectNameOf(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
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
