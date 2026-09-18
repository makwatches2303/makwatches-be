package scrape

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// extractMeta reads the social-sharing tags every shop emits.
//
// This is the floor, not the goal: og:title and og:image exist so a link
// pasted into WhatsApp looks right, which means they are maintained even on
// sites whose markup is otherwise unreadable. A draft built from them has a
// name and a hero image and nothing else, and that is still a better start
// than an empty form.
func extractMeta(doc *html.Node, base *url.URL) *Draft {
	draft := &Draft{Extractor: "opengraph"}

	draft.Name = firstNonEmpty(
		metaContent(doc, "og:title"),
		metaContent(doc, "twitter:title"),
		titleText(doc),
	)
	draft.Description = cleanHTMLText(firstNonEmpty(
		metaContent(doc, "og:description"),
		metaContent(doc, "twitter:description"),
		metaContent(doc, "description"),
	))
	draft.Brand = firstNonEmpty(
		metaContent(doc, "product:brand"),
		metaContent(doc, "og:brand"),
		metaContent(doc, "brand"),
	)
	draft.Price = ParsePrice(firstNonEmpty(
		metaContent(doc, "product:price:amount"),
		metaContent(doc, "og:price:amount"),
		metaContent(doc, "twitter:data1"),
	))
	draft.Currency = firstNonEmpty(
		metaContent(doc, "product:price:currency"),
		metaContent(doc, "og:price:currency"),
	)

	images := append(metaContents(doc, "og:image"), metaContents(doc, "twitter:image")...)
	images = append(images, metaContents(doc, "og:image:secure_url")...)
	for _, image := range images {
		draft.Images = append(draft.Images, UpgradeImageURL(absolute(base, image)))
	}

	// An <h1> is a better product name than a page title, which usually
	// carries the shop's name and a pipe.
	for _, h1 := range elementsByTag(doc, "h1") {
		if text := textOf(h1); len(text) > 3 && len(text) < 200 {
			draft.Name = text
			break
		}
	}

	if draft.Empty() {
		return nil
	}
	return draft
}

// flipkartStatePair matches one specification row inside the page state
// Flipkart embeds as JSON. The two halves appear in either order, so both
// spellings are matched.
var flipkartStatePair = regexp.MustCompile(
	`"label_1":\{"value":\{"text":\[?"([^"]{1,200})"\]?\}\},"label_0":\{"value":\{"text":\[?"([^"]{1,80})"\]?\}\}`)

// flipkartStateSpecs reads the specification table out of that embedded state.
//
// Flipkart renders its specifications from JavaScript, so they are not in the
// markup at all -- the server sends the whole page as a state blob and the
// browser builds the table. Reading the blob is the only way to get case
// diameter, dial colour and movement, which are exactly the fields this
// catalogue filters on.
func flipkartStateSpecs(doc *html.Node) []Spec {
	var specs []Spec
	for _, script := range scriptContents(doc, "label_0") {
		for _, match := range flipkartStatePair.FindAllStringSubmatch(script, -1) {
			value, label := match[1], match[2]
			specs = append(specs, Spec{Label: label, Value: value})
		}
	}
	return specs
}

// flipkartSpecRows reads label/value pairs out of Flipkart's div grid.
func flipkartSpecRows(doc *html.Node) []Spec {
	var specs []Spec
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || !classContains(n, "col-3-12") {
			return true
		}
		// The value is the next element sibling; text nodes between them are
		// whitespace from the source's own formatting.
		for sibling := n.NextSibling; sibling != nil; sibling = sibling.NextSibling {
			if sibling.Type != html.ElementNode {
				continue
			}
			if classContains(sibling, "col-9-12") {
				specs = append(specs, Spec{Label: textOf(n), Value: textOf(sibling)})
			}
			break
		}
		return true
	})
	return specs
}

// titleText returns the document <title>, trimmed of the shop's own suffix.
func titleText(doc *html.Node) string {
	for _, node := range elementsByTag(doc, "title") {
		text := textOf(node)
		// "Product name | Shop name" and "Product name - Buy online at Shop"
		// are the two shapes; keep the longest leading segment.
		for _, sep := range []string{" | ", " – ", " — "} {
			if idx := strings.Index(text, sep); idx > 10 {
				text = text[:idx]
				break
			}
		}
		return text
	}
	return ""
}

// rukminimImage matches Flipkart's image CDN, which is where its gallery
// lives; the markup around it is generated class names that change weekly.
var rukminimImage = regexp.MustCompile(`https://rukminim[0-9]*\.flixcart\.com/image/[0-9]+/[0-9]+/[^"'\\ ]+\.(?:jpe?g|png|webp)`)

// extractFlipkart supplements Flipkart's JSON-LD, which is reliable for name,
// price and rating but carries only one image.
func extractFlipkart(doc *html.Node, base *url.URL) *Draft {
	draft := &Draft{Extractor: "flipkart"}

	draft.Name = firstNonEmpty(titleText(doc), metaContent(doc, "og:title"))
	if idx := strings.Index(draft.Name, " Price in India"); idx > 0 {
		// Flipkart's own titles end with "... Price in India - Buy ... Online".
		draft.Name = draft.Name[:idx]
	}
	draft.Currency = "INR"

	// The gallery is present in the page's embedded state as a list of CDN
	// URLs, whatever the markup around it looks like today.
	seen := map[string]bool{}
	for _, script := range scriptContents(doc, "rukminim") {
		for _, match := range rukminimImage.FindAllString(script, -1) {
			url := UpgradeImageURL(unescapeSlashes(match))
			if !seen[url] {
				seen[url] = true
				draft.Images = append(draft.Images, url)
			}
		}
	}
	for _, image := range metaContents(doc, "og:image") {
		draft.Images = append(draft.Images, UpgradeImageURL(absolute(base, image)))
	}

	// Flipkart writes its specifications as a two-column grid of divs rather
	// than a table. The class names are generated and change often, but the
	// column-width fragments ("col-3-12" for the label, "col-9-12" for the
	// value) have outlived several redesigns.
	specs := append(specsFromTable(doc), flipkartSpecRows(doc)...)
	draft.Specs = dropNoiseSpecs(append(specs, flipkartStateSpecs(doc)...))

	if draft.Empty() {
		return nil
	}
	return draft
}
