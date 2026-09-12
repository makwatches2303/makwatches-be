package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
)

// AnalyticsHandler serves aggregate figures for the admin dashboard --
// everything that page used to compute in the browser after fetching every
// order (and probing up to six endpoints just to count users). All of this
// is a handful of indexed counts/aggregations instead.
type AnalyticsHandler struct {
	DB     *database.DBClient
	Config *config.Config
}

func NewAnalyticsHandler(db *database.DBClient, cfg *config.Config) *AnalyticsHandler {
	return &AnalyticsHandler{DB: db, Config: cfg}
}

// orderStatusAgg is the shape of one $group bucket from the status/revenue
// aggregation below.
type orderStatusAgg struct {
	Status  string  `bson:"_id"`
	Count   int64   `bson:"count"`
	Revenue float64 `bson:"revenue"`
}

// orderSummary is the lightweight projection used for both "recent orders"
// and "needs attention" -- just what the dashboard's tables render, not the
// full order document (items, addresses, payment info, shipping info).
type orderSummary struct {
	ID           primitive.ObjectID `json:"id" bson:"_id"`
	OrderNumber  string             `json:"orderNumber" bson:"order_number"`
	CustomerName string             `json:"customerName" bson:"customer_name"`
	Total        float64            `json:"total" bson:"total"`
	Status       string             `json:"status" bson:"status"`
	CreatedAt    time.Time          `json:"createdAt" bson:"created_at"`
}

// cancelledStatuses are excluded from totalRevenue -- a cancelled order was
// never actually earned.
var cancelledStatuses = map[string]bool{"cancelled": true}

// attentionStatuses are the order states the dashboard's "needs attention"
// widget surfaces: placed but not yet moving.
var attentionStatuses = []string{"pending", "processing"}

// GetDashboardSummary returns the admin dashboard's stat tiles, order
// status breakdown, most recent orders, and orders needing attention, in one
// call. GET /admin/analytics/summary
func (h *AnalyticsHandler) GetDashboardSummary(c *fiber.Ctx) error {
	ctx := c.Context()

	orderCollection := h.DB.Collections().Orders
	userCollection := h.DB.Collections().Users
	productCollection := h.DB.Collections().Products

	// One grouped pass over orders for both the per-status counts and
	// per-status revenue -- cheaper than decoding every order document just
	// to sum a field and tally a status in application code.
	cursor, err := orderCollection.Aggregate(ctx, bson.A{
		bson.M{"$group": bson.M{
			"_id":     "$status",
			"count":   bson.M{"$sum": 1},
			"revenue": bson.M{"$sum": "$total"},
		}},
	})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to aggregate orders",
			"error":   err.Error(),
		})
	}
	var statusAggs []orderStatusAgg
	if err := cursor.All(ctx, &statusAggs); err != nil {
		cursor.Close(ctx)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode order aggregation",
			"error":   err.Error(),
		})
	}
	cursor.Close(ctx)

	ordersByStatus := map[string]int64{}
	var totalOrders int64
	var totalRevenue float64
	for _, agg := range statusAggs {
		ordersByStatus[agg.Status] = agg.Count
		totalOrders += agg.Count
		if !cancelledStatuses[agg.Status] {
			totalRevenue += agg.Revenue
		}
	}

	recentOrders, err := findOrderSummaries(ctx, orderCollection, bson.M{}, "created_at", -1, 5)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch recent orders",
			"error":   err.Error(),
		})
	}

	needsAttention, err := findOrderSummaries(
		ctx, orderCollection,
		bson.M{"status": bson.M{"$in": attentionStatuses}},
		"created_at", 1, 10,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch orders needing attention",
			"error":   err.Error(),
		})
	}

	totalUsers, err := userCollection.CountDocuments(ctx, bson.M{})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to count users",
			"error":   err.Error(),
		})
	}

	totalProducts, err := productCollection.CountDocuments(ctx, bson.M{})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to count products",
			"error":   err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"totalUsers":     totalUsers,
			"totalOrders":    totalOrders,
			"totalRevenue":   totalRevenue,
			"totalProducts":  totalProducts,
			"ordersByStatus": ordersByStatus,
			"recentOrders":   recentOrders,
			"needsAttention": needsAttention,
		},
	})
}

// findOrderSummaries runs one filtered, sorted, limited, projected query --
// used for both "recent orders" (no filter) and "needs attention"
// (status $in [pending, processing]) so neither table needs a full order
// document, just what it renders.
func findOrderSummaries(
	ctx context.Context,
	collection *mongo.Collection,
	filter bson.M,
	sortField string,
	sortDir int,
	limit int64,
) ([]orderSummary, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: sortField, Value: sortDir}}).
		SetLimit(limit).
		SetProjection(bson.M{
			"order_number":  1,
			"customer_name": 1,
			"total":         1,
			"status":        1,
			"created_at":    1,
		})
	cursor, err := collection.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	summaries := []orderSummary{}
	if err := cursor.All(ctx, &summaries); err != nil {
		return nil, err
	}
	return summaries, nil
}
