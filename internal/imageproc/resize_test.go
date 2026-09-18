package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// detailedPhoto builds an image with fine detail at every scale, so a resize
// that loses sharpness or an encoder set too low shows up as a size collapse
// rather than passing on a flat test picture that compresses to nothing.
func detailedPhoto(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// Texture plus noise: like a brushed-metal case under studio light.
			base := uint8((x*7 + y*13) % 256)
			noise := uint8(rng.Intn(48))
			img.Set(x, y, color.RGBA{R: base, G: base/2 + noise, B: 255 - base, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRenditionsProducesSmallerSizes(t *testing.T) {
	src := detailedPhoto(t, 1500, 1500)

	renditions, err := Renditions(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(renditions) == 0 {
		t.Fatal("a 1500px photograph produced no renditions")
	}

	for _, r := range renditions {
		if r.Width >= 1500 {
			t.Errorf("rendition %dw is not smaller than the source", r.Width)
		}
		if r.Height != r.Width {
			t.Errorf("rendition %dx%d lost the square aspect ratio", r.Width, r.Height)
		}
		if len(r.Bytes) == 0 {
			t.Errorf("rendition %dw is empty", r.Width)
		}
		// Every rendition must decode: a file that only looks like an image
		// is worse than no rendition, because the read path would serve it.
		decoded, err := jpeg.Decode(bytes.NewReader(r.Bytes))
		if err != nil {
			t.Errorf("rendition %dw does not decode: %v", r.Width, err)
			continue
		}
		if decoded.Bounds().Dx() != r.Width {
			t.Errorf("rendition %dw decodes at %dpx", r.Width, decoded.Bounds().Dx())
		}
	}
}

func TestRenditionsNeverUpscale(t *testing.T) {
	// A 300px source is below every rendition size. Inventing pixels makes a
	// bigger file and no better picture.
	renditions, err := Renditions(detailedPhoto(t, 300, 300))
	if err != nil {
		t.Fatal(err)
	}
	if len(renditions) != 0 {
		t.Fatalf("a 300px source produced %d renditions; it should produce none", len(renditions))
	}
}

func TestRenditionsSkipsAnythingNotSmaller(t *testing.T) {
	// A source already compressed hard -- which is what a marketplace serves --
	// can re-encode larger at our quality. Storing that would cost bytes to
	// serve a bigger file that has also been through a second generation of
	// JPEG loss.
	img := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	rng := rand.New(rand.NewSource(2))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1000; x++ {
			v := uint8(rng.Intn(256))
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var tiny bytes.Buffer
	if err := jpeg.Encode(&tiny, img, &jpeg.Options{Quality: 5}); err != nil {
		t.Fatal(err)
	}

	renditions, err := Renditions(tiny.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range renditions {
		if len(r.Bytes) >= len(tiny.Bytes()) {
			t.Errorf("kept a %dw rendition of %d bytes against a %d byte source",
				r.Width, len(r.Bytes), len(tiny.Bytes()))
		}
	}
}

func TestRenditionsKeepTransparencyAsPNG(t *testing.T) {
	// A cut-out product shot on a transparent background: flattened into JPEG
	// it gains a white box wherever the page is not white.
	img := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 1200; x++ {
			if x > 300 && x < 900 && y > 300 && y < 900 {
				img.Set(x, y, color.RGBA{R: 200, G: 40, B: 40, A: 255})
			} else {
				img.Set(x, y, color.RGBA{})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	renditions, err := Renditions(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(renditions) == 0 {
		t.Fatal("no renditions for a 1200px cut-out")
	}
	for _, r := range renditions {
		if r.ContentType != "image/png" {
			t.Errorf("rendition %dw is %s; a transparent source must stay PNG", r.Width, r.ContentType)
		}
		decoded, err := png.Decode(bytes.NewReader(r.Bytes))
		if err != nil {
			t.Fatalf("rendition %dw does not decode as PNG: %v", r.Width, err)
		}
		if _, _, _, a := decoded.At(5, 5).RGBA(); a == 0xffff {
			t.Errorf("rendition %dw lost its transparency", r.Width)
		}
	}
}

func TestRenditionsRejectsWhatIsNotAnImage(t *testing.T) {
	if _, err := Renditions([]byte("this is not an image")); err == nil {
		t.Fatal("a text file was accepted as an image")
	}
}

func TestRenditionName(t *testing.T) {
	cases := []struct{ original, suffix, contentType, want string }{
		{"1789-fastrack-uptown-1.jpg", "-400w", "image/jpeg", "1789-fastrack-uptown-1-400w.jpg"},
		{"shot.png", "-800w", "image/png", "shot-800w.png"},
		// A re-encoded PNG source becomes JPEG, and the name has to follow the
		// bytes rather than the original's extension.
		{"shot.png", "-400w", "image/jpeg", "shot-400w.jpg"},
		{"no-extension", "-400w", "image/jpeg", "no-extension-400w.jpg"},
	}
	for _, c := range cases {
		if got := RenditionName(c.original, c.suffix, c.contentType); got != c.want {
			t.Errorf("RenditionName(%q, %q, %q) = %q, want %q",
				c.original, c.suffix, c.contentType, got, c.want)
		}
	}
}
