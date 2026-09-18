package scrape

import (
	"net/url"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return parsed
}

func TestParseTargetRejectsAddressesInsideTheNetwork(t *testing.T) {
	// The endpoint that hands out this Lambda's credentials, the database on
	// localhost, and a scheme that would read the filesystem. Each of these is
	// a real way an admin-supplied URL turns into a breach.
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/admin",
		"http://[::1]/",
		"http://10.0.0.5/internal",
		"http://192.168.1.1/",
		"file:///etc/passwd",
		"gopher://example.com/",
	} {
		if _, err := ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q) was allowed; it must be refused", raw)
		}
	}
}

func TestParseTargetAcceptsOrdinaryProductURLs(t *testing.T) {
	for _, raw := range []string{
		"https://www.amazon.in/dp/B0D7QK57R7",
		"www.flipkart.com/some-watch/p/itm123",
	} {
		parsed, err := ParseTarget(raw)
		if err != nil {
			t.Fatalf("ParseTarget(%q) refused a normal URL: %v", raw, err)
		}
		if parsed.Scheme != "https" {
			t.Errorf("ParseTarget(%q) scheme = %q, want https", raw, parsed.Scheme)
		}
	}
}

func TestParsePrice(t *testing.T) {
	cases := map[string]float64{
		"₹4,395":                4395,
		"₹4,395.00":             4395,
		"M.R.P.: ₹5,995":        5995,
		"4395":                  4395,
		"Rs. 1,29,999":          129999,
		"":                      0,
		"Currently unavailable": 0,
		"₹99,999,999,999":       0, // a misread, not a watch
	}
	for input, want := range cases {
		if got := ParsePrice(input); got != want {
			t.Errorf("ParsePrice(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMergeImagesDropsSizeVariantsOfTheSamePhoto(t *testing.T) {
	// Amazon serves one asset at a dozen sizes. A gallery of the same watch
	// four times is what the admin sees if these are not folded together.
	merged := mergeImages(nil, []string{
		"https://m.media-amazon.com/images/I/71abc._AC_SX679_.jpg",
		"https://m.media-amazon.com/images/I/71abc._AC_SL1500_.jpg",
		"https://m.media-amazon.com/images/I/71abc.jpg",
		"https://m.media-amazon.com/images/I/82def._AC_SX679_.jpg",
	})
	if len(merged) != 2 {
		t.Fatalf("mergeImages kept %d images, want 2: %v", len(merged), merged)
	}
}

func TestMergeImagesDropsSpritesAndIcons(t *testing.T) {
	merged := mergeImages(nil, []string{
		"https://example.com/logo.png",
		"https://example.com/transparent-pixel.gif",
		"https://example.com/icons/star-rating.png",
		"data:image/png;base64,iVBOR",
		"https://example.com/photos/watch-front.jpg",
	})
	if len(merged) != 1 || !strings.HasSuffix(merged[0], "watch-front.jpg") {
		t.Fatalf("mergeImages = %v, want only the product photo", merged)
	}
}

func TestUpgradeImageURLAsksForTheOriginal(t *testing.T) {
	got := UpgradeImageURL("https://m.media-amazon.com/images/I/71abc._AC_SX466_.jpg")
	want := "https://m.media-amazon.com/images/I/71abc.jpg"
	if got != want {
		t.Errorf("UpgradeImageURL = %q, want %q", got, want)
	}
}

const jsonLDPage = `<html><head>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"Product",
 "name":"Titan Neo Analog Blue Dial Men's Watch",
 "brand":{"@type":"Brand","name":"Titan"},
 "sku":"1806SM01",
 "description":"<p>A <b>blue dial</b> watch.</p>",
 "image":["https://cdn.example.com/a.jpg","https://cdn.example.com/b.jpg"],
 "additionalProperty":[{"@type":"PropertyValue","name":"Strap Material","value":"Stainless Steel"},
                       {"@type":"PropertyValue","name":"Dial Colour","value":"Blue"}],
 "offers":{"@type":"Offer","price":"4395.00","priceCurrency":"INR","highPrice":"5995"}}
</script></head><body><h1>Titan Neo</h1></body></html>`

func TestExtractJSONLD(t *testing.T) {
	doc, err := parseHTML([]byte(jsonLDPage))
	if err != nil {
		t.Fatal(err)
	}
	draft := extractJSONLD(doc, mustParse(t, "https://example.com/p/1"))
	if draft == nil {
		t.Fatal("extractJSONLD found nothing in a page that carries a Product")
	}
	if draft.Name != "Titan Neo Analog Blue Dial Men's Watch" {
		t.Errorf("name = %q", draft.Name)
	}
	if draft.Brand != "Titan" {
		t.Errorf("brand = %q, want Titan", draft.Brand)
	}
	if draft.Price != 4395 {
		t.Errorf("price = %v, want 4395", draft.Price)
	}
	if draft.MRP != 5995 {
		t.Errorf("mrp = %v, want 5995", draft.MRP)
	}
	if draft.SKU != "1806SM01" {
		t.Errorf("sku = %q", draft.SKU)
	}
	if len(draft.Images) != 2 {
		t.Errorf("images = %v, want 2", draft.Images)
	}
	// The description arrived as markup and must not reach an admin as tags.
	if strings.Contains(draft.Description, "<") {
		t.Errorf("description still contains markup: %q", draft.Description)
	}
	if len(draft.Specs) < 2 {
		t.Errorf("specs = %v, want the two additionalProperty rows", draft.Specs)
	}
}

func TestExtractJSONLDIgnoresNonProductNodes(t *testing.T) {
	page := `<html><head><script type="application/ld+json">
	{"@context":"https://schema.org","@type":"BreadcrumbList","itemListElement":[]}
	</script></head><body></body></html>`
	doc, _ := parseHTML([]byte(page))
	if draft := extractJSONLD(doc, mustParse(t, "https://example.com")); draft != nil {
		t.Errorf("extractJSONLD returned %+v for a page with no Product", draft)
	}
}

const amazonPage = `<html><body>
<span id="productTitle">  Noise Fashion Smart Watch for Women  </span>
<div id="bylineInfo">Visit the Noise Store</div>
<div id="corePriceDisplay_desktop_feature_div">
  <span class="a-price"><span class="a-offscreen">₹2,499</span></span>
  <span class="a-text-price"><span class="a-offscreen">₹5,999</span></span>
</div>
<div id="feature-bullets"><ul>
  <li><span class="a-list-item">1.85" AMOLED display with always-on mode</span></li>
  <li><span class="a-list-item">See more</span></li>
</ul></div>
<table id="productDetails_techSpec_section_1">
  <tr><th>Dial Colour</th><td>Rose Gold</td></tr>
  <tr><th>Strap Material</th><td>Silicone</td></tr>
  <tr><th>Best Sellers Rank</th><td>#12 in Watches</td></tr>
</table>
<img id="landingImage" src="https://m.media-amazon.com/images/I/61xyz._AC_SX466_.jpg"
     data-old-hires="https://m.media-amazon.com/images/I/61xyz.jpg">
<script>var data = {'colorImages': { 'initial': [
  {"hiRes":"https://m.media-amazon.com/images/I/71aaa.jpg","thumb":"https://m.media-amazon.com/images/I/31aaa.jpg"},
  {"hiRes":"https://m.media-amazon.com/images/I/71bbb.jpg","thumb":"https://m.media-amazon.com/images/I/31bbb.jpg"}]}};</script>
</body></html>`

func TestExtractAmazon(t *testing.T) {
	doc, err := parseHTML([]byte(amazonPage))
	if err != nil {
		t.Fatal(err)
	}
	draft := extractAmazon(doc, mustParse(t, "https://www.amazon.in/Noise-Fashion/dp/B0D7QK57R7/ref=sr_1_1"))
	if draft == nil {
		t.Fatal("extractAmazon found nothing")
	}
	if draft.Name != "Noise Fashion Smart Watch for Women" {
		t.Errorf("name = %q", draft.Name)
	}
	if draft.Brand != "Noise" {
		t.Errorf("brand = %q, want Noise", draft.Brand)
	}
	if draft.Price != 2499 {
		t.Errorf("price = %v, want the selling price 2499", draft.Price)
	}
	if draft.MRP != 5999 {
		t.Errorf("mrp = %v, want the struck-through 5999", draft.MRP)
	}
	if draft.SKU != "B0D7QK57R7" {
		t.Errorf("sku = %q, want the ASIN", draft.SKU)
	}
	if len(draft.Bullets) != 1 {
		t.Errorf("bullets = %v; the 'See more' UI row must be dropped", draft.Bullets)
	}
	for _, spec := range draft.Specs {
		if strings.Contains(strings.ToLower(spec.Label), "best sellers rank") {
			t.Errorf("specs kept a listing-ranking row: %+v", spec)
		}
	}
	// The hiRes assets are the seller's originals; the thumbnail strip is a
	// separate, smaller asset that cannot be upscaled (see imagefetch's docs).
	if len(draft.Images) == 0 || !strings.Contains(draft.Images[0], "71aaa") {
		t.Errorf("images = %v, want the hiRes gallery first", draft.Images)
	}
	for _, image := range draft.Images {
		if strings.Contains(image, "/31") {
			t.Errorf("images contain a thumbnail: %v", draft.Images)
		}
	}
}

func TestGuessAttributesReadsTheSpecTable(t *testing.T) {
	draft := &Draft{
		Name: "Noise Fashion Smart Watch for Women",
		Specs: []Spec{
			{Label: "Dial Colour", Value: "Rose Gold"},
			{Label: "Strap Material", Value: "Silicone"},
			{Label: "Display Type", Value: "Digital"},
			{Label: "Case Shape", Value: "Round"},
		},
	}
	attrs := guessAttributes(draft)
	if attrs.DialColor != "Rose Gold" || attrs.StrapMaterial != "Silicone" ||
		attrs.DialType != "Digital" || attrs.DialShape != "Round" {
		t.Fatalf("guessAttributes = %+v", attrs)
	}
	if attrs.Gender != "Women" || attrs.MainCategory != "Women" {
		t.Errorf("gender = %q / category = %q, want Women", attrs.Gender, attrs.MainCategory)
	}
}

func TestGuessGenderDoesNotFindMenInsideWomen(t *testing.T) {
	// "Women" contains "men". A substring match here files every women's
	// watch under Men, which is the kind of quiet mistake that only shows up
	// as a customer complaint.
	attrs := guessAttributes(&Draft{Name: "Fastrack Uptown Retreat Watch for Women"})
	if attrs.Gender != "Women" {
		t.Errorf("gender = %q, want Women", attrs.Gender)
	}
	attrs = guessAttributes(&Draft{Name: "Titan Analog Watch for Men"})
	if attrs.Gender != "Men" {
		t.Errorf("gender = %q, want Men", attrs.Gender)
	}
	attrs = guessAttributes(&Draft{Name: "Casio Unisex Digital Watch for Men and Women"})
	if attrs.Gender != "Unisex" {
		t.Errorf("gender = %q, want Unisex", attrs.Gender)
	}
}

func TestShopifyEndpoint(t *testing.T) {
	got := shopifyEndpoint(mustParse(t, "https://shop.example.com/collections/watches/products/nova-42?variant=123"))
	want := "https://shop.example.com/collections/watches/products/nova-42.js"
	if got != want {
		t.Errorf("shopifyEndpoint = %q, want %q", got, want)
	}
	if got := shopifyEndpoint(mustParse(t, "https://www.amazon.in/dp/B0D7QK57R7")); got != "" {
		t.Errorf("shopifyEndpoint on a non-Shopify URL = %q, want empty", got)
	}
}

func TestLooksBlockedRecognisesABotWall(t *testing.T) {
	if !looksBlocked([]byte(`<html><body><h4>Enter the characters you see below</h4></body></html>`)) {
		t.Error("an Amazon captcha page was not recognised as blocked")
	}
	if looksBlocked([]byte(amazonPage)) {
		t.Error("a real product page was mistaken for a bot wall")
	}
}

func TestNormalizeWarnsAboutWhatIsMissing(t *testing.T) {
	draft := &Draft{Name: "Some watch"}
	draft.normalize()
	if len(draft.Warnings) != 3 {
		t.Fatalf("warnings = %v, want one each for price, brand and images", draft.Warnings)
	}
}

func TestNormalizeSwapsAnInvertedPricePair(t *testing.T) {
	draft := &Draft{Name: "w", Brand: "b", Images: []string{"https://x.test/a.jpg"}, Price: 5999, MRP: 2499}
	draft.normalize()
	if draft.Price != 2499 || draft.MRP != 5999 {
		t.Errorf("price/mrp = %v/%v, want 2499/5999", draft.Price, draft.MRP)
	}
}

func TestExtractMetaFallsBackToSocialTags(t *testing.T) {
	page := `<html><head>
	<meta property="og:title" content="Casio Enticer Analog Watch">
	<meta property="og:image" content="//cdn.example.com/watch.jpg">
	<meta property="product:price:amount" content="3495">
	</head><body></body></html>`
	doc, _ := parseHTML([]byte(page))
	draft := extractMeta(doc, mustParse(t, "https://example.com/p"))
	if draft == nil {
		t.Fatal("extractMeta found nothing")
	}
	if draft.Price != 3495 {
		t.Errorf("price = %v", draft.Price)
	}
	// Protocol-relative URLs must be resolved, or the import step cannot
	// fetch them.
	if len(draft.Images) != 1 || !strings.HasPrefix(draft.Images[0], "https://") {
		t.Errorf("images = %v, want an absolute URL", draft.Images)
	}
}

const amazonVariantPage = `<html><body>
<span id="productTitle">Noise Pulse 2 Max Smart Watch (Deep Wine)</span>
<script type="text/javascript">
 var obj = jQuery.parseJSON('{"dimensionValuesDisplayData" : {"B0B6BPTFT5":["Deep Wine"],"B0B6BLTGTT":["Jet Black"],"B0B6BQ711Q":["Rose Pink"]},
 "variationValues" : {"color_name":["Deep Wine","Jet Black","Rose Pink"]}}');
</script>
<script>
 P.when('A').register('ImageBlockATF', function(A){
  var data = {
   'colorImages': { 'initial': A.$.parseJSON('[{"hiRes":"https://m.media-amazon.com/images/I/61wine1._SL1500_.jpg"},{"hiRes":"https://m.media-amazon.com/images/I/61wine2._SL1500_.jpg"}]')},
   'colorToAsin': { 'initial': '{}'}};
 });
</script>
<script type="a-state">{"landingAsinColor":"Deep Wine","colorImages":{"Deep Wine":[{"hiRes":"https://m.media-amazon.com/images/I/61wine1._SL1500_.jpg"}],"Jet Black":[{"hiRes":"https://m.media-amazon.com/images/I/61black._SL1500_.jpg"}],"Rose Pink":[{"hiRes":"https://m.media-amazon.com/images/I/61pink._SL1500_.jpg"}]}}</script>
</body></html>`

func TestAmazonVariantsReadsEveryColourway(t *testing.T) {
	doc, err := parseHTML([]byte(amazonVariantPage))
	if err != nil {
		t.Fatal(err)
	}
	base := mustParse(t, "https://www.amazon.in/dp/B0B6BPTFT5")

	dimension, variants, current := amazonVariants(doc, base)
	if dimension != "Colour" {
		t.Errorf("dimension = %q, want Colour", dimension)
	}
	if current != "Deep Wine" {
		t.Errorf("current = %q, want Deep Wine", current)
	}
	if len(variants) != 3 {
		t.Fatalf("got %d variants, want 3: %+v", len(variants), variants)
	}

	// The variant being viewed leads, so the admin sees the one the rest of
	// the draft describes first.
	if !variants[0].Current || variants[0].Label != "Deep Wine" {
		t.Errorf("first variant = %+v, want the current one", variants[0])
	}
	for _, variant := range variants {
		if variant.SKU == "" {
			t.Errorf("variant %q has no id, so its own page cannot be reached", variant.Label)
		}
		if variant.URL == "" || !strings.Contains(variant.URL, variant.SKU) {
			t.Errorf("variant %q has URL %q", variant.Label, variant.URL)
		}
		if len(variant.Images) == 0 {
			t.Errorf("variant %q has no photograph", variant.Label)
		}
	}
}

// The whole point of reading variants separately: one product must never be
// illustrated with another colourway's photographs.
func TestAmazonGalleryExcludesOtherColourways(t *testing.T) {
	doc, err := parseHTML([]byte(amazonVariantPage))
	if err != nil {
		t.Fatal(err)
	}
	draft := extractAmazon(doc, mustParse(t, "https://www.amazon.in/dp/B0B6BPTFT5"))
	if draft == nil {
		t.Fatal("extractAmazon found nothing")
	}

	for _, image := range draft.Images {
		if strings.Contains(image, "black") || strings.Contains(image, "pink") {
			t.Errorf("the Deep Wine gallery contains another colourway: %v", draft.Images)
		}
	}
	if len(draft.Images) != 2 {
		t.Errorf("gallery = %v, want the two Deep Wine photographs", draft.Images)
	}
}

func TestAmazonVariantsIgnoresASingleOption(t *testing.T) {
	page := `<html><body><span id="productTitle">One colour only</span>
	<script type="a-state">{"landingAsinColor":"Black","colorImages":{"Black":[{"hiRes":"https://m.media-amazon.com/images/I/61a._SL1500_.jpg"}]}}</script>
	</body></html>`
	doc, _ := parseHTML([]byte(page))
	_, variants, _ := amazonVariants(doc, mustParse(t, "https://www.amazon.in/dp/B000000001"))
	if len(variants) != 0 {
		t.Errorf("a listing with one option produced %d variants; a picker of one is noise", len(variants))
	}
}
