package scrape

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Amazon publishes no structured product data for its listings, so this is the
// one source that has to be read out of the layout. Everything here is
// therefore expected to rot: the selectors are the ones that have been stable
// across years of Amazon's own A/B tests (ids, not classes, wherever the id
// exists), and each is tried in turn so a redesign degrades the draft rather
// than emptying it.

var (
	// hiResImage matches the image list Amazon embeds as JavaScript on every
	// listing. Both quoting styles appear depending on which template served
	// the page. These are the seller's own uploads -- the thumbnail strip's
	// <img src> is a genuinely smaller asset that cannot be upscaled by URL
	// rewriting, a mistake this catalogue has shipped before (see
	// internal/imagefetch's package comment).
	hiResImage = regexp.MustCompile(`["']hiRes["']\s*:\s*["'](https:[^"']+)["']`)
	largeImage = regexp.MustCompile(`["']large["']\s*:\s*["'](https:[^"']+)["']`)
	// asinFromPath pulls the product id out of /dp/B0D7QK57R7 or
	// /gp/product/B0D7QK57R7, ignoring the rest of the tracking spaghetti.
	asinFromPath = regexp.MustCompile(`/(?:dp|gp/product|gp/aw/d)/([A-Z0-9]{10})`)
)

// extractAmazon reads an Amazon product page.
func extractAmazon(doc *html.Node, base *url.URL) *Draft {
	draft := &Draft{Extractor: "amazon"}

	if title := elementByID(doc, "productTitle"); title != nil {
		draft.Name = textOf(title)
	}
	if draft.Name == "" {
		draft.Name = strings.TrimSuffix(metaContent(doc, "og:title"), " : Amazon.in")
	}

	draft.Brand = amazonBrand(doc)
	draft.Price, draft.MRP = amazonPrices(doc)
	draft.Currency = "INR"
	draft.Bullets = amazonBullets(doc)
	draft.Specs = amazonSpecs(doc)
	draft.Images = amazonImages(doc, base)

	if match := asinFromPath.FindStringSubmatch(base.Path); len(match) == 2 {
		draft.SKU = match[1]
	}

	// The "Product Description" block, when the seller wrote one. Bullets are
	// kept separately because they are the part worth keeping most often.
	for _, id := range []string{"productDescription", "bookDescription_feature_div"} {
		if node := elementByID(doc, id); node != nil {
			if text := textOf(node); len(text) > 40 {
				draft.Description = text
				break
			}
		}
	}
	if draft.Description == "" {
		draft.Description = strings.Join(draft.Bullets, " ")
	}

	if draft.Empty() {
		return nil
	}
	return draft
}

// amazonBrand reads the byline, which is written half a dozen ways.
func amazonBrand(doc *html.Node) string {
	if byline := elementByID(doc, "bylineInfo"); byline != nil {
		text := textOf(byline)
		// "Visit the Fastrack Store", "Brand: Fastrack", "Fastrack".
		for _, prefix := range []string{"Visit the ", "Brand: "} {
			if strings.HasPrefix(text, prefix) {
				text = strings.TrimPrefix(text, prefix)
				break
			}
		}
		text = strings.TrimSuffix(text, " Store")
		if text != "" && len(text) < 60 {
			return text
		}
	}
	// The product-overview table carries a Brand row on most listings.
	for _, spec := range amazonSpecs(doc) {
		if strings.EqualFold(spec.Label, "brand") || strings.EqualFold(spec.Label, "manufacturer") {
			return spec.Value
		}
	}
	return ""
}

// amazonPrices returns the selling price and the struck-through list price.
//
// The selling price is read from the price block rather than from the first
// .a-price on the page: the page also carries prices for "similar items",
// "frequently bought together" and sponsored strips, and the first match in
// document order is not reliably the product being looked at.
func amazonPrices(doc *html.Node) (price, mrp float64) {
	for _, id := range []string{"corePriceDisplay_desktop_feature_div", "corePrice_feature_div", "apex_desktop", "corePrice_desktop", "price"} {
		block := elementByID(doc, id)
		if block == nil {
			continue
		}
		if price == 0 {
			price = amazonOffscreenPrice(block, "a-price")
		}
		if mrp == 0 {
			// The list price is the same markup with an extra class, or a
			// row labelled M.R.P.
			mrp = amazonOffscreenPrice(block, "a-text-price")
			if mrp == 0 {
				mrp = amazonRowPrice(block, "basisPrice")
			}
		}
		if price > 0 {
			break
		}
	}

	if price == 0 {
		// Last resort: the whole document. Better a price to check than none.
		price = amazonOffscreenPrice(doc, "a-price")
	}
	return price, mrp
}

// amazonOffscreenPrice finds the screen-reader copy of a price, which is the
// only place Amazon writes it as one contiguous string -- the visible markup
// splits "4,395" across separate whole/fraction spans.
func amazonOffscreenPrice(root *html.Node, wrapperClass string) float64 {
	for _, wrapper := range elementsByClass(root, wrapperClass) {
		for _, span := range elementsByClass(wrapper, "a-offscreen") {
			if value := ParsePrice(textOf(span)); value > 0 {
				return value
			}
		}
	}
	return 0
}

func amazonRowPrice(root *html.Node, class string) float64 {
	if node := firstByClass(root, class); node != nil {
		return ParsePrice(textOf(node))
	}
	return 0
}

// amazonBullets reads the "About this item" list.
func amazonBullets(doc *html.Node) []string {
	block := elementByID(doc, "feature-bullets")
	if block == nil {
		block = elementByID(doc, "featurebullets_feature_div")
	}
	if block == nil {
		return nil
	}
	var out []string
	for _, item := range elementsByTag(block, "li") {
		text := textOf(item)
		// Amazon hides several list items for its own UI ("See more product
		// details"); they are short and never useful.
		if text == "" || len(text) < 12 || strings.HasPrefix(text, "See more") {
			continue
		}
		out = append(out, text)
	}
	return out
}

// amazonSpecs reads whichever of the four specification layouts this listing
// happens to use.
func amazonSpecs(doc *html.Node) []Spec {
	var specs []Spec

	// 1 & 2: the technical-details and product-details tables.
	for _, id := range []string{
		"productDetails_techSpec_section_1",
		"productDetails_techSpec_section_2",
		"productDetails_detailBullets_sections1",
		"technicalSpecifications_section_1",
	} {
		if table := elementByID(doc, id); table != nil {
			specs = append(specs, specsFromTable(table)...)
		}
	}

	// 3: the newer product-overview grid at the top of the page.
	if overview := elementByID(doc, "productOverview_feature_div"); overview != nil {
		specs = append(specs, specsFromTable(overview)...)
	}

	// 4: the "Product details" bullet list, where label and value are
	// separated by a colon inside one list item.
	if bullets := elementByID(doc, "detailBullets_feature_div"); bullets != nil {
		for _, item := range elementsByTag(bullets, "li") {
			text := textOf(item)
			// The separator is a unicode colon surrounded by thin spaces on
			// this layout, so split on the first colon of either kind.
			idx := strings.IndexAny(text, ":：")
			if idx <= 0 || idx > 60 {
				continue
			}
			specs = append(specs, Spec{
				Label: strings.Trim(text[:idx], " ‎‏"),
				Value: strings.Trim(text[idx+1:], " ‎‏"),
			})
		}
	}

	return dropNoiseSpecs(specs)
}

// specsFromTable reads label/value pairs out of a two-column table, accepting
// both <th>/<td> and <td>/<td> layouts.
func specsFromTable(root *html.Node) []Spec {
	var specs []Spec
	for _, row := range elementsByTag(root, "tr") {
		var cells []string
		for cell := row.FirstChild; cell != nil; cell = cell.NextSibling {
			if cell.Type == html.ElementNode && (cell.Data == "th" || cell.Data == "td") {
				cells = append(cells, textOf(cell))
			}
		}
		if len(cells) == 2 && cells[0] != "" && cells[1] != "" {
			specs = append(specs, Spec{Label: cells[0], Value: cells[1]})
		}
	}
	return specs
}

// dropNoiseSpecs removes rows that are about the listing rather than the
// watch.
//
// Two kinds of noise. The named ones are marketplace bookkeeping -- rankings,
// ASINs, packer details -- which no shopper of ours wants to read. The rest is
// structural: reading a page's embedded state pulls in every label/value pair
// on it, including delivery promises, bank offers and seller ratings, so a
// label that looks like money, a percentage or a sentence is rejected on
// shape. A specification label is a short noun phrase; anything else is
// something we have misread.
func dropNoiseSpecs(specs []Spec) []Spec {
	noise := []string{
		"best sellers rank", "customer reviews", "date first available",
		"asin", "product dimensions", "item weight", "manufacturer reference",
		"packer", "importer", "country of origin", "feedback", "warranty",
		"try on", "product sold", "quality score", "speed score", "note",
		"delivery", "offer", "emi", "exchange", "seller", "rating", "in stock",
		"view similar", "highlights", "price",
	}
	out := make([]Spec, 0, len(specs))
	for _, spec := range specs {
		label := strings.ToLower(spec.Label)
		if !looksLikeSpecLabel(spec.Label) || strings.HasPrefix(spec.Value, "•") {
			continue
		}
		skip := false
		for _, bad := range noise {
			if strings.Contains(label, bad) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, spec)
		}
	}
	return out
}

// looksLikeSpecLabel rejects anything that is not a short attribute name:
// prices, percentages, counts and half-sentences all arrive as "labels" when a
// page's whole state is read.
func looksLikeSpecLabel(label string) bool {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" || len(trimmed) > 40 {
		return false
	}
	if strings.ContainsAny(trimmed, "₹$%•|") {
		return false
	}
	letters := 0
	for _, r := range trimmed {
		switch {
		case r >= '0' && r <= '9':
			// A digit in a label ("1 Year Warranty") means it is a value.
			return false
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	return letters >= 3
}

// amazonImages collects the gallery at the highest resolution the page offers.
func amazonImages(doc *html.Node, base *url.URL) []string {
	var images []string

	for _, script := range scriptContents(doc, "hiRes") {
		for _, match := range hiResImage.FindAllStringSubmatch(script, -1) {
			images = append(images, unescapeSlashes(match[1]))
		}
	}
	if len(images) == 0 {
		for _, script := range scriptContents(doc, "large") {
			for _, match := range largeImage.FindAllStringSubmatch(script, -1) {
				images = append(images, unescapeSlashes(match[1]))
			}
		}
	}

	// The main image element carries the same asset for pages whose script
	// block we could not read.
	if landing := elementByID(doc, "landingImage"); landing != nil {
		if hires := attr(landing, "data-old-hires"); hires != "" {
			images = append(images, absolute(base, hires))
		}
		// data-a-dynamic-image is a JSON object keyed by URL, valued by the
		// dimensions that URL serves.
		if dynamic := attr(landing, "data-a-dynamic-image"); dynamic != "" {
			for _, candidate := range jsonObjectKeys(dynamic) {
				images = append(images, absolute(base, candidate))
			}
		}
		if src := attr(landing, "src"); src != "" {
			images = append(images, absolute(base, src))
		}
	}

	upgraded := make([]string, 0, len(images))
	for _, image := range images {
		upgraded = append(upgraded, UpgradeImageURL(image))
	}
	return upgraded
}

var jsonKey = regexp.MustCompile(`"(https?:[^"]+)"\s*:`)

// jsonObjectKeys pulls the keys out of a JSON object without decoding it,
// which is what data-a-dynamic-image needs: its values are arrays of numbers
// we do not care about and its quoting survives HTML-attribute escaping
// unevenly.
func jsonObjectKeys(raw string) []string {
	var out []string
	for _, match := range jsonKey.FindAllStringSubmatch(raw, -1) {
		out = append(out, unescapeSlashes(match[1]))
	}
	return out
}

func unescapeSlashes(s string) string {
	return strings.ReplaceAll(s, `\/`, "/")
}

// looksBlocked reports whether a page is a bot wall rather than a product.
//
// Told apart from a genuine 404 because the two need different words in front
// of an admin: one means "check the link", the other means "the site refused
// us, paste the details by hand or try again".
func looksBlocked(body []byte) bool {
	sample := strings.ToLower(string(body))
	if len(sample) > 20000 {
		sample = sample[:20000]
	}
	for _, marker := range []string{
		"enter the characters you see below",
		"type the characters you see in this image",
		"/errors/validatecaptcha",
		"api-services-support@amazon.com",
		"to discuss automated access to amazon data",
		"request blocked",
		"access denied",
		"are you a human",
		"verifying you are human",
		"just a moment...",
		"checking your browser before accessing",
		"cf-browser-verification",
		"px-captcha",
	} {
		if strings.Contains(sample, marker) {
			return true
		}
	}
	return false
}
