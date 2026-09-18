// Package imageproc prepares an uploaded photograph for the catalogue.
//
// One upload becomes several renditions -- a thumbnail for the grid, a mid
// size for the product page, and the full-size original for the zoom viewer.
// This exists because image optimization at the edge was switched off (the
// Vercel quota is exhausted; see the storefront's next.config.ts), so whatever
// the browser is given is what it downloads. Without this, a listing page of
// twenty watches ships twenty 1500px originals to fill twenty 200px squares.
//
// Quality is the constraint, not a target to trade away: these are the
// photographs a customer decides to spend money on. So the resampling filter
// is Catmull-Rom rather than the cheap nearest/bilinear, JPEG quality is held
// at a level where the difference from the original is not visible at 100%,
// and the original is always kept untouched alongside the renditions.
package imageproc

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"

	// Decoders for what the catalogue accepts. Registered for side effects.
	_ "image/gif"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Rendition sizes.
//
// Chosen from how the storefront actually lays out: the product grid draws at
// roughly 300-400 CSS px (so 400 covers 1x and 800 covers a 2x screen), the
// product page's main image at 600-700 (so 1200 covers it at 2x), and the zoom
// viewer wants everything there is, which is the original.
const (
	ThumbWidth  = 400
	MediumWidth = 800
	LargeWidth  = 1200
)

// JPEG quality for each rendition.
//
// 88 is deliberately above the usual 75-80 default. At 400-1200px on a watch
// photograph -- fine bracelet links, brushed-metal gradients, printed dial
// text -- 75 shows ringing around the text and banding on the case, and it is
// exactly the kind of degradation nobody notices in review and everybody
// notices on a product page. The extra bytes are worth it; even at 88 a
// rendition is a fraction of the original's weight.
const (
	qualityThumb  = 86
	qualityMedium = 88
	qualityLarge  = 88
)

// A Rendition is one prepared size of an uploaded image.
type Rendition struct {
	// Suffix is appended to the object's base name, e.g. "-400w".
	Suffix string
	Width  int
	Height int
	Bytes  []byte
	// ContentType is always image/jpeg for renditions: every rendition is
	// re-encoded, and JPEG is the one format every browser has always read.
	ContentType string
}

// Renditions returns the smaller sizes to store alongside an original.
//
// Never upscales: an 800px upload gets a 400px thumbnail and nothing else,
// because inventing pixels makes a file bigger and the picture no better.
// Returns an error only when the bytes are not a decodable image; a source too
// small for any rendition is a normal result of no renditions.
func Renditions(src []byte) ([]Rendition, error) {
	decoded, format, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("that file is not an image we can read: %w", err)
	}

	bounds := decoded.Bounds()
	srcWidth, srcHeight := bounds.Dx(), bounds.Dy()
	if srcWidth <= 0 || srcHeight <= 0 {
		return nil, fmt.Errorf("that image has no size")
	}

	// A photograph with transparency is a cut-out on a transparent background;
	// flattening it onto white in JPEG would put a white box behind it
	// wherever the page is not white. Those stay PNG.
	keepPNG := format == "png" && hasTransparency(decoded)

	wanted := []struct {
		width   int
		quality int
		suffix  string
	}{
		{ThumbWidth, qualityThumb, fmt.Sprintf("-%dw", ThumbWidth)},
		{MediumWidth, qualityMedium, fmt.Sprintf("-%dw", MediumWidth)},
		{LargeWidth, qualityLarge, fmt.Sprintf("-%dw", LargeWidth)},
	}

	out := make([]Rendition, 0, len(wanted))
	for _, size := range wanted {
		if size.width >= srcWidth {
			// Already at or above the source: the original serves this size.
			continue
		}
		height := srcHeight * size.width / srcWidth
		if height < 1 {
			height = 1
		}

		resized := image.NewRGBA(image.Rect(0, 0, size.width, height))
		// Catmull-Rom: a cubic filter that keeps edges crisp where a bilinear
		// scale goes soft. Watch photography is mostly fine detail -- bracelet
		// links, indices, printed text on the dial -- and softness there reads
		// as a cheap photograph.
		draw.CatmullRom.Scale(resized, resized.Bounds(), decoded, bounds, draw.Over, nil)

		encoded, contentType, err := encode(resized, keepPNG, size.quality)
		if err != nil {
			return nil, err
		}

		// A rendition that is not meaningfully smaller than what it replaces
		// is not worth storing or serving. This happens more often than it
		// sounds: a marketplace original is already compressed hard, so a
		// 1200px re-encode at our quality can come out *larger* than the
		// 1500px source -- we would then be paying storage to serve a bigger
		// file that also went through a second generation of JPEG loss.
		if len(encoded) >= len(src)*85/100 {
			continue
		}

		out = append(out, Rendition{
			Suffix:      size.suffix,
			Width:       size.width,
			Height:      height,
			Bytes:       encoded,
			ContentType: contentType,
		})
	}

	return out, nil
}

func encode(img image.Image, keepPNG bool, quality int) ([]byte, string, error) {
	var buf bytes.Buffer
	if keepPNG {
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("could not write the resized image: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, "", fmt.Errorf("could not write the resized image: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// hasTransparency reports whether any pixel is not fully opaque.
//
// Sampled rather than exhaustive: a cut-out product shot is transparent around
// its whole border, so a grid of samples finds it, and a full scan of a
// 3000x3000 image on every upload is nine million reads for a question a few
// hundred can answer.
func hasTransparency(img image.Image) bool {
	bounds := img.Bounds()
	const samples = 40
	stepX := max(bounds.Dx()/samples, 1)
	stepY := max(bounds.Dy()/samples, 1)

	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

// RenditionName builds the object name for a rendition from the original's.
//
// "watch-1.jpg" + "-400w" becomes "watch-1-400w.jpg", so the whole set sorts
// together in the bucket and the relationship is readable by eye.
func RenditionName(original, suffix, contentType string) string {
	extension := ".jpg"
	if contentType == "image/png" {
		extension = ".png"
	}

	base := original
	if dot := strings.LastIndex(original, "."); dot > 0 {
		base = original[:dot]
	}
	return base + suffix + extension
}

// Uploader stores an object under exactly the name it is given and returns its
// public URL. Implemented by the Firebase client; an interface so this package
// never depends on a particular bucket.
//
// The exact name matters: a rendition is found by appending "-400w" to the
// original's object name, so one stored under any other name -- a timestamped
// one, say -- is invisible to every reader and the grid keeps being served
// full-size originals.
type Uploader interface {
	UploadObject(ctx context.Context, file io.Reader, objectName string) (string, error)
}

// Store writes an image's renditions beside it and returns them keyed by width.
//
// Best effort throughout: a product is perfectly serviceable with only its
// original, so neither a resize nor an upload failure is worth failing an
// upload for. The read path checks whether a rendition exists before pointing
// anyone at one, so a missing rendition is simply never used.
func Store(ctx context.Context, uploader Uploader, original []byte, name string) map[string]string {
	renditions, err := Renditions(original)
	if err != nil {
		log.Printf("[IMAGEPROC] could not resize %s: %v", name, err)
		return nil
	}
	if len(renditions) == 0 {
		return nil
	}

	stored := make(map[string]string, len(renditions))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, rendition := range renditions {
		wg.Add(1)
		go func(r Rendition) {
			defer wg.Done()
			renditionName := RenditionName(name, r.Suffix, r.ContentType)
			url, err := uploader.UploadObject(ctx, bytes.NewReader(r.Bytes), renditionName)
			if err != nil {
				log.Printf("[IMAGEPROC] could not store rendition %s: %v", renditionName, err)
				return
			}
			mu.Lock()
			stored[strconv.Itoa(r.Width)] = url
			mu.Unlock()
		}(rendition)
	}
	wg.Wait()

	return stored
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
