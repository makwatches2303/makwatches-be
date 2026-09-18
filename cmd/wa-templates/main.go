// Command wa-templates prints the WhatsApp templates FlowSell reports for the
// configured account -- the same list the admin panel's pickers are built from.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	templates, err := whatsapp.NewClient(cfg).ListTemplates(ctx, true)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range templates {
		fmt.Printf("%-40s %-6s %-9s %-10s header=%-6s vars=%d\n", t.Name, t.Language, t.Status, t.Category, t.HeaderFormat, len(t.Variables))
	}
}
