package handlers

import (
	"testing"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

func TestCouponCalculateDiscount(t *testing.T) {
	cap500 := 500.0
	past := time.Now().Add(-24 * time.Hour)
	future := time.Now().Add(24 * time.Hour)

	tests := []struct {
		name        string
		coupon      models.Coupon
		subtotal    float64
		expected    float64
		expectError bool
	}{
		{
			name: "Valid percentage discount",
			coupon: models.Coupon{
				Code:          "WELCOME10",
				DiscountType:  models.DiscountTypePercentage,
				DiscountValue: 10,
				IsActive:      true,
			},
			subtotal:    25000,
			expected:    2500,
			expectError: false,
		},
		{
			name: "Percentage discount with cap",
			coupon: models.Coupon{
				Code:              "SAVE10CAP",
				DiscountType:      models.DiscountTypePercentage,
				DiscountValue:     10,
				MaxDiscountAmount: &cap500,
				IsActive:          true,
			},
			subtotal:    25000,
			expected:    500, // Capped at 500
			expectError: false,
		},
		{
			name: "Fixed discount",
			coupon: models.Coupon{
				Code:          "FLAT1000",
				DiscountType:  models.DiscountTypeFixed,
				DiscountValue: 1000,
				IsActive:      true,
			},
			subtotal:    5000,
			expected:    1000,
			expectError: false,
		},
		{
			name: "Fixed discount exceeds subtotal",
			coupon: models.Coupon{
				Code:          "FLAT1000",
				DiscountType:  models.DiscountTypeFixed,
				DiscountValue: 1000,
				IsActive:      true,
			},
			subtotal:    600,
			expected:    600, // Capped at subtotal
			expectError: false,
		},
		{
			name: "Inactive coupon",
			coupon: models.Coupon{
				Code:          "INACTIVE",
				DiscountType:  models.DiscountTypePercentage,
				DiscountValue: 10,
				IsActive:      false,
			},
			subtotal:    1000,
			expectError: true,
		},
		{
			name: "Expired coupon",
			coupon: models.Coupon{
				Code:          "EXPIRED",
				DiscountType:  models.DiscountTypePercentage,
				DiscountValue: 10,
				EndDate:       &past,
				IsActive:      true,
			},
			subtotal:    1000,
			expectError: true,
		},
		{
			name: "Scheduled coupon in future",
			coupon: models.Coupon{
				Code:          "FUTURE",
				DiscountType:  models.DiscountTypePercentage,
				DiscountValue: 10,
				StartDate:     &future,
				IsActive:      true,
			},
			subtotal:    1000,
			expectError: true,
		},
		{
			name: "Subtotal below minimum order amount",
			coupon: models.Coupon{
				Code:           "MIN5000",
				DiscountType:   models.DiscountTypePercentage,
				DiscountValue:  10,
				MinOrderAmount: 5000,
				IsActive:       true,
			},
			subtotal:    4000,
			expectError: true,
		},
		{
			name: "Usage limit reached",
			coupon: models.Coupon{
				Code:            "MAXEDOUT",
				DiscountType:    models.DiscountTypePercentage,
				DiscountValue:   10,
				UsageLimitTotal: 10,
				UsageCount:      10,
				IsActive:        true,
			},
			subtotal:    4000,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discount, err := tt.coupon.CalculateDiscount(tt.subtotal)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got nil, discount=%.2f", discount)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if discount != tt.expected {
				t.Fatalf("expected discount %.2f, got %.2f", tt.expected, discount)
			}
		})
	}
}
