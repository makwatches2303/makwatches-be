// Command image-demo writes the catalogue's renditions for one image so the
// result can be looked at before the pipeline is trusted with the catalogue.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/imageproc"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: image-demo <url|file> <out-dir>")
		os.Exit(2)
	}
	src, outDir := os.Args[1], os.Args[2]

	var body []byte
	var err error
	if len(src) > 4 && src[:4] == "http" {
		req, _ := http.NewRequest("GET", src, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
		res, e := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		defer res.Body.Close()
		body, err = io.ReadAll(res.Body)
	} else {
		body, err = os.ReadFile(src)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	originalPath := filepath.Join(outDir, "original.jpg")
	_ = os.WriteFile(originalPath, body, 0o644)

	start := time.Now()
	renditions, err := imageproc.Renditions(body)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("original          %7.1f KB\n", float64(len(body))/1024)
	for _, r := range renditions {
		path := filepath.Join(outDir, fmt.Sprintf("%dw.jpg", r.Width))
		_ = os.WriteFile(path, r.Bytes, 0o644)
		fmt.Printf("%-4dx%-4d %s  %7.1f KB  (%.0f%% of original)\n",
			r.Width, r.Height, r.Suffix, float64(len(r.Bytes))/1024,
			100*float64(len(r.Bytes))/float64(len(body)))
	}
	fmt.Printf("\nall renditions produced in %v\n", elapsed.Round(time.Millisecond))
}
