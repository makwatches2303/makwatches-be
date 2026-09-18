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
}

// Page is a paginated listing result.
type Page struct {
	Items []models.Product `json:"items"`
	Page  int              `json:"page"`
	Limit int              `json:"limit"`
	Total int64            `json:"total"`
	Pages int64            `json:"pages"`
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
		f["$and"] = []bson.M{{
			"$or": []bson.M{
				{"name": rx},
				{"brand": rx},
				{"category": rx},
				{"collection": rx},
				{"short_description": rx},
			},
		}}
	}

	return f
}

// List returns a page of products matching the query.
func (s *Service) List(ctx context.Context, q Query) (*Page, error) {
	q.normalize()

	coll := s.db.Collections().Products
	f := s.filter(q)

	total, err := coll.CountDocuments(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("catalog: count products: %w", err)
	}

	dir := -1
	if q.Order == "asc" {
		dir = 1
	}
	sortField := map[string]string{
		"price":     "price",
		"name":      "name",
		"createdAt": "created_at",
	}[q.SortBy]

	opts := options.Find().
		SetSkip(int64((q.Page - 1) * q.Limit)).
		SetLimit(int64(q.Limit)).
		SetSort(bson.D{{Key: sortField, Value: dir}})

	cursor, err := coll.Find(ctx, f, opts)
	if err != nil {
		return nil, fmt.Errorf("catalog: find products: %w", err)
	}
	defer cursor.Close(ctx)

	items := []models.Product{}
	if err := cursor.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("catalog: decode products: %w", err)
	}

	for i := range items {
		s.resolveMedia(ctx, &items[i])
	}

	pages := int64(0)
	if q.Limit > 0 {
		pages = (total + int64(q.Limit) - 1) / int64(q.Limit)
	}

	return &Page{Items: items, Page: q.Page, Limit: q.Limit, Total: total, Pages: pages}, nil
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
