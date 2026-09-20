// Package catalog holds the read-side catalog logic that the /api/v1 handlers
// build on, so query construction lives here rather than being inlined in HTTP
// handlers as it is in the legacy flat routes.
//
// The legacy handlers are deliberately left untouched: they keep serving the
// existing storefront and admin app unchanged while the new routes are built
// against this service.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageproc"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageurl"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// ErrNotFound is returned when a lookup matches no document.
var ErrNotFound = errors.New("catalog: not found")

// Service provides read access to the product catalog.
type Service struct {
	db     *database.DBClient
	bucket string
	// media verifies that a resolved image reference actually exists in the
	// bucket. Optional: a nil index reports every object as present.
	media *mediaindex.Index
}

// New creates a catalog service.
//
// bucket is the Firebase Storage bucket used to resolve stored image
// references to canonical URLs. media may be nil, in which case references are
// emitted without an existence check.
func New(db *database.DBClient, bucket string, media *mediaindex.Index) *Service {
	return &Service{db: db, bucket: bucket, media: media}
}

// Query describes a catalog listing request.
type Query struct {
	Category       string
	MainCategory   string
	Subcategory    string
	Collection     string
	VariantGroupID string
	Gender         string
	Search         string

	// Attribute facets, mirroring the fields the products collection carries.
	// Brand is multi-select; the rest are single-select.
	Brands        []string
	DialColor     string
	DialShape     string
	DialType      string
	StrapColor    string
	StrapMaterial string
	Style         string
	DialThickness string
	MinPrice      *float64
	MaxPrice      *float64
	InStock       bool
	Featured      bool
	NewArrival    bool
	Bestseller    bool
	SortBy        string
	Order         string
	Page          int
	Limit         int

	// Cursor requests the batch after a previous one, by keyset rather than by
	// offset. When set, Page is ignored. See cursor.go.
	Cursor string
}

// Page is a listing result.
//
// It serves both pagination modes. A page request carries Total and Pages; a
// cursor request carries NextCursor and leaves the totals unset, because
// counting the whole result set on every batch of an infinite scroll is a
// collection scan the shopper never sees. TotalKnown says which it is, so a
// caller cannot mistake "not counted" for "none".
type Page struct {
	Items []models.Product `json:"items"`
	Page  int              `json:"page"`
	Limit int              `json:"limit"`
	Total int64            `json:"total"`
	Pages int64            `json:"pages"`
	// TotalKnown is false on cursor requests, where Total and Pages are unset.
	TotalKnown bool `json:"totalKnown"`
	// NextCursor is the cursor for the following batch, empty at the end.
	NextCursor string `json:"nextCursor,omitempty"`
	// HasMore reports whether another batch exists.
	HasMore bool `json:"hasMore"`
}

const (
	defaultLimit = 24
	maxLimit     = 100
)

// normalize clamps pagination and defaults sorting.
func (q *Query) normalize() {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Limit < 1 {
		q.Limit = defaultLimit
	}
	if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	switch q.SortBy {
	case "price", "name", "createdAt":
	default:
		q.SortBy = "createdAt"
	}
	if !strings.EqualFold(q.Order, "asc") {
		q.Order = "desc"
	}
}

// filter builds the Mongo filter for a query.
//
// Published-status handling matches models.IsPublished: a record with no
// `status` field predates publication state and stays visible, so the filter
// admits both "missing" and "published" rather than requiring the new field.
func (s *Service) filter(q Query) bson.M {
	f := bson.M{
		"$or": []bson.M{
			{"status": bson.M{"$exists": false}},
			{"status": ""},
			{"status": models.ProductStatusPublished},
		},
	}

	// Category scoping.
	//
	// The catalog stores a composite path in `category` ("Men — Gold watch")
	// alongside a dedicated `subcategory` field. The separator is an em dash,
	// not a slash, and `main_category` is unreliable -- some records hold the
	// full path in it. So the top level is matched as a prefix of `category`,
	// and the subcategory against its own field.
	//
	// Subcategory values carry inconsistent casing and stray trailing spaces
	// ("Leather watch "), hence the anchored, whitespace-tolerant,
	// case-insensitive match rather than equality.
	if q.Category != "" {
		f["category"] = q.Category
	} else {
		if q.MainCategory != "" {
			f["category"] = bson.M{"$regex": "^" + regexEscape(q.MainCategory)}
		}
		if q.Subcategory != "" {
			f["subcategory"] = bson.M{
				"$regex":   `^\s*` + regexEscape(q.Subcategory) + `\s*$`,
				"$options": "i",
			}
		}
	}

	if q.Collection != "" {
		f["collection"] = q.Collection
	}
	if q.VariantGroupID != "" {
		f["variant_group_id"] = q.VariantGroupID
	}
	if q.Gender != "" {
		f["gender"] = q.Gender
	}

	// Brand is multi-select: one value matches exactly, several use $in.
	if len(q.Brands) == 1 {
		f["brand"] = q.Brands[0]
	} else if len(q.Brands) > 1 {
		f["brand"] = bson.M{"$in": q.Brands}
	}

	// Single-select attribute facets. The bson field names are snake_case; see
	// models.Product.
	for field, value := range map[string]string{
		"dial_color":     q.DialColor,
		"dial_shape":     q.DialShape,
		"dial_type":      q.DialType,
		"strap_color":    q.StrapColor,
		"strap_material": q.StrapMaterial,
		"style":          q.Style,
		"dial_thickness": q.DialThickness,
	} {
		if value != "" {
			f[field] = value
		}
	}
	if q.InStock {
		f["stock"] = bson.M{"$gt": 0}
	}
	if q.Featured {
		f["featured"] = true
	}
	if q.NewArrival {
		f["new_arrival"] = true
	}
	if q.Bestseller {
		f["bestseller"] = true
	}

	if q.MinPrice != nil || q.MaxPrice != nil {
		price := bson.M{}
		if q.MinPrice != nil {
			price["$gte"] = *q.MinPrice
		}
		if q.MaxPrice != nil {
			price["$lte"] = *q.MaxPrice
		}
		f["price"] = price
	}

	if term := strings.TrimSpace(q.Search); term != "" {
		rx := bson.M{"$regex": regexEscape(term), "$options": "i"}
		// $and-wrapped so this does not collide with the status $or above.
		and(f, bson.M{
			"$or": []bson.M{
				{"name": rx},
				{"brand": rx},
				{"category": rx},
				{"collection": rx},
				{"short_description": rx},
			},
		})
	}

	return f
}

// and appends a condition to a filter's $and, creating it if absent.
//
// Both the status test and the free-text search are already $or clauses, and a
// keyset seek is a third. A bson.M holds one "$or" key, so the only way to
// carry several is to move them under $and -- assigning a second "$or" would
// silently drop the first.
func and(f bson.M, cond bson.M) {
	existing, _ := f["$and"].([]bson.M)
	f["$and"] = append(existing, cond)
}

// sortFieldFor maps the API's sort key to its bson field. The query is
// normalized before this is called, so the default is unreachable in practice
// and exists only so the function is total.
func sortFieldFor(sortBy string) string {
	switch sortBy {
	case "price":
		return "price"
	case "name":
		return "name"
	default:
		return "created_at"
	}
}

// sortValueOf reads a product's value for the given sort field. This is what
// goes into the next cursor, so it must be the same value the sort ordered by.
func sortValueOf(p *models.Product, sortBy string) any {
	switch sortBy {
	case "price":
		return p.Price
	case "name":
		return p.Name
	default:
		return p.CreatedAt
	}
}

// listingProjection drops the fields a listing never draws.
//
// Only `seo` qualifies. It is per-product metadata used solely by the product
// page's <head>, so a grid of 24 carries 24 copies of text nothing renders.
// Everything else the catalogue holds is genuinely on screen: the card shows
// the movement from `specs`, and Quick view shows `description`. Projecting
// those away would save more bytes and break both.
func listingProjection() bson.M {
	return bson.M{"seo": 0}
}

// List returns a batch of products matching the query.
//
// Two modes, one query builder. Without a cursor this is the original
// page/skip listing, unchanged for every caller that still uses it. With a
// cursor it is a keyset seek: the batch after a named document, at a cost that
// does not grow with how far the shopper has scrolled. See cursor.go.
func (s *Service) List(ctx context.Context, q Query) (*Page, error) {
	q.normalize()

	coll := s.db.Collections().Products
	f := s.filter(q)

	descending := !strings.EqualFold(q.Order, "asc")
	dir := -1
	if !descending {
		dir = 1
	}
	sortField := sortFieldFor(q.SortBy)

	// A cursor is validated against the query it arrived with, so one issued
	// for a different scope, filter or sort is refused rather than quietly
	// windowing the wrong catalogue.
	var cursor *Cursor
	if q.Cursor != "" {
		decoded, err := decodeCursor(q.Cursor, q)
		if err != nil {
			return nil, err
		}
		cursor = decoded
		and(f, keysetFilter(sortField, descending, *cursor))
	}

	// Counting is a full scan of the matching set. A page request needs it to
	// draw page numbers; a cursor request does not, and paying for it on every
	// batch of an infinite scroll would be the most expensive part of it.
	var total int64
	totalKnown := cursor == nil
	if totalKnown {
		var err error
		// The count must not see the keyset predicate, which is why it reads
		// the filter before one is appended -- but there is none in this
		// branch by construction.
		total, err = coll.CountDocuments(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("catalog: count products: %w", err)
		}
	}

	// The _id tie-break makes the order total. Without it, documents sharing a
	// created_at come back in whatever order the storage engine likes, and two
	// requests may disagree -- which is how a product shows up twice, or never,
	// across a boundary. It matters for skip paging too, so it is applied to
	// both modes rather than only to the cursor path.
	sort := bson.D{{Key: sortField, Value: dir}, {Key: "_id", Value: dir}}

	// One extra document answers "is there another batch?" without a second
	// query. It is dropped before the batch is returned.
	fetch := int64(q.Limit) + 1

	opts := options.Find().
		SetLimit(fetch).
		SetSort(sort).
		SetProjection(listingProjection())

	// Skip belongs to page mode only. A cursor has already seeked.
	if cursor == nil {
		opts.SetSkip(int64((q.Page - 1) * q.Limit))
	}

	dbCursor, err := coll.Find(ctx, f, opts)
	if err != nil {
		return nil, fmt.Errorf("catalog: find products: %w", err)
	}
	defer dbCursor.Close(ctx)

	fetched := []models.Product{}
	if err := dbCursor.All(ctx, &fetched); err != nil {
		return nil, fmt.Errorf("catalog: decode products: %w", err)
	}

	page, err := assemble(q, fetched, total, totalKnown)
	if err != nil {
		return nil, err
	}

	for i := range page.Items {
		s.resolveMedia(ctx, &page.Items[i])
	}

	return page, nil
}

// assemble turns the raw find output into a listing result.
//
// Split from List, and free of both Mongo and media resolution, because this
// is the part with the arithmetic: where the over-fetched document is dropped,
// what "is there more" means in each mode, and which document the next cursor
// names. A listing that stops one batch early is a bug with no stack trace,
// so it is worth being able to test the boundary directly.
//
// fetched is the batch plus at most one extra document; see List.
func assemble(q Query, fetched []models.Product, total int64, totalKnown bool) (*Page, error) {
	// The extra document is the whole "is there another batch" signal in
	// cursor mode: if the query could produce one more than was asked for,
	// there is more to come.
	overfetched := len(fetched) > q.Limit
	items := fetched
	if overfetched {
		items = fetched[:q.Limit]
	}

	page := &Page{
		Items:      items,
		Page:       q.Page,
		Limit:      q.Limit,
		TotalKnown: totalKnown,
		HasMore:    overfetched,
	}

	if totalKnown {
		page.Total = total
		if q.Limit > 0 {
			page.Pages = (total + int64(q.Limit) - 1) / int64(q.Limit)
		}
		// In page mode the count is the stronger signal: it knows about pages
		// beyond this one, which the single extra document does not.
		page.HasMore = int64(q.Page)*int64(q.Limit) < total
	}

	// The cursor names the last document actually returned, so the next batch
	// resumes exactly where this one stopped.
	//
	// Minted from the over-fetch rather than from HasMore, because those can
	// disagree: page 3 of a 37-page listing has more to come by the count, and
	// its cursor is only meaningful if this batch really did have a document
	// after it.
	if overfetched && len(items) > 0 {
		last := items[len(items)-1]
		next, err := encodeCursor(q, sortValueOf(&last, q.SortBy), last.ID)
		if err != nil {
			return nil, err
		}
		page.NextCursor = next
	}

	return page, nil
}

// GetByID returns one product by its ObjectID hex string.
func (s *Service) GetByID(ctx context.Context, id string) (*models.Product, error) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return s.findOne(ctx, bson.M{"_id": oid})
}

// GetBySlug returns one product by its slug.
//
// Slugs are only present on records that have been through the additive slug
// backfill, so an unmigrated catalog simply returns ErrNotFound here and the
// caller falls back to the id route.
func (s *Service) GetBySlug(ctx context.Context, slug string) (*models.Product, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrNotFound
	}
	return s.findOne(ctx, bson.M{"slug": slug})
}

func (s *Service) findOne(ctx context.Context, f bson.M) (*models.Product, error) {
	var p models.Product
	err := s.db.Collections().Products.FindOne(ctx, f).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: find product: %w", err)
	}

	s.resolveMedia(ctx, &p)
	return &p, nil
}

// Collection is an editorial grouping derived from the products that reference it.
type Collection struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// ListCollections returns the distinct collections present in the catalog.
//
// Collections are derived from product records rather than stored in their own
// table: nothing owns a collection yet, so inventing a collections table now
// would mean inventing its contents too. A dedicated model can replace this
// once real collection data exists.
func (s *Service) ListCollections(ctx context.Context) ([]Collection, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"collection": bson.M{"$exists": true, "$nin": []interface{}{"", nil}},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":   "$collection",
			"count": bson.M{"$sum": 1},
		}}},
		{{Key: "$sort", Value: bson.M{"_id": 1}}},
	}

	cursor, err := s.db.Collections().Products.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("catalog: aggregate collections: %w", err)
	}
	defer cursor.Close(ctx)

	var rows []struct {
		Name  string `bson:"_id"`
		Count int64  `bson:"count"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("catalog: decode collections: %w", err)
	}

	out := make([]Collection, 0, len(rows))
	for _, r := range rows {
		out = append(out, Collection{
			Slug:  models.Slugify(r.Name),
			Name:  r.Name,
			Count: r.Count,
		})
	}
	return out, nil
}

// VariantSummary is the reduced shape returned for a product's sibling
// colorways -- identity, thumbnail and stock only. Deliberately excludes
// price/discount fields: replicating those here would create a second place
// discount math could drift from Product.GetFinalPrice()/the frontend's
// effectivePrice(), and each sibling's own product page already computes its
// own price correctly when you land on it.
type VariantSummary struct {
	ID           string `json:"id"`
	Slug         string `json:"slug,omitempty"`
	Name         string `json:"name"`
	VariantLabel string `json:"variantLabel,omitempty"`
	Thumbnail    string `json:"thumbnail,omitempty"`
	InStock      bool   `json:"inStock"`
}

func toVariantSummary(p models.Product) VariantSummary {
	return VariantSummary{
		ID:           p.ID.Hex(),
		Slug:         p.Slug,
		Name:         p.Name,
		VariantLabel: p.VariantLabel,
		Thumbnail:    p.Thumbnail,
		InStock:      p.Stock > 0,
	}
}

// ListVariants returns the sibling products sharing a variant group.
//
// Callers pass the groupID from a product they've already loaded (every
// product carrying VariantGroupID exposes it on the wire), so this never
// needs its own lookup of the source product the way a /products/:id/variants
// route would.
func (s *Service) ListVariants(ctx context.Context, groupID string) ([]VariantSummary, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return []VariantSummary{}, nil
	}
	page, err := s.List(ctx, Query{VariantGroupID: groupID, Limit: maxLimit, SortBy: "name", Order: "asc"})
	if err != nil {
		return nil, fmt.Errorf("catalog: list variants: %w", err)
	}
	out := make([]VariantSummary, 0, len(page.Items))
	for _, p := range page.Items {
		out = append(out, toVariantSummary(p))
	}
	return out, nil
}

// SearchResult groups the different things a query can match.
type SearchResult struct {
	Query       string           `json:"query"`
	Products    []models.Product `json:"products"`
	Collections []Collection     `json:"collections"`
	Categories  []string         `json:"categories"`
	Total       int64            `json:"total"`
}

// Search runs a storefront search across products, collections and categories.
func (s *Service) Search(ctx context.Context, term string, limit int) (*SearchResult, error) {
	term = strings.TrimSpace(term)
	result := &SearchResult{
		Query:       term,
		Products:    []models.Product{},
		Collections: []Collection{},
		Categories:  []string{},
	}
	if term == "" {
		return result, nil
	}

	page, err := s.List(ctx, Query{Search: term, Limit: limit})
	if err != nil {
		return nil, err
	}
	result.Products = page.Items
	result.Total = page.Total

	// Collection and category suggestions are filtered from the catalog's own
	// vocabulary, so a suggestion always leads somewhere with results.
	lower := strings.ToLower(term)

	collections, err := s.ListCollections(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range collections {
		if strings.Contains(strings.ToLower(c.Name), lower) {
			result.Collections = append(result.Collections, c)
		}
	}

	categories, err := s.distinctStrings(ctx, "category")
	if err != nil {
		return nil, err
	}
	for _, c := range categories {
		if strings.Contains(strings.ToLower(c), lower) {
			result.Categories = append(result.Categories, c)
		}
	}

	return result, nil
}

func (s *Service) distinctStrings(ctx context.Context, field string) ([]string, error) {
	values, err := s.db.Collections().Products.Distinct(ctx, field, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("catalog: distinct %s: %w", field, err)
	}

	out := make([]string, 0, len(values))
	for _, v := range values {
		if str, ok := v.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out, nil
}

// resolveMedia rewrites stored image references to canonical Firebase Storage
// URLs and backfills the structured Media slice from the legacy Images field,
// so new clients can consume Media exclusively while old records still work.
//
// This mutates only the in-memory copy; nothing is written back to Mongo.
func (s *Service) resolveMedia(ctx context.Context, p *models.Product) {
	p.ImageURL = imageurl.Resolve(p.ImageURL, s.bucket)
	p.Images = imageurl.ResolveAll(p.Images, s.bucket)

	for i := range p.Media {
		p.Media[i].URL = imageurl.Resolve(p.Media[i].URL, s.bucket)
	}

	// Records written before MediaRef existed carry only Images[]; project them
	// so every response exposes the same structured shape.
	if len(p.Media) == 0 && len(p.Images) > 0 {
		p.Media = make([]models.MediaRef, 0, len(p.Images))
		for _, img := range p.Images {
			p.Media = append(p.Media, models.MediaRef{URL: img, Kind: "image"})
		}
	}

	// Drop references whose object is not in the bucket.
	//
	// A missing object answers anonymous requests with 403 rather than 404, so
	// emitting the URL would make every consumer -- the Next.js image optimizer
	// most expensively -- fetch and fail on it repeatedly. Withholding the URL
	// lets the storefront render its placeholder immediately.
	//
	// The index fails open, so this is a no-op whenever the bucket cannot be
	// listed.
	p.Images = s.presentOnly(ctx, p.Images)
	p.Media = s.presentMedia(ctx, p.Media)

	if !s.mediaPresent(ctx, p.ImageURL) {
		p.ImageURL = ""
	}
	if p.ImageURL == "" && len(p.Images) > 0 {
		p.ImageURL = p.Images[0]
	}
	p.Thumbnail = s.thumbnailFor(ctx, p.ImageURL)
}

// thumbnailFor returns the small rendition of an image when one has been
// stored, and the image itself otherwise -- which is every product uploaded
// before renditions existed.
func (s *Service) thumbnailFor(ctx context.Context, imageURL string) string {
	if imageURL == "" {
		return ""
	}
	object := mediaindex.ObjectName(imageURL)
	thumb := imageproc.RenditionName(object, fmt.Sprintf("-%dw", imageproc.ThumbWidth), "image/jpeg")
	if thumb == object || !s.mediaPresent(ctx, thumb) {
		return imageURL
	}
	return strings.Replace(imageURL, object, thumb, 1)
}

// mediaPresent reports whether a resolved reference points at an object that
// exists. Empty references are absent by definition.
func (s *Service) mediaPresent(ctx context.Context, ref string) bool {
	if strings.TrimSpace(ref) == "" {
		return false
	}
	return s.media.Has(ctx, mediaindex.ObjectName(ref))
}

// presentOnly filters a reference slice down to objects that exist.
func (s *Service) presentOnly(ctx context.Context, refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if s.mediaPresent(ctx, ref) {
			out = append(out, ref)
		}
	}
	return out
}

// presentMedia filters structured media down to objects that exist.
func (s *Service) presentMedia(ctx context.Context, refs []models.MediaRef) []models.MediaRef {
	out := make([]models.MediaRef, 0, len(refs))
	for _, ref := range refs {
		if s.mediaPresent(ctx, ref.URL) {
			out = append(out, ref)
		}
	}
	return out
}

// regexEscape quotes regex metacharacters so user input is matched literally.
func regexEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
