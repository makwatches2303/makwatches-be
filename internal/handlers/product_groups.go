package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// productGroup is one watch and every colourway of it, for the admin table.
//
// The catalogue stores each colourway as its own product -- that is what
// lets each carry its own price, stock and photographs -- but an admin
// managing the range does not want thirteen rows of "Noise Pulse 2 Max (…)".
// They want the watch once, opened to its colours when needed. The first
// member is the representative row; Variants holds every member, itself
// included, as full products so the table's inline edits can send the whole
// document back (see InlinePriceCell for why a partial update is unsafe).
type productGroup struct {
	models.Product
	GroupKey     string           `json:"groupKey"`
	VariantCount int              `json:"variantCount"`
	Variants     []models.Product `json:"variants"`
	StockTotal   int              `json:"stockTotal"`
	PriceMin     float64          `json:"priceMin"`
	PriceMax     float64          `json:"priceMax"`
}

// getProductGroups answers GET /products?group=variants: the same filter
// and sort as the flat list, collapsed to one row per variant group.
//
// A product with no group is a group of one, keyed by its own id, so the
// page is a straight mix of grouped and standalone watches in the requested
// order. Pagination counts groups, not products, which is the number an
// admin paging through "watches" expects.
func (h *ProductHandler) getProductGroups(c *fiber.Ctx, ctx context.Context, filter bson.M, sortBy string, sortDirection int, page, limit int) error {
	collection := h.DB.Collections().Products

	// Members are ordered within a group by label, so the representative --
	// and the order the colours open in -- is stable between requests rather
	// than whichever document Mongo happened to read first.
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$addFields", Value: bson.M{
			"group_key": bson.M{"$cond": bson.A{
				bson.M{"$gt": bson.A{bson.M{"$strLenCP": bson.M{"$ifNull": bson.A{"$variant_group_id", ""}}}, 0}},
				"$variant_group_id",
				bson.M{"$toString": "$_id"},
			}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: sortBy, Value: sortDirection}, {Key: "variant_label", Value: 1}, {Key: "_id", Value: 1}}}},
		{{Key: "$group", Value: bson.M{
			"_id":        "$group_key",
			"rep":        bson.M{"$first": "$$ROOT"},
			"members":    bson.M{"$push": "$$ROOT"},
			"count":      bson.M{"$sum": 1},
			"stockTotal": bson.M{"$sum": "$stock"},
			"priceMin":   bson.M{"$min": "$price"},
			"priceMax":   bson.M{"$max": "$price"},
		}}},
		// Groups take the sort order of their representative, which is the
		// member that sorts first -- so "newest" shows the watch whose newest
		// colourway is newest, and "price" the cheapest colourway's price.
		{{Key: "$sort", Value: bson.D{{Key: "rep." + sortBy, Value: sortDirection}, {Key: "_id", Value: 1}}}},
		{{Key: "$facet", Value: bson.M{
			"total": bson.A{bson.M{"$count": "n"}},
			"page": bson.A{
				bson.M{"$skip": int64((page - 1) * limit)},
				bson.M{"$limit": int64(limit)},
			},
		}}},
	}

	cursor, err := collection.Aggregate(ctx, pipeline)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false, "message": "Failed to group products", "error": err.Error(),
		})
	}
	defer cursor.Close(ctx)

	var result []struct {
		Total []struct {
			N int64 `bson:"n"`
		} `bson:"total"`
		Page []struct {
			Key        string           `bson:"_id"`
			Rep        models.Product   `bson:"rep"`
			Members    []models.Product `bson:"members"`
			Count      int              `bson:"count"`
			StockTotal int              `bson:"stockTotal"`
			PriceMin   float64          `bson:"priceMin"`
			PriceMax   float64          `bson:"priceMax"`
		} `bson:"page"`
	}
	if err := cursor.All(ctx, &result); err != nil || len(result) == 0 {
		msg := "Failed to decode product groups"
		if err == nil {
			err = mongo.ErrNoDocuments
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false, "message": msg, "error": err.Error(),
		})
	}

	var total int64
	if len(result[0].Total) > 0 {
		total = result[0].Total[0].N
	}

	groups := make([]productGroup, 0, len(result[0].Page))
	for _, row := range result[0].Page {
		h.resolveProductImages(ctx, &row.Rep)
		h.resolveProductListImages(ctx, row.Members)
		groups = append(groups, productGroup{
			Product:      row.Rep,
			GroupKey:     row.Key,
			VariantCount: row.Count,
			Variants:     row.Members,
			StockTotal:   row.StockTotal,
			PriceMin:     row.PriceMin,
			PriceMax:     row.PriceMax,
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Products retrieved successfully",
		"data":    groups,
		"meta": fiber.Map{
			"page":  page,
			"limit": limit,
			"total": total,
			"pages": (total + int64(limit) - 1) / int64(limit),
		},
	})
}
