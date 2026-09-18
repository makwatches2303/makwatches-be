// Command scrape-check reads a product URL with internal/scrape and prints the
// draft, for checking an extractor against a real page without deploying.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/scrape"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: scrape-check <url> [proxyTemplate]")
		os.Exit(2)
	}
	proxy := ""
	if len(os.Args) > 2 {
		proxy = os.Args[2]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	draft, err := scrape.NewService(proxy).Scrape(ctx, os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrape failed: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(draft, "", "  ")
	fmt.Println(string(out))
}
