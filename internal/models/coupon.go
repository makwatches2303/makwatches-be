package models

import (
	"errors"
	"fmt"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DiscountType defines whether a coupon deducts a percentage or fixed amount
type DiscountType string

const (
	DiscountTypePercentage DiscountType = "percentage"
	DiscountTypeFixed      DiscountType = "fixed"
)

// Coupon represents a promotional offer or discount coupon
type Coupon struct {
	ID                primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	Code              string             `json:"code" bson:"code"`                                             // Unique uppercase code, e.g. MAK10
	Description       string             `json:"description" bson:"description"`                               // e.g. "10% off on all luxury timepieces"
	DiscountType      DiscountType       `json:"discountType" bson:"discount_type"`                           // "percentage" | "fixed"
	DiscountValue     float64            `json:"discountValue" bson:"discount_value"`                         // e.g. 10 (%) or 1000 (₹)
	MaxDiscountAmount *float64           `json:"maxDiscountAmount,omitempty" bson:"max_discount_amount,omitempty"` // Cap for % discounts (e.g. ₹2,000 max)
	MinOrderAmount    float64            `json:"minOrderAmount" bson:"min_order_amount"`                       // Minimum cart spend required (0 = no min)
	UsageLimitTotal   int                `json:"usageLimitTotal" bson:"usage_limit_total"`                     // 0 = unlimited total redemptions
	UsageCount        int                `json:"usageCount" bson:"usage_count"`                               // Number of times redeemed
	PerUserLimit      int                `json:"perUserLimit" bson:"per_user_limit"`                           // Max redemptions per customer (default 1)
	StartDate         *time.Time         `json:"startDate,omitempty" bson:"start_date,omitempty"`             // Optional start date
	EndDate           *time.Time         `json:"endDate,omitempty" bson:"end_date,omitempty"`                 // Optional expiry date
	IsActive          bool               `json:"isActive" bson:"is_active"`                                   // Active toggle
	CreatedAt         time.Time          `json:"createdAt" bson:"created_at"`
	UpdatedAt         time.Time          `json:"updatedAt" bson:"updated_at"`
}

// CalculateDiscount validates the coupon rules against subtotal and computes the discount amount
func (c *Coupon) CalculateDiscount(subtotal float64) (float64, error) {
	if !c.IsActive {
		return 0, errors.New("this coupon is currently inactive")
	}

	now := time.Now()
	if c.StartDate != nil && now.Before(*c.StartDate) {
		return 0, fmt.Errorf("this coupon is not valid until %s", c.StartDate.Format("Jan 02, 2006"))
	}
	if c.EndDate != nil && now.After(*c.EndDate) {
		return 0, errors.New("this coupon has expired")
	}

	if c.UsageLimitTotal > 0 && c.UsageCount >= c.UsageLimitTotal {
		return 0, errors.New("this coupon has reached its maximum redemption limit")
	}

	if c.MinOrderAmount > 0 && subtotal < c.MinOrderAmount {
		return 0, fmt.Errorf("minimum cart order of ₹%.0f required to use this coupon", c.MinOrderAmount)
	}

	var discount float64
	switch c.DiscountType {
	case DiscountTypePercentage:
		if c.DiscountValue <= 0 || c.DiscountValue > 100 {
			return 0, errors.New("invalid percentage discount value")
		}
		discount = (subtotal * c.DiscountValue) / 100.0
		if c.MaxDiscountAmount != nil && *c.MaxDiscountAmount > 0 && discount > *c.MaxDiscountAmount {
			discount = *c.MaxDiscountAmount
		}
	case DiscountTypeFixed:
		if c.DiscountValue <= 0 {
			return 0, errors.New("invalid fixed discount value")
		}
		discount = c.DiscountValue
		if discount > subtotal {
			discount = subtotal
		}
	default:
		return 0, errors.New("unsupported discount type")
	}

	// Round to 2 decimal places
	discount = math.Round(discount*100) / 100
	return discount, nil
}

// ValidateCouponRequest is the payload sent from frontend/checkout to test a coupon
type ValidateCouponRequest struct {
	Code     string  `json:"code"`
	Subtotal float64 `json:"subtotal"`
	UserID   string  `json:"userId,omitempty"`
	Email    string  `json:"email,omitempty"`
}

// ValidateCouponResponse returns whether a coupon is valid and the calculated discount amount
type ValidateCouponResponse struct {
	Valid          bool         `json:"valid"`
	Code           string       `json:"code"`
	Description    string       `json:"description,omitempty"`
	DiscountType   DiscountType `json:"discountType"`
	DiscountValue  float64      `json:"discountValue"`
	DiscountAmount float64      `json:"discountAmount"`
	FinalTotal     float64      `json:"finalTotal"`
	Message        string       `json:"message"`
}

// CouponStats provides high-level summary KPIs for admin
type CouponStats struct {
	TotalCoupons        int     `json:"totalCoupons"`
	ActiveCoupons       int     `json:"activeCoupons"`
	TotalRedemptions    int     `json:"totalRedemptions"`
	TotalSavingsGranted float64 `json:"totalSavingsGranted"`
}
