package scrape

import (
	"regexp"
	"strconv"
	"strings"
)

// Spec is one row of a product's specification table, as the source wrote it.
//
// Kept as a flat label/value pair rather than mapped onto our own fields: the
// sources disagree about names ("Case Diameter", "Dial Size", "Item Display
// Diameter") and guessing wrong silently is worse than handing an admin the
// rows and letting them decide.
type Spec struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Draft is everything we could read off a product page.
//
// Every field is best-effort and may be empty. It is deliberately *not* a
// Product: nothing here has been approved by a human, prices and stock claims
// belong to someone else's shop, and the admin panel is expected to show this
// for correction before any of it reaches the catalogue.
type Draft struct {
	SourceURL  string `json:"sourceUrl"`
	SourceSite string `json:"sourceSite"`
	// Extractor names the strategy that produced the bulk of this draft, so
	// the panel can say where the data came from and we can tell a rich
	// structured read from a scrape of last resort.
	Extractor string `json:"extractor"`

	Name        string `json:"name"`
	Brand       string `json:"brand"`
	Description string `json:"description"`
	// Bullets are the source's own selling points, kept separate from the
	// description so an admin can drop marketing copy wholesale.
	Bullets []string `json:"bullets,omitempty"`

	Price    float64 `json:"price"`
	MRP      float64 `json:"mrp"`
	Currency string  `json:"currency,omitempty"`

	SKU      string `json:"sku,omitempty"`
	ModelNo  string `json:"modelNumber,omitempty"`
	Category string `json:"category,omitempty"`

	Images []string `json:"images,omitempty"`
	Specs  []Spec   `json:"specs,omitempty"`

	// Attributes are this catalogue's own filterable fields, guessed from the
	// specs above. Always presented for confirmation, never applied silently.
	Attributes *Attributes `json:"attributes,omitempty"`

	// VariantDimension names what separates the variants, e.g. "Colour".
	VariantDimension string `json:"variantDimension,omitempty"`
	// Variants are the other purchasable versions of the same watch. Empty
	// for a listing that sells one thing. The images above belong to the
	// variant marked Current, never to all of them at once.
	Variants []Variant `json:"variants,omitempty"`

	// Warnings are things the admin should look at before approving: a price
	// we could not find, a description that is clearly truncated, images that
	// may be a size variant of each other.
	Warnings []string `json:"warnings,omitempty"`
}

// Empty reports whether nothing usable was found. A draft with no name is not
// worth showing anyone: every extractor finds a title first.
func (d *Draft) Empty() bool {
	return d == nil || (strings.TrimSpace(d.Name) == "" && len(d.Images) == 0)
}

// fillFrom copies fields from a lower-priority extraction into the gaps of
// this one, so a rich source can be topped up from a thin one without a weaker
// guess ever overwriting a stronger one.
func (d *Draft) fillFrom(other *Draft) {
	if other == nil {
		return
	}
	if d.Name == "" {
		d.Name = other.Name
	}
	if d.Brand == "" {
		d.Brand = other.Brand
	}
	if d.Description == "" {
		d.Description = other.Description
	}
	if len(d.Bullets) == 0 {
		d.Bullets = other.Bullets
	}
	if d.Price == 0 {
		d.Price = other.Price
	}
	if d.MRP == 0 {
		d.MRP = other.MRP
	}
	if d.Currency == "" {
		d.Currency = other.Currency
	}
	if d.SKU == "" {
		d.SKU = other.SKU
	}
	if d.ModelNo == "" {
		d.ModelNo = other.ModelNo
	}
	if d.Category == "" {
		d.Category = other.Category
	}
	if len(d.Specs) == 0 {
		d.Specs = other.Specs
	}
	if len(d.Variants) == 0 {
		d.Variants = other.Variants
		d.VariantDimension = other.VariantDimension
	}
	// Images merge rather than replace: a page often carries its gallery in
	// one place and its highest-resolution hero in another.
	d.Images = mergeImages(d.Images, other.Images)
}

// normalize tidies a finished draft: whitespace, obviously-wrong prices, and
// duplicate imagery.
func (d *Draft) normalize() {
	d.Name = collapseSpace(d.Name)
	d.Brand = collapseSpace(d.Brand)
	d.Description = strings.TrimSpace(d.Description)
	d.SKU = collapseSpace(d.SKU)
	d.ModelNo = collapseSpace(d.ModelNo)

	cleanBullets := make([]string, 0, len(d.Bullets))
	seen := map[string]bool{}
	for _, b := range d.Bullets {
		b = collapseSpace(b)
		if b == "" || len(b) > 400 || seen[strings.ToLower(b)] {
			continue
		}
		seen[strings.ToLower(b)] = true
		cleanBullets = append(cleanBullets, b)
	}
	d.Bullets = cleanBullets

	cleanSpecs := make([]Spec, 0, len(d.Specs))
	seenSpec := map[string]bool{}
	for _, s := range d.Specs {
		// Sources vary on whether the label carries its own colon; the panel
		// draws one, so two look like a typo.
		label := strings.TrimRight(collapseSpace(s.Label), ": ")
		value := collapseSpace(s.Value)
		if label == "" || value == "" || len(label) > 80 || len(value) > 300 {
			continue
		}
		key := strings.ToLower(label)
		if seenSpec[key] {
			continue
		}
		seenSpec[key] = true
		cleanSpecs = append(cleanSpecs, Spec{Label: label, Value: value})
	}
	d.Specs = cleanSpecs

	d.Images = mergeImages(nil, d.Images)
	if len(d.Images) > 12 {
		// Past a dozen the rest are lifestyle shots and size charts. The admin
		// picks from what we show; showing forty is not help.
		d.Images = d.Images[:12]
	}

	// A "sale price" above the list price means we read two unrelated numbers,
	// which is worse than reading one. Keep the lower as the price and say so.
	if d.MRP > 0 && d.Price > 0 && d.MRP < d.Price {
		d.Price, d.MRP = d.MRP, d.Price
	}
	if d.MRP > 0 && d.MRP == d.Price {
		d.MRP = 0
	}
	if d.Price == 0 {
		d.Warnings = append(d.Warnings, "We could not read a price from this page. Enter it yourself before publishing.")
	}
	if d.Brand == "" {
		d.Warnings = append(d.Warnings, "No brand was named on the page. Pick one before publishing.")
	}
	if len(d.Images) == 0 {
		d.Warnings = append(d.Warnings, "No images could be read from this page.")
	}
}

// mergeImages appends new image URLs to a list, dropping duplicates and
// obvious non-product assets, preserving order.
func mergeImages(into []string, add []string) []string {
	seen := make(map[string]bool, len(into)+len(add))
	out := make([]string, 0, len(into)+len(add))
	for _, list := range [][]string{into, add} {
		for _, raw := range list {
			u := strings.TrimSpace(raw)
			if u == "" || !usableImageURL(u) {
				continue
			}
			key := imageIdentity(u)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, u)
		}
	}
	return out
}

// amazonSizeSuffix matches Amazon's "._AC_SX679_." style size directives,
// which sit between the asset id and the extension.
var amazonSizeSuffix = regexp.MustCompile(`\._[A-Za-z0-9,_]+_\.`)

// cdnSizePath matches the width/height segment retail CDNs put in the path,
// and cdnShard the numbered hostnames they round-robin over.
var (
	cdnSizePath = regexp.MustCompile(`/image/\d+/\d+/`)
	cdnShard    = regexp.MustCompile(`rukminim?\d+`)
)

// imageIdentity strips the parts of a URL that only choose a rendering size or
// a CDN shard, so the same photograph counts once however it is addressed.
//
// Without the shard and path rules, a Flipkart gallery arrives twice: once as
// rukmini1/image/1500/1500/... from the structured data and once as
// rukminim2/image/1664/1664/... after the size upgrade, and the admin is shown
// every watch twice over.
func imageIdentity(raw string) string {
	id := amazonSizeSuffix.ReplaceAllString(raw, ".")
	if i := strings.IndexByte(id, '?'); i >= 0 {
		// Query strings on image CDNs are nearly always width/quality knobs.
		id = id[:i]
	}
	id = cdnSizePath.ReplaceAllString(id, "/image/")
	id = cdnShard.ReplaceAllString(id, "rukmini")
	return strings.ToLower(id)
}

// UpgradeImageURL asks a CDN for the largest version of an asset it will give
// us, so the catalogue does not end up with 100px thumbnails.
func UpgradeImageURL(raw string) string {
	if strings.Contains(raw, "media-amazon.com") || strings.Contains(raw, "ssl-images-amazon.com") {
		// Dropping the size directive entirely returns the original upload,
		// which is what the zoom viewer uses.
		return amazonSizeSuffix.ReplaceAllString(raw, ".")
	}
	if strings.Contains(raw, "rukminim") { // Flipkart's image CDN
		// Flipkart encodes the size as a path segment: /image/416/416/....
		return regexp.MustCompile(`/image/\d+/\d+/`).ReplaceAllString(raw, "/image/1664/1664/")
	}
	if strings.Contains(raw, "cdn.shopify.com") {
		// Shopify appends _500x500 before the extension.
		return regexp.MustCompile(`_(\d+)x(\d+)?(\.[a-zA-Z]+)`).ReplaceAllString(raw, "$3")
	}
	return raw
}

// usableImageURL filters out sprites, tracking pixels, icons and data URIs
// that no one wants in a product gallery.
func usableImageURL(raw string) bool {
	low := strings.ToLower(raw)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return false
	}
	if strings.HasSuffix(low, ".svg") || strings.HasSuffix(low, ".gif") {
		return false
	}
	for _, bad := range []string{
		"sprite", "icon", "logo", "pixel", "transparent-pixel", "grey-pixel",
		"placeholder", "loading", "spinner", "play-button", "1x1", "badge",
		"prime", "star-rating", "captcha",
	} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	return true
}

var spaceRun = regexp.MustCompile(`\s+`)

func collapseSpace(s string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(strings.ReplaceAll(s, " ", " "), " "))
}

// priceDigits keeps digits, dots and commas so "₹4,395.00" survives to
// ParseFloat and "MRP: ₹5,995 (incl. of all taxes)" does not smear two
// numbers together.
var priceToken = regexp.MustCompile(`[0-9][0-9,]*(?:\.[0-9]{1,2})?`)

// ParsePrice reads the first money-shaped number out of a string.
//
// Returns 0 when there is none, which every caller treats as "not found"
// rather than "free".
func ParsePrice(s string) float64 {
	match := priceToken.FindString(strings.TrimSpace(s))
	if match == "" {
		return 0
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(match, ",", ""), 64)
	if err != nil || value <= 0 {
		return 0
	}
	// A watch priced in crores is a misread of a phone number or a pincode.
	if value > 10_000_000 {
		return 0
	}
	return value
}
