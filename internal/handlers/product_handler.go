package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/imageurl"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// ProductHandler handles product related requests
type ProductHandler struct {
	DB     *database.DBClient
	Config *config.Config
	// Firebase is the shared Storage provider used by the admin create/update
	// paths. It is lazily initialized, so a deployment without credentials
	// still serves the read-only catalog.
	Firebase *firebase.Provider
	// Media verifies that a resolved image reference exists in the bucket.
	// A nil index reports every object as present.
	Media *mediaindex.Index
}

// NewProductHandler creates a new instance of ProductHandler
func NewProductHandler(db *database.DBClient, cfg *config.Config, fb *firebase.Provider, media *mediaindex.Index) *ProductHandler {
	return &ProductHandler{
		DB:       db,
		Config:   cfg,
		Firebase: fb,
		Media:    media,
	}
}

// resolveProductImages rewrites a product's stored image references to canonical
// Firebase Storage URLs. Legacy records hold absolute "/uploads/..." URLs built
// from the API host at upload time; see the imageurl package.
//
// References whose object is absent from the bucket are dropped rather than
// emitted: Cloud Storage answers anonymous requests for a missing object with
// 403 rather than 404, so serving the URL makes every client retry and fail on
// it. See the mediaindex package. The index fails open, so this is a no-op when
// the bucket cannot be listed.
func (h *ProductHandler) resolveProductImages(ctx context.Context, p *models.Product) {
	bucket := h.Config.FirebaseBucketName
	p.ImageURL = imageurl.Resolve(p.ImageURL, bucket)
	p.Images = imageurl.ResolveAll(p.Images, bucket)

	present := make([]string, 0, len(p.Images))
	for _, img := range p.Images {
		if h.Media.Has(ctx, mediaindex.ObjectName(img)) {
			present = append(present, img)
		}
	}
	p.Images = present

	if p.ImageURL != "" && !h.Media.Has(ctx, mediaindex.ObjectName(p.ImageURL)) {
		p.ImageURL = ""
	}
	if p.ImageURL == "" && len(p.Images) > 0 {
		p.ImageURL = p.Images[0]
	}
}

// presentImages drops references whose object is absent from the bucket.
//
// Shared by the reduced-payload public endpoints, which project into their own
// structs rather than decoding a full models.Product and so cannot use
// resolveProductImages. Fails open with the index.
func (h *ProductHandler) presentImages(ctx context.Context, refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if h.Media.Has(ctx, mediaindex.ObjectName(ref)) {
			out = append(out, ref)
		}
	}
	return out
}

// resolveProductListImages applies resolveProductImages across a slice.
func (h *ProductHandler) resolveProductListImages(ctx context.Context, products []models.Product) {
	for i := range products {
		h.resolveProductImages(ctx, &products[i])
	}
}

// GetProducts returns all products with optional filters
func (h *ProductHandler) GetProducts(c *fiber.Ctx) error {
	ctx := c.Context()

	// Parse query parameters for filtering
	category := c.Query("category")
	mainCategory := c.Query("mainCategory")
	subcategory := c.Query("subcategory")
	minPriceStr := c.Query("minPrice")
	maxPriceStr := c.Query("maxPrice")
	sortBy := c.Query("sortBy", "createdAt") // Default sort by createdAt
	order := c.Query("order", "desc")        // Default order desc
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", "10")

	// Convert string parameters to appropriate types
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 1 {
		limit = 10
	}

	// Build the filter
	filter := bson.M{}

	// Add category filter if provided (support legacy and split main/sub params)
	if category != "" {
		filter["category"] = category
	} else if mainCategory != "" && subcategory != "" {
		filter["category"] = mainCategory + "/" + subcategory
	} else if mainCategory != "" {
		filter["category"] = bson.M{"$regex": fmt.Sprintf("^%s", mainCategory)}
	}

	// Add price range filters if provided
	if minPriceStr != "" {
		minPrice, err := strconv.ParseFloat(minPriceStr, 64)
		if err == nil && minPrice >= 0 {
			filter["price"] = bson.M{"$gte": minPrice}
		}
	}

	if maxPriceStr != "" {
		maxPrice, err := strconv.ParseFloat(maxPriceStr, 64)
		if err == nil && maxPrice > 0 {
			if _, ok := filter["price"]; ok {
				filter["price"].(bson.M)["$lte"] = maxPrice
			} else {
				filter["price"] = bson.M{"$lte": maxPrice}
			}
		}
	}

	// Determine sort direction
	sortDirection := 1 // ascending
	if order == "desc" {
		sortDirection = -1 // descending
	}

	// Configure options for pagination and sorting
	findOptions := options.Find()
	findOptions.SetSkip(int64((page - 1) * limit))
	findOptions.SetLimit(int64(limit))
	findOptions.SetSort(bson.D{{Key: sortBy, Value: sortDirection}})

	// First check if we have this query cached in Redis
	cacheKey := fmt.Sprintf("products:%s:%s:%s:%s:%d:%d",
		category, minPriceStr, maxPriceStr, sortBy, page, limit)

	var products []models.Product
	err = h.DB.CacheGet(ctx, cacheKey, &products)
	if err == nil {
		// Cache hit
		h.resolveProductListImages(ctx, products)
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Products retrieved from cache",
			"data":    products,
			"meta": fiber.Map{
				"page":  page,
				"limit": limit,
			},
		})
	}

	// Cache miss, get from database
	collection := h.DB.Collections().Products

	// Count total matching documents for pagination info
	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to count products",
			"error":   err.Error(),
		})
	}

	// Execute the query
	cursor, err := collection.Find(ctx, filter, findOptions)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve products",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	// Decode the results
	if err := cursor.All(ctx, &products); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode products",
			"error":   err.Error(),
		})
	}

	h.resolveProductListImages(ctx, products)

	// Cache the results for future requests (expire after 10 minutes)
	h.DB.CacheSet(ctx, cacheKey, products, 10*time.Minute)

	// Return the products
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Products retrieved successfully",
		"data":    products,
		"meta": fiber.Map{
			"page":  page,
			"limit": limit,
			"total": total,
			"pages": (total + int64(limit) - 1) / int64(limit), // ceiling division
		},
	})
}

// GetProductByID returns a single product by ID
func (h *ProductHandler) GetProductByID(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get product ID from URL parameter
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Product ID is required",
		})
	}

	// Check if the product is in Redis cache
	cacheKey := fmt.Sprintf("product:%s", id)

	var product models.Product
	err := h.DB.CacheGet(ctx, cacheKey, &product)
	if err == nil {
		// Cache hit
		h.resolveProductImages(ctx, &product)
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Product retrieved from cache",
			"data":    product,
		})
	}

	// Convert string ID to ObjectID
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID format",
			"error":   err.Error(),
		})
	}

	// Find product in database
	collection := h.DB.Collections().Products
	if err := collection.FindOne(ctx, bson.M{"_id": objectID}).Decode(&product); err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Product not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve product",
			"error":   err.Error(),
		})
	}

	h.resolveProductImages(ctx, &product)

	// Cache the product for future requests (expire after 30 minutes)
	h.DB.CacheSet(ctx, cacheKey, product, 30*time.Minute)

	// Return the product
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Product retrieved successfully",
		"data":    product,
	})
}

// GetPublicProducts is a light-weight customer storefront endpoint.
// GET /catalog/products
// Accepts same query params as GetProducts but responds with a reduced field set
// to minimize payload (id, name, price, images, category, stock, brand, mainCategory, subcategory).
func (h *ProductHandler) GetPublicProducts(c *fiber.Ctx) error {
	// Reuse GetProducts logic but then map response data
	// Call the internal logic directly by duplicating minimal parts to avoid double writes.
	ctx := c.Context()

	// Parse subset of filters (reuse existing parsing by calling original handler would cause double writes)
	category := c.Query("category")
	mainCategory := c.Query("mainCategory")
	subcategory := c.Query("subcategory")
	// Dynamic filter params
	brandParam := c.Query("brand") // comma-separated or single
	gender := c.Query("gender")
	dialColor := c.Query("dialColor")
	dialShape := c.Query("dialShape")
	dialType := c.Query("dialType")
	strapColor := c.Query("strapColor")
	strapMaterial := c.Query("strapMaterial")
	style := c.Query("style")
	dialThickness := c.Query("dialThickness")
	minPriceStr := c.Query("minPrice")
	maxPriceStr := c.Query("maxPrice")
	inStockStr := c.Query("inStock")
	sortBy := c.Query("sortBy", "createdAt")
	order := c.Query("order", "desc")
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", "100")

	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(limitStr)
	if limit < 1 || limit > 200 {
		limit = 100
	}

	filter := bson.M{}
	if category != "" {
		filter["category"] = category
	} else if mainCategory != "" && subcategory != "" {
		filter["category"] = mainCategory + "/" + subcategory
	} else if mainCategory != "" {
		filter["category"] = bson.M{"$regex": fmt.Sprintf("^%s", mainCategory)}
	}

	// Apply dynamic attribute filters
	if brandParam != "" {
		// split by comma and trim
		parts := []string{}
		for _, b := range strings.Split(brandParam, ",") {
			if s := strings.TrimSpace(b); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 1 {
			filter["brand"] = parts[0]
		} else if len(parts) > 1 {
			filter["brand"] = bson.M{"$in": parts}
		}
	}
	if gender != "" {
		filter["gender"] = gender
	}
	if dialColor != "" {
		filter["dial_color"] = dialColor
	}
	if dialShape != "" {
		filter["dial_shape"] = dialShape
	}
	if dialType != "" {
		filter["dial_type"] = dialType
	}
	if strapColor != "" {
		filter["strap_color"] = strapColor
	}
	if strapMaterial != "" {
		filter["strap_material"] = strapMaterial
	}
	if style != "" {
		filter["style"] = style
	}
	if dialThickness != "" {
		filter["dial_thickness"] = dialThickness
	}
	if inStockStr != "" {
		if inStockStr == "1" || strings.EqualFold(inStockStr, "true") {
			filter["stock"] = bson.M{"$gt": 0}
		}
	}
	if minPriceStr != "" {
		if v, err := strconv.ParseFloat(minPriceStr, 64); err == nil {
			filter["price"] = bson.M{"$gte": v}
		}
	}
	if maxPriceStr != "" {
		if v, err := strconv.ParseFloat(maxPriceStr, 64); err == nil {
			if m, ok := filter["price"].(bson.M); ok {
				m["$lte"] = v
			} else {
				filter["price"] = bson.M{"$lte": v}
			}
		}
	}

	collection := h.DB.Collections().Products

	// Simple pagination without caching (could add later)
	findOptions := options.Find()
	findOptions.SetSkip(int64((page - 1) * limit))
	findOptions.SetLimit(int64(limit))
	// Determine sort
	sortField := sortBy
	if sortField == "createdAt" || sortField == "price" || sortField == "stock" {
		dir := -1
		if strings.EqualFold(order, "asc") {
			dir = 1
		}
		findOptions.SetSort(bson.D{{Key: sortField, Value: dir}})
	} else {
		findOptions.SetSort(bson.D{{Key: "createdAt", Value: -1}})
	}
	// Projection to reduce payload (but include discount fields)
	findOptions.SetProjection(bson.M{
		"name":         1,
		"price":        1,
		"images":       1,
		"category":     1,
		"stock":        1,
		"brand":        1,
		"mainCategory": 1,
		"subcategory":  1,
		// discount fields
		"discount_percentage": 1,
		"discount_amount":     1,
		"discount_start_date": 1,
		"discount_end_date":   1,
	})

	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to count products", "error": err.Error()})
	}

	cursor, err := collection.Find(ctx, filter, findOptions)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to retrieve products", "error": err.Error()})
	}
	defer cursor.Close(ctx)

	type PublicProduct struct {
		ID           primitive.ObjectID `bson:"_id" json:"id"`
		Name         string             `json:"name"`
		Price        float64            `json:"price"`
		Images       []string           `json:"images"`
		Category     string             `json:"category"`
		Stock        int                `json:"stock"`
		Brand        string             `json:"brand,omitempty"`
		MainCategory string             `json:"mainCategory,omitempty"`
		Subcategory  string             `json:"subcategory,omitempty"`
		// discount fields
		DiscountPercentage *float64   `bson:"discount_percentage,omitempty" json:"discountPercentage,omitempty"`
		DiscountAmount     *float64   `bson:"discount_amount,omitempty" json:"discountAmount,omitempty"`
		DiscountStartDate  *time.Time `bson:"discount_start_date,omitempty" json:"discountStartDate,omitempty"`
		DiscountEndDate    *time.Time `bson:"discount_end_date,omitempty" json:"discountEndDate,omitempty"`
	}

	var items []PublicProduct
	if err := cursor.All(ctx, &items); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to decode products", "error": err.Error()})
	}

	for i := range items {
		items[i].Images = h.presentImages(ctx,
			imageurl.ResolveAll(items[i].Images, h.Config.FirebaseBucketName))
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Products retrieved successfully",
		"data":    items,
		"meta": fiber.Map{
			"page":  page,
			"limit": limit,
			"total": total,
			"pages": (total + int64(limit) - 1) / int64(limit),
		},
	})
}

// GetPublicProductByID returns complete product info for storefront
// GET /catalog/products/:id
func (h *ProductHandler) GetPublicProductByID(c *fiber.Ctx) error {
	ctx := c.Context()
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Product ID is required"})
	}
	objID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Invalid product ID"})
	}
	collection := h.DB.Collections().Products
	var doc struct {
		ID           primitive.ObjectID `bson:"_id" json:"id"`
		Name         string             `bson:"name" json:"name"`
		Brand        string             `bson:"brand,omitempty" json:"brand,omitempty"`
		Description  string             `bson:"description" json:"description"`
		Price        float64            `bson:"price" json:"price"`
		Category     string             `bson:"category" json:"category"`
		MainCategory string             `bson:"main_category,omitempty" json:"mainCategory,omitempty"`
		Subcategory  string             `bson:"subcategory,omitempty" json:"subcategory,omitempty"`
		ImageURL     string             `bson:"image_url,omitempty" json:"imageUrl,omitempty"`
		Images       []string           `bson:"images" json:"images"`
		Stock        int                `bson:"stock" json:"stock"`
		// Optional filterable attributes
		Gender        string `bson:"gender,omitempty" json:"gender,omitempty"`
		DialColor     string `bson:"dial_color,omitempty" json:"dialColor,omitempty"`
		DialShape     string `bson:"dial_shape,omitempty" json:"dialShape,omitempty"`
		DialType      string `bson:"dial_type,omitempty" json:"dialType,omitempty"`
		StrapColor    string `bson:"strap_color,omitempty" json:"strapColor,omitempty"`
		StrapMaterial string `bson:"strap_material,omitempty" json:"strapMaterial,omitempty"`
		Style         string `bson:"style,omitempty" json:"style,omitempty"`
		DialThickness string `bson:"dial_thickness,omitempty" json:"dialThickness,omitempty"`
		// Discount fields
		DiscountPercentage *float64   `bson:"discount_percentage,omitempty" json:"discountPercentage,omitempty"`
		DiscountAmount     *float64   `bson:"discount_amount,omitempty" json:"discountAmount,omitempty"`
		DiscountStartDate  *time.Time `bson:"discount_start_date,omitempty" json:"discountStartDate,omitempty"`
		DiscountEndDate    *time.Time `bson:"discount_end_date,omitempty" json:"discountEndDate,omitempty"`
		// Timestamps
		CreatedAt time.Time `bson:"created_at" json:"createdAt"`
		UpdatedAt time.Time `bson:"updated_at" json:"updatedAt"`
	}
	err = collection.FindOne(c.Context(), bson.M{"_id": objID}, options.FindOne().SetProjection(bson.M{
		"name":                1,
		"brand":               1,
		"description":         1,
		"price":               1,
		"category":            1,
		"main_category":       1,
		"subcategory":         1,
		"image_url":           1,
		"images":              1,
		"stock":               1,
		"gender":              1,
		"dial_color":          1,
		"dial_shape":          1,
		"dial_type":           1,
		"strap_color":         1,
		"strap_material":      1,
		"style":               1,
		"dial_thickness":      1,
		"discount_percentage": 1,
		"discount_amount":     1,
		"discount_start_date": 1,
		"discount_end_date":   1,
		"created_at":          1,
		"updated_at":          1,
	})).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"success": false, "message": "Product not found"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to fetch product", "error": err.Error()})
	}
	doc.ImageURL = imageurl.Resolve(doc.ImageURL, h.Config.FirebaseBucketName)
	doc.Images = h.presentImages(ctx,
		imageurl.ResolveAll(doc.Images, h.Config.FirebaseBucketName))
	if doc.ImageURL != "" && !h.Media.Has(ctx, mediaindex.ObjectName(doc.ImageURL)) {
		doc.ImageURL = ""
	}
	if doc.ImageURL == "" && len(doc.Images) > 0 {
		doc.ImageURL = doc.Images[0]
	}

	return c.JSON(fiber.Map{"success": true, "message": "Product retrieved successfully", "data": doc})
}

// GetCatalogFilters returns dynamic filter options based on current products and optional category scope
// GET /catalog/filters?mainCategory=Men&category=Men&subcategory=Chronograph
func (h *ProductHandler) GetCatalogFilters(c *fiber.Ctx) error {
	ctx := c.Context()

	mainCategory := c.Query("mainCategory")
	category := c.Query("category")
	subcategory := c.Query("subcategory")

	filter := bson.M{}
	if category != "" {
		filter["category"] = category
	} else if mainCategory != "" && subcategory != "" {
		filter["category"] = mainCategory + "/" + subcategory
	} else if mainCategory != "" {
		filter["category"] = bson.M{"$regex": fmt.Sprintf("^%s", mainCategory)}
	}

	// Only project fields needed for filters
	proj := bson.M{
		"brand":          1,
		"gender":         1,
		"dial_color":     1,
		"dial_shape":     1,
		"dial_type":      1,
		"strap_color":    1,
		"strap_material": 1,
		"style":          1,
		"dial_thickness": 1,
		"price":          1,
		"stock":          1,
	}

	coll := h.DB.Collections().Products
	cur, err := coll.Find(ctx, filter, options.Find().SetProjection(proj))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to fetch filters", "error": err.Error()})
	}
	defer cur.Close(ctx)

	type row struct {
		Brand         string  `bson:"brand"`
		Gender        string  `bson:"gender"`
		DialColor     string  `bson:"dial_color"`
		DialShape     string  `bson:"dial_shape"`
		DialType      string  `bson:"dial_type"`
		StrapColor    string  `bson:"strap_color"`
		StrapMaterial string  `bson:"strap_material"`
		Style         string  `bson:"style"`
		DialThickness string  `bson:"dial_thickness"`
		Price         float64 `bson:"price"`
		Stock         int     `bson:"stock"`
	}

	var items []row
	if err := cur.All(ctx, &items); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "message": "Failed to decode filters", "error": err.Error()})
	}

	// Build unique sets
	uniq := func() map[string]struct{} { return map[string]struct{}{} }
	brands := uniq()
	genders := uniq()
	dialColors := uniq()
	dialShapes := uniq()
	dialTypes := uniq()
	strapColors := uniq()
	strapMaterials := uniq()
	styles := uniq()
	dialThicknesses := uniq()

	var minPrice, maxPrice float64
	var havePrice bool
	inStock := false

	for _, it := range items {
		if it.Brand != "" {
			brands[it.Brand] = struct{}{}
		}
		if it.Gender != "" {
			genders[it.Gender] = struct{}{}
		}
		if it.DialColor != "" {
			dialColors[it.DialColor] = struct{}{}
		}
		if it.DialShape != "" {
			dialShapes[it.DialShape] = struct{}{}
		}
		if it.DialType != "" {
			dialTypes[it.DialType] = struct{}{}
		}
		if it.StrapColor != "" {
			strapColors[it.StrapColor] = struct{}{}
		}
		if it.StrapMaterial != "" {
			strapMaterials[it.StrapMaterial] = struct{}{}
		}
		if it.Style != "" {
			styles[it.Style] = struct{}{}
		}
		if it.DialThickness != "" {
			dialThicknesses[it.DialThickness] = struct{}{}
		}
		if !havePrice {
			minPrice, maxPrice, havePrice = it.Price, it.Price, true
		} else {
			if it.Price < minPrice {
				minPrice = it.Price
			}
			if it.Price > maxPrice {
				maxPrice = it.Price
			}
		}
		if it.Stock > 0 {
			inStock = true
		}
	}

	// Convert sets to arrays (sorted optional)
	toList := func(m map[string]struct{}) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Filters retrieved",
		"data": fiber.Map{
			"brands":          toList(brands),
			"genders":         toList(genders),
			"dialColors":      toList(dialColors),
			"dialShapes":      toList(dialShapes),
			"dialTypes":       toList(dialTypes),
			"strapColors":     toList(strapColors),
			"strapMaterials":  toList(strapMaterials),
			"styles":          toList(styles),
			"dialThicknesses": toList(dialThicknesses),
			"minPrice":        minPrice,
			"maxPrice":        maxPrice,
			"hasStock":        inStock,
		},
	})
}
