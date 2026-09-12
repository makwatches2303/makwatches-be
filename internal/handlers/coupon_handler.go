package handlers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// CouponHandler handles promotional offers and coupon management
type CouponHandler struct {
	DB     *database.DBClient
	Config *config.Config
}

// NewCouponHandler creates a new CouponHandler instance
func NewCouponHandler(db *database.DBClient, cfg *config.Config) *CouponHandler {
	return &CouponHandler{DB: db, Config: cfg}
}

// ListCoupons returns all coupons with optional filtering and analytics stats
func (h *CouponHandler) ListCoupons(c *fiber.Ctx) error {
	ctx := c.Context()
	collection := h.DB.Collections().Coupons

	search := strings.TrimSpace(c.Query("search", ""))
	status := strings.ToLower(strings.TrimSpace(c.Query("status", "all")))

	filter := bson.M{}
	now := time.Now()

	if search != "" {
		escaped := search
		filter["$or"] = []bson.M{
			{"code": bson.M{"$regex": escaped, "$options": "i"}},
			{"description": bson.M{"$regex": escaped, "$options": "i"}},
		}
	}

	switch status {
	case "active":
		filter["is_active"] = true
		filter["$and"] = []bson.M{
			{"$or": []bson.M{{"start_date": nil}, {"start_date": bson.M{"$lte": now}}}},
			{"$or": []bson.M{{"end_date": nil}, {"end_date": bson.M{"$gte": now}}}},
		}
	case "expired":
		filter["end_date"] = bson.M{"$lt": now}
	case "scheduled":
		filter["start_date"] = bson.M{"$gt": now}
	}

	findOptions := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := collection.Find(ctx, filter, findOptions)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to fetch coupons",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	var coupons []models.Coupon
	if err := cursor.All(ctx, &coupons); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode coupons",
			"error":   err.Error(),
		})
	}
	if coupons == nil {
		coupons = []models.Coupon{}
	}

	// Compute overall KPI stats
	stats := h.computeCouponStats(ctx)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    coupons,
		"stats":   stats,
		"total":   len(coupons),
	})
}

// computeCouponStats aggregates high-level analytics for the admin dashboard
func (h *CouponHandler) computeCouponStats(ctx context.Context) models.CouponStats {
	now := time.Now()
	couponsCol := h.DB.Collections().Coupons
	ordersCol := h.DB.Collections().Orders

	totalCount, _ := couponsCol.CountDocuments(ctx, bson.M{})
	activeCount, _ := couponsCol.CountDocuments(ctx, bson.M{
		"is_active": true,
		"$or": []bson.M{
			{"end_date": nil},
			{"end_date": bson.M{"$gte": now}},
		},
	})

	// Sum total redemptions
	var totalRedemptions int
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "totalRedemptions", Value: bson.D{{Key: "$sum", Value: "$usage_count"}}},
		}}},
	}
	cursor, err := couponsCol.Aggregate(ctx, pipeline)
	if err == nil {
		var result []bson.M
		if err := cursor.All(ctx, &result); err == nil && len(result) > 0 {
			if tr, ok := result[0]["totalRedemptions"].(int32); ok {
				totalRedemptions = int(tr)
			} else if tr, ok := result[0]["totalRedemptions"].(int64); ok {
				totalRedemptions = int(tr)
			}
		}
		cursor.Close(ctx)
	}

	// Sum total savings granted from orders
	var totalSavings float64
	orderPipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "discount_amount", Value: bson.D{{Key: "$gt", Value: 0}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "totalSavings", Value: bson.D{{Key: "$sum", Value: "$discount_amount"}}},
		}}},
	}
	orderCursor, err := ordersCol.Aggregate(ctx, orderPipeline)
	if err == nil {
		var orderResult []bson.M
		if err := orderCursor.All(ctx, &orderResult); err == nil && len(orderResult) > 0 {
			if ts, ok := orderResult[0]["totalSavings"].(float64); ok {
				totalSavings = ts
			}
		}
		orderCursor.Close(ctx)
	}

	return models.CouponStats{
		TotalCoupons:        int(totalCount),
		ActiveCoupons:       int(activeCount),
		TotalRedemptions:    totalRedemptions,
		TotalSavingsGranted: math.Round(totalSavings*100) / 100,
	}
}

// CreateCoupon creates a new promotional coupon
func (h *CouponHandler) CreateCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	var coupon models.Coupon
	if err := c.BodyParser(&coupon); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid coupon request body",
			"error":   err.Error(),
		})
	}

	coupon.Code = strings.ToUpper(strings.TrimSpace(coupon.Code))
	if coupon.Code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Coupon code is required",
		})
	}

	if coupon.DiscountType != models.DiscountTypePercentage && coupon.DiscountType != models.DiscountTypeFixed {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Discount type must be 'percentage' or 'fixed'",
		})
	}

	if coupon.DiscountValue <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Discount value must be greater than 0",
		})
	}

	if coupon.DiscountType == models.DiscountTypePercentage && coupon.DiscountValue > 100 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Percentage discount cannot exceed 100%",
		})
	}

	if coupon.PerUserLimit <= 0 {
		coupon.PerUserLimit = 1
	}

	collection := h.DB.Collections().Coupons
	count, err := collection.CountDocuments(ctx, bson.M{"code": coupon.Code})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database query error",
			"error":   err.Error(),
		})
	}
	if count > 0 {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": fmt.Sprintf("A coupon with code '%s' already exists", coupon.Code),
		})
	}

	now := time.Now()
	coupon.ID = primitive.NewObjectID()
	coupon.UsageCount = 0
	coupon.CreatedAt = now
	coupon.UpdatedAt = now

	_, err = collection.InsertOne(ctx, coupon)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to create coupon",
			"error":   err.Error(),
		})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "Coupon created successfully",
		"data":    coupon,
	})
}

// GetCoupon retrieves a single coupon by its ID
func (h *CouponHandler) GetCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	idHex := c.Params("id")
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid coupon ID format",
		})
	}

	var coupon models.Coupon
	err = h.DB.Collections().Coupons.FindOne(ctx, bson.M{"_id": oid}).Decode(&coupon)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Coupon not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve coupon",
			"error":   err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    coupon,
	})
}

// UpdateCoupon updates an existing coupon
func (h *CouponHandler) UpdateCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	idHex := c.Params("id")
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid coupon ID format",
		})
	}

	var updateReq models.Coupon
	if err := c.BodyParser(&updateReq); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid update payload",
			"error":   err.Error(),
		})
	}

	updateReq.Code = strings.ToUpper(strings.TrimSpace(updateReq.Code))
	if updateReq.Code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Coupon code cannot be empty",
		})
	}

	collection := h.DB.Collections().Coupons

	// Check if another coupon has this code
	duplicateCount, err := collection.CountDocuments(ctx, bson.M{
		"_id":  bson.M{"$ne": oid},
		"code": updateReq.Code,
	})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Error checking duplicate coupon code",
		})
	}
	if duplicateCount > 0 {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"message": fmt.Sprintf("Code '%s' is already in use by another coupon", updateReq.Code),
		})
	}

	updateDoc := bson.M{
		"$set": bson.M{
			"code":                updateReq.Code,
			"description":         updateReq.Description,
			"discount_type":       updateReq.DiscountType,
			"discount_value":      updateReq.DiscountValue,
			"max_discount_amount": updateReq.MaxDiscountAmount,
			"min_order_amount":    updateReq.MinOrderAmount,
			"usage_limit_total":   updateReq.UsageLimitTotal,
			"per_user_limit":      updateReq.PerUserLimit,
			"start_date":          updateReq.StartDate,
			"end_date":            updateReq.EndDate,
			"is_active":           updateReq.IsActive,
			"updated_at":          time.Now(),
		},
	}

	res, err := collection.UpdateOne(ctx, bson.M{"_id": oid}, updateDoc)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to update coupon",
			"error":   err.Error(),
		})
	}
	if res.MatchedCount == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Coupon not found",
		})
	}

	var updated models.Coupon
	_ = collection.FindOne(ctx, bson.M{"_id": oid}).Decode(&updated)

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Coupon updated successfully",
		"data":    updated,
	})
}

// DeleteCoupon removes a coupon permanently
func (h *CouponHandler) DeleteCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	idHex := c.Params("id")
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid coupon ID",
		})
	}

	res, err := h.DB.Collections().Coupons.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to delete coupon",
			"error":   err.Error(),
		})
	}
	if res.DeletedCount == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Coupon not found",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Coupon deleted successfully",
	})
}

// ToggleCoupon flips a coupon's active status
func (h *CouponHandler) ToggleCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	idHex := c.Params("id")
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid coupon ID",
		})
	}

	var body struct {
		IsActive *bool `json:"isActive"`
	}
	_ = c.BodyParser(&body)

	var current models.Coupon
	err = h.DB.Collections().Coupons.FindOne(ctx, bson.M{"_id": oid}).Decode(&current)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Coupon not found",
		})
	}

	newStatus := !current.IsActive
	if body.IsActive != nil {
		newStatus = *body.IsActive
	}

	_, err = h.DB.Collections().Coupons.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{
		"$set": bson.M{
			"is_active":  newStatus,
			"updated_at": time.Now(),
		},
	})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to update coupon status",
		})
	}

	current.IsActive = newStatus
	return c.JSON(fiber.Map{
		"success": true,
		"message": fmt.Sprintf("Coupon status set to %t", newStatus),
		"data":    current,
	})
}

// ValidateCoupon handles public / customer validation of a coupon code against a cart subtotal
func (h *CouponHandler) ValidateCoupon(c *fiber.Ctx) error {
	ctx := c.Context()
	var req models.ValidateCouponRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(models.ValidateCouponResponse{
			Valid:   false,
			Message: "Invalid validation request body",
		})
	}

	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(models.ValidateCouponResponse{
			Valid:   false,
			Message: "Please enter a coupon code",
		})
	}

	var coupon models.Coupon
	err := h.DB.Collections().Coupons.FindOne(ctx, bson.M{"code": code}).Decode(&coupon)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(models.ValidateCouponResponse{
				Valid:   false,
				Code:    code,
				Message: fmt.Sprintf("Coupon code '%s' is invalid or not found", code),
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(models.ValidateCouponResponse{
			Valid:   false,
			Code:    code,
			Message: "Failed to validate coupon",
		})
	}

	discountAmount, err := coupon.CalculateDiscount(req.Subtotal)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(models.ValidateCouponResponse{
			Valid:          false,
			Code:           coupon.Code,
			Description:    coupon.Description,
			DiscountType:   coupon.DiscountType,
			DiscountValue:  coupon.DiscountValue,
			DiscountAmount: 0,
			FinalTotal:     req.Subtotal,
			Message:        err.Error(),
		})
	}

	finalTotal := math.Max(0, req.Subtotal-discountAmount)
	finalTotal = math.Round(finalTotal*100) / 100

	message := fmt.Sprintf("₹%.2f discount applied!", discountAmount)
	if coupon.DiscountType == models.DiscountTypePercentage {
		message = fmt.Sprintf("%.0f%% discount applied (-₹%.2f)", coupon.DiscountValue, discountAmount)
	}

	return c.JSON(models.ValidateCouponResponse{
		Valid:          true,
		Code:           coupon.Code,
		Description:    coupon.Description,
		DiscountType:   coupon.DiscountType,
		DiscountValue:  coupon.DiscountValue,
		DiscountAmount: discountAmount,
		FinalTotal:     finalTotal,
		Message:        message,
	})
}
