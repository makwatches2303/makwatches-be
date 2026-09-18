package scrape

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// A Variant is one purchasable version of the same watch -- almost always a
// colourway.
//
// Read separately rather than folded into the main gallery, because a listing
// page carries every colourway's photographs at once and mixing them produces
// a product whose gallery shows a black watch, a rose-gold watch and a blue
// watch as though they were three angles of one item. That has shipped in this
// catalogue before; see internal/imagefetch's package comment.
type Variant struct {
	// Label as the source writes it, e.g. "Deep Wine".
	Label string `json:"label"`
	// SKU is the source's own id for this variant (an ASIN on Amazon), which
	// is also what makes its page reachable.
	SKU string `json:"sku,omitempty"`
	// URL is that variant's own product page, so its full gallery and price
	// can be read on demand rather than guessed from this one.
	URL string `json:"url,omitempty"`
	// Images are the photographs belonging to this colourway alone.
	Images []string `json:"images,omitempty"`
	// Current marks the variant this page is actually about: the one whose
	// price, specifications and full gallery the rest of the draft describes.
	Current bool `json:"current,omitempty"`
}

var (
	// twisterDisplayData maps each variant's id to its label.
	twisterDisplayData = regexp.MustCompile(`"dimensionValuesDisplayData"\s*:\s*\{`)
	// twisterVariationValues names the dimension ("color_name") and lists its
	// values. Only the name is read here; the values come with their ids.
	twisterVariationValues = regexp.MustCompile(`"variationValues"\s*:\s*\{`)
	// perVariantImages is the gallery map keyed by variant label. Distinct
	// from the single-quoted 'colorImages': { 'initial': ... } block, which is
	// the currently-selected variant's own full gallery.
	perVariantImages = regexp.MustCompile(`"colorImages"\s*:\s*\{"`)
	landingVariant   = regexp.MustCompile(`"landingAsinColor"\s*:\s*"([^"]*)"`)
)

// amazonVariants reads the colourways a listing offers.
//
// Returns the dimension's display name ("Colour"), the variants, and the label
// of the one this page is showing. Everything is best effort: a listing with no
// variants is the common case and yields nothing at all.
func amazonVariants(doc *html.Node, base *url.URL) (dimension string, variants []Variant, current string) {
	scripts := scriptContents(doc, "dimensionValuesDisplayData")
	scripts = append(scripts, scriptContents(doc, "landingAsinColor")...)
	if len(scripts) == 0 {
		return "", nil, ""
	}

	labelsBySKU := map[string]string{}
	imagesByLabel := map[string][]string{}

	for _, script := range scripts {
		if current == "" {
			if match := landingVariant.FindStringSubmatch(script); len(match) == 2 {
				current = match[1]
			}
		}

		if dimension == "" {
			if raw := objectAfter(script, twisterVariationValues); raw != "" {
				var values map[string][]string
				if json.Unmarshal([]byte(raw), &values) == nil {
					for key := range values {
						dimension = prettyDimension(key)
						break
					}
				}
			}
		}

		if len(labelsBySKU) == 0 {
			if raw := objectAfter(script, twisterDisplayData); raw != "" {
				var display map[string][]string
				if json.Unmarshal([]byte(raw), &display) == nil {
					for sku, labels := range display {
						if len(labels) > 0 && labels[0] != "" {
							labelsBySKU[sku] = strings.Join(labels, " / ")
						}
					}
				}
			}
		}

		if len(imagesByLabel) == 0 {
			if raw := objectAfter(script, perVariantImages); raw != "" {
				var galleries map[string][]struct {
					HiRes string `json:"hiRes"`
					Large string `json:"large"`
				}
				if json.Unmarshal([]byte(raw), &galleries) == nil {
					for label, shots := range galleries {
						for _, shot := range shots {
							if image := firstNonEmpty(shot.HiRes, shot.Large); image != "" {
								imagesByLabel[label] = append(imagesByLabel[label], UpgradeImageURL(unescapeSlashes(image)))
							}
						}
					}
				}
			}
		}
	}

	if len(labelsBySKU) == 0 && len(imagesByLabel) == 0 {
		return "", nil, ""
	}
	if dimension == "" {
		dimension = "Colour"
	}

	// Built from the id map where there is one, so every variant carries the
	// link to its own page; labels seen only in the image map are still
	// included, because a photograph and a name is enough to be worth showing.
	seen := map[string]bool{}
	for sku, label := range labelsBySKU {
		variant := Variant{
			Label:   label,
			SKU:     sku,
			URL:     variantURL(base, sku),
			Images:  mergeImages(nil, imagesByLabel[label]),
			Current: strings.EqualFold(label, current),
		}
		seen[strings.ToLower(label)] = true
		variants = append(variants, variant)
	}
	for label, images := range imagesByLabel {
		if seen[strings.ToLower(label)] {
			continue
		}
		variants = append(variants, Variant{
			Label:   label,
			Images:  mergeImages(nil, images),
			Current: strings.EqualFold(label, current),
		})
	}

	// A single variant is not a variant: a listing that offers one colourway
	// is just a product, and showing a picker with one entry is noise.
	if len(variants) < 2 {
		return "", nil, current
	}

	sortVariants(variants)
	return dimension, variants, current
}

// variantURL builds the canonical product URL for another id on the same site.
func variantURL(base *url.URL, sku string) string {
	if base == nil || sku == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s/dp/%s", base.Scheme, base.Host, sku)
}

// prettyDimension turns Amazon's internal dimension key into something an
// admin reads: "color_name" becomes "Colour".
func prettyDimension(key string) string {
	switch strings.ToLower(key) {
	case "color_name", "color", "colour_name", "colour":
		return "Colour"
	case "size_name", "size":
		return "Size"
	case "style_name", "style":
		return "Style"
	}
	cleaned := strings.ReplaceAll(strings.TrimSuffix(key, "_name"), "_", " ")
	if cleaned == "" {
		return "Variant"
	}
	return strings.ToUpper(cleaned[:1]) + cleaned[1:]
}

// sortVariants puts the one being viewed first and the rest in a stable
// alphabetical order, so the panel's list does not reshuffle between reads.
func sortVariants(variants []Variant) {
	for i := 1; i < len(variants); i++ {
		for j := i; j > 0; j-- {
			left, right := variants[j-1], variants[j]
			if left.Current || (!right.Current && strings.ToLower(left.Label) <= strings.ToLower(right.Label)) {
				break
			}
			variants[j-1], variants[j] = right, left
		}
	}
}

// objectAfter returns the JSON object that begins at the brace the pattern
// ends on, matched to its close.
//
// A brace-matching reader rather than a regular expression because these
// objects nest, quote braces inside strings, and run to tens of kilobytes --
// the shapes that regular expressions get quietly wrong.
func objectAfter(script string, pattern *regexp.Regexp) string {
	location := pattern.FindStringIndex(script)
	if location == nil {
		return ""
	}
	// The pattern ends just past its opening brace, except where it also
	// consumed the first key's quote.
	start := strings.LastIndex(script[:location[1]], "{")
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(script); i++ {
		c := script[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are data, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return script[start : i+1]
			}
		}
	}
	return ""
}
