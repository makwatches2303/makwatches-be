package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// StorefrontContent is the admin-managed presentation layer for the storefront.
//
// # Why this exists
//
// The storefront must not be hard-wired to whatever the catalogue happens to
// contain today. Which products a homepage rail shows, what the hero says, which
// sections appear at all -- these are merchandising decisions the admin owns,
// not constants baked into a deployment.
//
// This is deliberately separate from models.Settings, which holds *operational*
// configuration (tax rate, payment gateways, legal policy text). This document
// holds *presentation*: copy, section toggles and product selection.
//
// It is a single document. The storefront has one published configuration at a
// time; versioning and scheduling can be layered on later without changing the
// read path.
//
// Every section carries its own Enabled flag. A disabled section does not
// render at all -- that is how the admin removes a section without a deploy,
// and how a section whose real content does not exist yet stays off the page
// rather than showing invented copy.
type StorefrontContent struct {
	ID primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`

	Navigation    NavigationContent    `json:"navigation" bson:"navigation"`
	CategoryTiles CategoryTilesContent `json:"categoryTiles" bson:"category_tiles"`

	// Listings is the header copy of the catalog listing pages -- the
	// collection, men's and women's edits. These were literals in the
	// storefront's page files, which meant the one part of those pages an
	// admin would actually want to reword was the one part that needed a
	// deploy to change.
	Listings ListingPagesContent `json:"listings" bson:"listings"`

	Hero     HeroContent     `json:"hero" bson:"hero"`
	Trust    TrustContent    `json:"trust" bson:"trust"`
	Stats    StatsContent    `json:"stats" bson:"stats"`
	Craft    CraftContent    `json:"craft" bson:"craft"`
	House    HouseContent    `json:"house" bson:"house"`
	Poster   PosterContent   `json:"poster" bson:"poster"`
	Boutique BoutiqueContent `json:"boutique" bson:"boutique"`
	Footer   FooterContent   `json:"footer" bson:"footer"`
	Marquee  MarqueeContent  `json:"marquee" bson:"marquee"`
	Policies PoliciesContent `json:"policies" bson:"policies"`

	// Rails are the product sections of the homepage, in display order. The
	// admin controls how many there are, what each is called, and which slice
	// of the catalogue each shows.
	Rails []ProductRail `json:"rails" bson:"rails"`

	UpdatedAt time.Time `json:"updatedAt" bson:"updated_at"`
	UpdatedBy string    `json:"updatedBy,omitempty" bson:"updated_by,omitempty"`
}

// HeroContent is the homepage opening statement.
//
// Imagery and the featured product come from the hero slides CMS
// (models.HeroSlide); this is the editorial copy laid over them.
type HeroContent struct {
	Enabled bool   `json:"enabled" bson:"enabled"`
	Eyebrow string `json:"eyebrow" bson:"eyebrow"`
	// One entry per rendered line, so the admin controls where the headline
	// breaks rather than leaving it to the viewport.
	HeadlineLines []string `json:"headlineLines" bson:"headline_lines"`
	Supporting    string   `json:"supporting" bson:"supporting"`
	// Nil omits the "Priced from" clause rather than quoting a price the
	// catalogue may not support.
	PricedFrom   *float64 `json:"pricedFrom,omitempty" bson:"priced_from,omitempty"`
	PrimaryCta   CtaLink  `json:"primaryCta" bson:"primary_cta"`
	SecondaryCta CtaLink  `json:"secondaryCta" bson:"secondary_cta"`
}

// ListingHeader is the editorial header of one catalog listing page: the
// small eyebrow above the headline, the headline, and the sentence under it.
//
// Deliberately copy only. Which products a listing shows is decided by its
// route's own scope (`/men` is the Men category tree, and nothing an admin
// types here can widen it); this is what the page *says* above that grid.
//
// An empty field falls back to the shipped default rather than rendering
// blank, so clearing a title in the admin cannot leave a headless page.
type ListingHeader struct {
	Eyebrow     string `json:"eyebrow" bson:"eyebrow"`
	Title       string `json:"title" bson:"title"`
	Description string `json:"description" bson:"description"`
}

// IsEmpty reports whether nothing has been authored for this header, which is
// how a document written before listing copy existed is told apart from one an
// admin has deliberately edited.
func (h ListingHeader) IsEmpty() bool {
	return h.Eyebrow == "" && h.Title == "" && h.Description == ""
}

// Or returns h with any empty field filled from fallback.
//
// Field by field rather than all-or-nothing: an admin who rewrites the
// headline and leaves the eyebrow alone gets their headline plus the shipped
// eyebrow, not a half-empty header.
func (h ListingHeader) Or(fallback ListingHeader) ListingHeader {
	if h.Eyebrow == "" {
		h.Eyebrow = fallback.Eyebrow
	}
	if h.Title == "" {
		h.Title = fallback.Title
	}
	if h.Description == "" {
		h.Description = fallback.Description
	}
	return h
}

// ListingPagesContent is the header copy for every catalog listing page whose
// heading is editorial rather than derived from data.
//
// /collections/[slug] and /category/[slug] are deliberately absent: their
// headings are the collection's and the category's own names, which come from
// the catalogue. Letting an admin override those here would create a second
// place a category is named and a way for the two to disagree.
type ListingPagesContent struct {
	// Collection is /shop -- the whole catalogue.
	Collection ListingHeader `json:"collection" bson:"collection"`
	// Categories is /collections -- the "shop by category" front doors.
	Categories ListingHeader `json:"categories" bson:"categories"`
	Men        ListingHeader `json:"men" bson:"men"`
	Women      ListingHeader `json:"women" bson:"women"`
}

// WithDefaults fills any header that has never been authored.
func (c ListingPagesContent) WithDefaults(d ListingPagesContent) ListingPagesContent {
	c.Collection = c.Collection.Or(d.Collection)
	c.Categories = c.Categories.Or(d.Categories)
	c.Men = c.Men.Or(d.Men)
	c.Women = c.Women.Or(d.Women)
	return c
}

// CtaLink is a labelled destination.
type CtaLink struct {
	Label string `json:"label" bson:"label"`
	Href  string `json:"href" bson:"href"`
}

// TrustContent is the service strip: shipping, warranty, returns.
//
// These are commitments to a customer. The section stays disabled until the
// admin enters real terms.
type TrustContent struct {
	Enabled bool     `json:"enabled" bson:"enabled"`
	Items   []string `json:"items" bson:"items"`
}

// StatsContent is the headline-figures band.
type StatsContent struct {
	Enabled bool       `json:"enabled" bson:"enabled"`
	Items   []StatItem `json:"items" bson:"items"`
}

// StatItem is one figure. Value is a free string so "52,000+" and "4.9" both
// work; CountUp animates numeric values in on scroll.
type StatItem struct {
	Value   string `json:"value" bson:"value"`
	Label   string `json:"label" bson:"label"`
	CountUp bool   `json:"countUp" bson:"count_up"`
}

// CraftContent is the sticky-scroll engineering story.
type CraftContent struct {
	Enabled bool         `json:"enabled" bson:"enabled"`
	Eyebrow string       `json:"eyebrow" bson:"eyebrow"`
	Panels  []CraftPanel `json:"panels" bson:"panels"`
}

// CraftPanel is one numbered step. Specs are technical claims about a physical
// product and must come from real MAK data.
type CraftPanel struct {
	Number string      `json:"number" bson:"number"`
	Title  string      `json:"title" bson:"title"`
	Body   string      `json:"body" bson:"body"`
	Specs  []SpecEntry `json:"specs" bson:"specs"`
	// Optional override; falls back to gallery imagery when empty.
	Image string `json:"image,omitempty" bson:"image,omitempty"`
}

// SpecEntry is a labelled figure.
type SpecEntry struct {
	Key   string `json:"key" bson:"key"`
	Value string `json:"value" bson:"value"`
}

// HouseContent is the full-bleed brand story band.
type HouseContent struct {
	Enabled bool    `json:"enabled" bson:"enabled"`
	Eyebrow string  `json:"eyebrow" bson:"eyebrow"`
	Title   string  `json:"title" bson:"title"`
	Body    string  `json:"body" bson:"body"`
	Cta     CtaLink `json:"cta" bson:"cta"`
	Image   string  `json:"image,omitempty" bson:"image,omitempty"`
}

// PosterContent is the accent-field closing band and newsletter.
type PosterContent struct {
	Enabled       bool     `json:"enabled" bson:"enabled"`
	HeadlineLines []string `json:"headlineLines" bson:"headline_lines"`
	Body          string   `json:"body" bson:"body"`
	EmailLabel    string   `json:"emailLabel" bson:"email_label"`
	SubmitLabel   string   `json:"submitLabel" bson:"submit_label"`
	Note          string   `json:"note" bson:"note"`
}

// BoutiqueContent configures the optional 3D showroom.
//
// Off by default, and deliberately so: it is an enhancement, never a
// requirement for buying anything. The route stays reachable while disabled but
// renders the plain shoppable grid, so a link to it is never broken.
//
// Which pieces it shows is a selection *rule*, exactly as a product rail is --
// Source is one of the RailSource* constants and Value parameterises it. No
// fixed product ids, so the showroom keeps up as stock turns over.
type BoutiqueContent struct {
	Enabled bool   `json:"enabled" bson:"enabled"`
	Eyebrow string `json:"eyebrow" bson:"eyebrow"`
	Title   string `json:"title" bson:"title"`
	Body    string `json:"body" bson:"body"`

	Source string `json:"source" bson:"source"`
	Value  string `json:"value,omitempty" bson:"value,omitempty"`
	Limit  int    `json:"limit" bson:"limit"`
}

// FooterContent is the footer tagline and social links.
type FooterContent struct {
	Tagline string       `json:"tagline" bson:"tagline"`
	Social  []SocialLink `json:"social" bson:"social"`
}

// SocialLink is a labelled external destination.
type SocialLink struct {
	Label string `json:"label" bson:"label"`
	Href  string `json:"href" bson:"href"`
}

// MarqueeContent is the scrolling band between sections.
type MarqueeContent struct {
	Enabled bool `json:"enabled" bson:"enabled"`
	// When true the band is built from live category names rather than Terms,
	// so it stays correct as the catalogue changes.
	UseCategoryNames bool     `json:"useCategoryNames" bson:"use_category_names"`
	Terms            []string `json:"terms" bson:"terms"`
	DurationSeconds  int      `json:"durationSeconds" bson:"duration_seconds"`
}

// PoliciesContent is the shipping/returns/warranty copy shown on every product
// page. Disabled panels do not render.
type PoliciesContent struct {
	Shipping    PolicyPanel `json:"shipping" bson:"shipping"`
	Returns     PolicyPanel `json:"returns" bson:"returns"`
	Warranty    PolicyPanel `json:"warranty" bson:"warranty"`
	BoxContents BoxContents `json:"boxContents" bson:"box_contents"`
}

// PolicyPanel is one accordion panel on the product page.
type PolicyPanel struct {
	Enabled bool   `json:"enabled" bson:"enabled"`
	Title   string `json:"title" bson:"title"`
	Body    string `json:"body" bson:"body"`
}

// BoxContents is the catalogue-wide "what's included" list. A product's own
// specs.boxContents takes precedence over this.
type BoxContents struct {
	Enabled bool     `json:"enabled" bson:"enabled"`
	Title   string   `json:"title" bson:"title"`
	Items   []string `json:"items" bson:"items"`
}

// ── Navigation ──────────────────────────────────────────────────────────────

// Navigation item kinds. Kind describes what the item points at; Value
// parameterises the kinds that need it. A plain "link" needs only Href.
//
// The richer kinds are declared now so the schema does not have to change when
// the admin UI grows category and collection pickers -- but only "link" is
// resolved today. An unrecognised kind falls back to Href, so an item written
// by a newer admin build never disappears from an older storefront.
const (
	NavKindLink       = "link"
	NavKindCategory   = "category"
	NavKindCollection = "collection"
	NavKindExternal   = "external"
	NavKindPromo      = "promo"
)

// NavItem is one entry in a navigation menu.
//
// Deliberately not a mega-menu CMS. It carries the minimum the storefront needs
// today (id, label, href, enabled, order) plus the fields a richer menu will
// need, so adding dropdowns or a category picker later is an admin-UI change
// rather than a schema migration.
type NavItem struct {
	ID      string `json:"id" bson:"id"`
	Label   string `json:"label" bson:"label"`
	Href    string `json:"href" bson:"href"`
	Enabled bool   `json:"enabled" bson:"enabled"`
	Order   int    `json:"order" bson:"order"`

	// Kind is one of the NavKind* constants. Empty means NavKindLink.
	Kind string `json:"kind,omitempty" bson:"kind,omitempty"`
	// Value parameterises Kind -- a category name, a collection slug.
	Value string `json:"value,omitempty" bson:"value,omitempty"`

	// External opens in a new tab and adds the required rel.
	External bool `json:"external,omitempty" bson:"external,omitempty"`
	// Badge is a short promotional label, e.g. "New".
	Badge string `json:"badge,omitempty" bson:"badge,omitempty"`

	// Children backs future dropdowns. Rendered flat today.
	Children []NavItem `json:"children,omitempty" bson:"children,omitempty"`
}

// FooterColumn is one titled column of footer links.
type FooterColumn struct {
	ID      string    `json:"id" bson:"id"`
	Heading string    `json:"heading" bson:"heading"`
	Enabled bool      `json:"enabled" bson:"enabled"`
	Order   int       `json:"order" bson:"order"`
	Items   []NavItem `json:"items" bson:"items"`
}

// NavigationContent is every menu the storefront renders.
//
// Primary is the header nav and the top of the mobile menu; Support is the
// customer-care list in the mobile menu; Footer is the link columns.
type NavigationContent struct {
	Primary []NavItem      `json:"primary" bson:"primary"`
	Support []NavItem      `json:"support" bson:"support"`
	Footer  []FooterColumn `json:"footer" bson:"footer"`
}

// ── Category merchandising ──────────────────────────────────────────────────

// Category tile sources. A tile references the live category tree by name; it
// never stores product ids.
const (
	TileSourceCategory    = "category"
	TileSourceSubcategory = "subcategory"
)

// CategoryTileSeparator joins a parent and child in a subcategory reference:
// "Men > Gold watch".
const CategoryTileSeparator = " > "

// CategoryTile is one merchandised tile in the "shop by category" grid.
//
// It holds a *reference* into the live category tree, not a copy of it, so the
// tile keeps resolving as categories are renamed or re-imaged in the admin. It
// deliberately holds no product ids: which categories to show is a separate
// decision from which products a rail selects.
//
// A reference that no longer resolves is omitted from the storefront rather
// than crashing or silently swapping in a different category -- see
// ResolveCategoryTiles.
type CategoryTile struct {
	ID      string `json:"id" bson:"id"`
	Enabled bool   `json:"enabled" bson:"enabled"`
	Order   int    `json:"order" bson:"order"`

	// Source is one of the TileSource* constants.
	Source string `json:"source" bson:"source"`
	// Value names the category: "Men", or "Men > Gold watch" for a subcategory.
	Value string `json:"value" bson:"value"`

	// Optional overrides. Empty falls back to the live category's own values.
	Label    string `json:"label,omitempty" bson:"label,omitempty"`
	Subtitle string `json:"subtitle,omitempty" bson:"subtitle,omitempty"`
	Image    string `json:"image,omitempty" bson:"image,omitempty"`
	Href     string `json:"href,omitempty" bson:"href,omitempty"`
}

// CategoryTilesContent is the "shop by category" section.
type CategoryTilesContent struct {
	Enabled bool   `json:"enabled" bson:"enabled"`
	Eyebrow string `json:"eyebrow" bson:"eyebrow"`
	Title   string `json:"title" bson:"title"`

	// AutoFromCategories derives one tile per subcategory from the live tree,
	// in tree order. This is the default: a fresh install shows the whole
	// catalogue without anyone curating it first, and the section stays correct
	// as categories are added.
	//
	// It is itself a configurable rule, not frontend logic. Turning it off
	// hands full control to Tiles below.
	AutoFromCategories bool `json:"autoFromCategories" bson:"auto_from_categories"`

	// Tiles is the curated selection, used when AutoFromCategories is false.
	Tiles []CategoryTile `json:"tiles" bson:"tiles"`
}

// Product rail sources. These name a *rule*, not a fixed list of ids, so a rail
// keeps working as the catalogue changes rather than pinning today's products.
const (
	RailSourceLatest      = "latest"
	RailSourceFeatured    = "featured"
	RailSourceBestseller  = "bestseller"
	RailSourceNewArrival  = "newArrival"
	RailSourceCategory    = "category"
	RailSourceSubcategory = "subcategory"
	RailSourceCollection  = "collection"
)

// ProductRail is one product section of the homepage.
//
// Source names the selection rule and Value parameterises it, so the admin
// changes what a rail shows without anyone editing code or pinning product ids
// that will go out of stock.
type ProductRail struct {
	ID       string `json:"id" bson:"id"`
	Enabled  bool   `json:"enabled" bson:"enabled"`
	Position int    `json:"position" bson:"position"`

	Eyebrow string `json:"eyebrow" bson:"eyebrow"`
	Title   string `json:"title" bson:"title"`

	// Source is one of the RailSource* constants; Value parameterises the ones
	// that need it (a category name, a collection name).
	Source string `json:"source" bson:"source"`
	Value  string `json:"value,omitempty" bson:"value,omitempty"`

	Limit int `json:"limit" bson:"limit"`

	// Render the chip/sort controls, as the reference's collection band does.
	Filterable bool `json:"filterable" bson:"filterable"`

	ViewAll CtaLink `json:"viewAll" bson:"view_all"`

	// Alternate ground, for rhythm between adjacent rails.
	Tone string `json:"tone,omitempty" bson:"tone,omitempty"`
}

// DefaultStorefrontContent is what the storefront renders before an admin has
// configured anything.
//
// Sections whose content would be a factual claim about the business -- service
// terms, company statistics, manufacturing specifications -- default to
// disabled. The storefront therefore ships truthful and incomplete rather than
// complete and invented, and the admin turns each on as real copy arrives.
func DefaultStorefrontContent() StorefrontContent {
	return StorefrontContent{
		// The wording the storefront's page files used to hardcode. Kept
		// verbatim so introducing this section changed nothing visible: the
		// pages render exactly what they did until an admin edits them.
		Listings: ListingPagesContent{
			Collection: ListingHeader{
				Eyebrow:     "The collection",
				Title:       "Every watch we make.",
				Description: "The complete MAK catalogue. Filter by brand, price and availability.",
			},
			Categories: ListingHeader{
				Eyebrow:     "Shop by category",
				Title:       "Shop by Category",
				Description: "Find the right timepiece for every style, occasion and generation.",
			},
			Men: ListingHeader{
				Eyebrow: "For him",
				Title:   "The men's edit.",
			},
			Women: ListingHeader{
				Eyebrow: "For her",
				Title:   "The women's edit.",
			},
		},
		// Reproduces the reference navigation exactly, so moving it out of the
		// frontend loses no functionality. Every entry is now data the admin
		// can relabel, reorder, disable or repoint without a deploy.
		Navigation: NavigationContent{
			Primary: []NavItem{
				{ID: "collection", Label: "Collection", Href: "/shop", Enabled: true, Order: 1},
				{ID: "categories", Label: "Categories", Href: "/collections", Enabled: true, Order: 2},
				{ID: "craft", Label: "Craft", Href: "/craft", Enabled: false, Order: 3},
				{ID: "house", Label: "House", Href: "/about", Enabled: true, Order: 4},
				// Shipped off. The route exists and renders the shoppable
				// fallback, so switching this on is never a broken link.
				{ID: "boutique", Label: "Boutique", Href: "/boutique", Enabled: false, Order: 5},
				{ID: "men", Label: "Men", Href: "/men", Enabled: true, Order: 6},
				{ID: "women", Label: "Women", Href: "/women", Enabled: true, Order: 7},
			},
			Support: []NavItem{
				{ID: "shipping", Label: "Shipping", Href: "/shipping", Enabled: true, Order: 1},
				{ID: "returns", Label: "Returns", Href: "/refund", Enabled: true, Order: 2},
				{ID: "contact", Label: "Contact", Href: "/contact", Enabled: true, Order: 3},
				{ID: "track", Label: "Track order", Href: "/orders", Enabled: true, Order: 4},
			},
			Footer: []FooterColumn{
				{ID: "shop", Heading: "Shop", Enabled: true, Order: 1, Items: []NavItem{
					{ID: "all", Label: "All watches", Href: "/shop", Enabled: true, Order: 1},
					{ID: "men", Label: "Men", Href: "/men", Enabled: true, Order: 2},
					{ID: "women", Label: "Women", Href: "/women", Enabled: true, Order: 3},
					{ID: "collections", Label: "Collections", Href: "/collections", Enabled: true, Order: 4},
				}},
				{ID: "house", Heading: "House", Enabled: true, Order: 2, Items: []NavItem{
					{ID: "about", Label: "About", Href: "/about", Enabled: true, Order: 1},
					{ID: "blog", Label: "Blog", Href: "/blog", Enabled: true, Order: 2},
					{ID: "contact", Label: "Contact", Href: "/contact", Enabled: true, Order: 3},
				}},
				{ID: "care", Heading: "Care", Enabled: true, Order: 3, Items: []NavItem{
					{ID: "shipping", Label: "Shipping", Href: "/shipping", Enabled: true, Order: 1},
					{ID: "returns", Label: "Returns", Href: "/refund", Enabled: true, Order: 2},
					{ID: "track", Label: "Track order", Href: "/orders", Enabled: true, Order: 3},
				}},
				{ID: "legal", Heading: "Legal", Enabled: true, Order: 4, Items: []NavItem{
					{ID: "privacy", Label: "Privacy", Href: "/privacy", Enabled: true, Order: 1},
					{ID: "terms", Label: "Terms", Href: "/terms", Enabled: true, Order: 2},
					{ID: "refunds", Label: "Refunds", Href: "/refund", Enabled: true, Order: 3},
				}},
			},
		},

		// Defaults to deriving tiles from the live category tree, which is
		// exactly today's behaviour -- but as a rule the admin can turn off in
		// favour of a curated, ordered selection. No category name is baked in.
		CategoryTiles: CategoryTilesContent{
			Enabled:            true,
			Eyebrow:            "Shop by category",
			Title:              "Find your movement.",
			AutoFromCategories: true,
			Tiles:              []CategoryTile{},
		},

		Hero: HeroContent{
			Enabled:       true,
			Eyebrow:       "The precision house",
			HeadlineLines: []string{"Time,", "Engineered."},
			Supporting:    "A curated house of mechanical and quartz timepieces — from your first everyday watch to a collector's grail.",
			PrimaryCta:    CtaLink{Label: "Shop the collection", Href: "/shop"},
			SecondaryCta:  CtaLink{Label: "Explore categories", Href: "/collections"},
		},
		Trust: TrustContent{Enabled: false, Items: []string{}},
		Stats: StatsContent{Enabled: false, Items: []StatItem{}},
		// ⚠️ REFERENCE DEMO VALUES, enabled at the client's explicit request so
		// the section matches the approved design out of the box.
		//
		// Every specification below is carried over verbatim from the reference
		// prototype, which describes a fictional watch. They are NOT MAK's real
		// figures and must be replaced through the admin before publishing.
		Craft: CraftContent{
			Enabled: true,
			Eyebrow: "The craft",
			Panels: []CraftPanel{
				{
					Number: "01",
					Title:  "A movement you can trust",
					Body:   "Regulated in five positions and timed to within seconds a day. Every calibre is run for a full week before it ships.",
					Specs: []SpecEntry{
						{Key: "Accuracy", Value: "±5s/day"},
						{Key: "Reserve", Value: "41 hours"},
					},
				},
				{
					Number: "02",
					Title:  "A case built to outlast you",
					Body:   "Brushed and polished steel, sapphire crystal, and a screw-down crown — sealed by hand and pressure-tested for water resistance.",
					Specs: []SpecEntry{
						{Key: "Crystal", Value: "Sapphire"},
						{Key: "Water", Value: "50–600m"},
					},
				},
				{
					Number: "03",
					Title:  "Finishing under a loupe",
					Body:   "Bevelled edges, brushed dials and applied indices, inspected under magnification before the piece earns the MAK mark.",
					Specs: []SpecEntry{
						{Key: "Warranty", Value: "5 years"},
						{Key: "Made", Value: "In-house"},
					},
				},
			},
		},
		// "About MAK Watches", not "The MAK house". MAK is a retailer; a "house"
		// is what a maison calls itself, and the word reads as a claim to make
		// the watches. Only the shipped default changes here -- a stored value
		// an admin has written still wins on read.
		House: HouseContent{Enabled: false, Eyebrow: "About MAK Watches"},
		Poster: PosterContent{
			Enabled:       true,
			HeadlineLines: []string{"Join the", "list."},
			Body:          "Be first to hear when new pieces land.",
			EmailLabel:    "Your email",
			SubmitLabel:   "Notify me",
			Note:          "We will only email you about new arrivals.",
		},
		// Shipped switched off. It is an optional enhancement, and turning it
		// on for everyone by default would put a WebGL scene in front of
		// customers who never asked for one.
		Boutique: BoutiqueContent{
			Enabled: false,
			Eyebrow: "The boutique",
			Title:   "Step inside.",
			Body:    "A room you can walk around. Every piece here is the same piece you can buy from any other page.",
			Source:  RailSourceLatest,
			Limit:   6,
		},
		Footer: FooterContent{
			Tagline: "Precision timepieces, engineered for the people who measure their days.",
			Social:  []SocialLink{},
		},
		Marquee: MarqueeContent{
			Enabled:          true,
			UseCategoryNames: true,
			Terms:            []string{},
			DurationSeconds:  32,
		},
		Policies: PoliciesContent{
			Shipping:    PolicyPanel{Enabled: false, Title: "Shipping"},
			Returns:     PolicyPanel{Enabled: false, Title: "Returns"},
			Warranty:    PolicyPanel{Enabled: false, Title: "Warranty"},
			BoxContents: BoxContents{Enabled: false, Title: "What's included", Items: []string{}},
		},
		// Rails default to rules, never to pinned ids: "the newest in stock",
		// "the men's edit". They stay correct as the catalogue turns over.
		Rails: []ProductRail{
			{
				ID: "collection", Enabled: true, Position: 1,
				Eyebrow: "The collection", Title: "Every watch we make.",
				Source: RailSourceLatest, Limit: 8, Filterable: true,
				ViewAll: CtaLink{Label: "View the full collection", Href: "/shop"},
			},
			{
				ID: "men", Enabled: true, Position: 2,
				Eyebrow: "For him", Title: "The men's edit.",
				Source: RailSourceCategory, Value: "Men", Limit: 4,
				ViewAll: CtaLink{Label: "Shop men's watches", Href: "/men"},
			},
			{
				ID: "women", Enabled: true, Position: 3,
				Eyebrow: "For her", Title: "The women's edit.",
				Source: RailSourceCategory, Value: "Women", Limit: 4,
				ViewAll: CtaLink{Label: "Shop women's watches", Href: "/women"},
				Tone:    "surface",
			},
		},
		UpdatedAt: time.Now(),
	}
}

// ── Backward compatibility ──────────────────────────────────────────────────

// WithDefaults fills sections a stored document predates.
//
// A document written before navigation and category tiles existed decodes with
// those fields empty. Filling them from the shipped defaults means an older
// stored configuration keeps rendering a complete storefront instead of losing
// its menus -- so no migration is needed, and rolling the API back is safe.
func (c StorefrontContent) WithDefaults() StorefrontContent {
	d := DefaultStorefrontContent()

	if len(c.Navigation.Primary) == 0 {
		c.Navigation.Primary = d.Navigation.Primary
	}
	if len(c.Navigation.Support) == 0 {
		c.Navigation.Support = d.Navigation.Support
	}
	if len(c.Navigation.Footer) == 0 {
		c.Navigation.Footer = d.Navigation.Footer
	}

	// A category-tiles block that decoded entirely empty predates the field.
	// Distinguishable from a deliberately emptied one, which still carries a
	// title or an explicit tile list.
	if c.CategoryTiles.Eyebrow == "" && c.CategoryTiles.Title == "" && len(c.CategoryTiles.Tiles) == 0 {
		c.CategoryTiles = d.CategoryTiles
	}

	if len(c.Rails) == 0 {
		c.Rails = d.Rails
	}

	// Per header, not per section: a document written before listing copy
	// existed has all three empty, and one an admin has partly edited keeps
	// every field they filled in.
	c.Listings = c.Listings.WithDefaults(d.Listings)

	// A boutique block with no source at all predates the field. A deliberately
	// disabled one still carries its source and copy, so the two are
	// distinguishable and switching it off survives a reload.
	if c.Boutique.Source == "" && c.Boutique.Title == "" {
		c.Boutique = d.Boutique
	}

	return c
}

// enabledNav returns the usable items of a menu, in the admin's order.
//
// An item with no destination is dropped: it would render as an inert label.
func enabledNav(items []NavItem) []NavItem {
	out := make([]NavItem, 0, len(items))
	for _, item := range items {
		if !item.Enabled || item.Href == "" {
			continue
		}
		item.Children = enabledNav(item.Children)
		out = append(out, item)
	}
	sortNavByOrder(out)
	return out
}

func sortNavByOrder(items []NavItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Order < items[j-1].Order; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// PublicNavigation returns only the entries the storefront should render.
//
// Filtering here rather than in the browser means a disabled or unfinished menu
// item is never shipped to the client at all.
func (n NavigationContent) PublicNavigation() NavigationContent {
	columns := make([]FooterColumn, 0, len(n.Footer))
	for _, column := range n.Footer {
		if !column.Enabled {
			continue
		}
		column.Items = enabledNav(column.Items)
		if len(column.Items) == 0 {
			// A heading with no links is noise.
			continue
		}
		columns = append(columns, column)
	}
	for i := 1; i < len(columns); i++ {
		for j := i; j > 0 && columns[j].Order < columns[j-1].Order; j-- {
			columns[j], columns[j-1] = columns[j-1], columns[j]
		}
	}

	return NavigationContent{
		Primary: enabledNav(n.Primary),
		Support: enabledNav(n.Support),
		Footer:  columns,
	}
}
