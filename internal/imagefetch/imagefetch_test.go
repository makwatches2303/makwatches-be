package imagefetch

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encodeJPEG returns bytes for a solid-color JPEG of the given size --
// enough for image.DecodeConfig to report real dimensions without needing
// a network fetch.
func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encoding test JPEG: %v", err)
	}
	return buf.Bytes()
}

func writeTempImage(t *testing.T, w, h int, ext string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-image"+ext)
	if err := os.WriteFile(path, encodeJPEG(t, w, h), 0o644); err != nil {
		t.Fatalf("writing temp image: %v", err)
	}
	return path
}

func TestFetch_RejectsBelowMinimumResolution(t *testing.T) {
	// This is the exact class of bug this package exists to prevent: a
	// 500x500 thumbnail (Amazon's altImages strip size) that would have
	// shipped as a blurry "product photo" without this check.
	path := writeTempImage(t, 500, 500, ".jpg")

	_, err := Fetch(context.Background(), "file://"+path)
	if err == nil {
		t.Fatal("expected a 500x500 image to be rejected as too small, got no error")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("expected a 'too small' error, got: %v", err)
	}
}

func TestFetch_AcceptsAtOrAboveMinimumResolution(t *testing.T) {
	path := writeTempImage(t, MinWidth, MinHeight, ".jpg")

	res, err := Fetch(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("expected a %dx%d image to be accepted, got error: %v", MinWidth, MinHeight, err)
	}
	if res.Width != MinWidth || res.Height != MinHeight {
		t.Errorf("expected decoded dimensions %dx%d, got %dx%d", MinWidth, MinHeight, res.Width, res.Height)
	}
	if len(res.Body) == 0 {
		t.Error("expected non-empty image body")
	}
}

func TestFetch_RejectsNonImageContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-an-image.jpg")
	if err := os.WriteFile(path, []byte("this is not image data"), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	if _, err := Fetch(context.Background(), "file://"+path); err == nil {
		t.Fatal("expected non-image content to be rejected, got no error")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fastrack Hype Adventure!":     "fastrack-hype-adventure",
		"  Leading/trailing spaces  ":  "leading-trailing-spaces",
		"Multiple---dashes___collapse": "multiple-dashes-collapse",
		"Titan Karishma (Black Dial)":  "titan-karishma-black-dial",
	}
	for input, want := range cases {
		if got := Slugify(input); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", input, got, want)
		}
	}
}
