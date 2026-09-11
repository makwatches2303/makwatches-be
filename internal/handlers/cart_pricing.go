package handlers

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// cartComposition reads a customer's cart and prices it from the catalogue.
//
// One implementation, shared by the three places that need it: the payment
// intent, the checkout shipping options, and the cart fingerprint. They used
// to price independently, which is now a correctness problem rather than a
// tidiness one -- checkout verifies the captured Razorpay amount against its
// own total, so an intent priced by different arithmetic would reject
// legitimate payments.
//
// Returns the lines for fingerprinting and the goods subtotal in rupees.
func cartComposition(ctx context.Context, db *database.DBClient, userID primitive.ObjectID) ([]shipping.CartLine, float64, error) {
	cursor, err := db.Collections().CartItems.Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return nil, 0, errors.New("Could not read your bag.")
	}
	defer cursor.Close(ctx)

	var items []models.CartItem
	if err := cursor.All(ctx, &items); err != nil {
		return nil, 0, errors.New("Could not read your bag.")
	}

	lines := make([]shipping.CartLine, 0, len(items))
	subtotal := 0.0
	products := db.Collections().Products

	for _, item := range items {
		if item.Quantity <= 0 {
			continue
		}
		var product models.Product
		if err := products.FindOne(ctx, bson.M{"_id": item.ProductID}).Decode(&product); err != nil {
			// A cart line whose product is gone cannot be priced, and guessing
			// would misprice the order.
			return nil, 0, fmt.Errorf("An item in your bag is no longer available.")
		}
		// The same price checkout will use, discounts included.
		subtotal += product.GetFinalPrice() * float64(item.Quantity)
		lines = append(lines, shipping.CartLine{
			ProductID: item.ProductID.Hex(),
			Quantity:  item.Quantity,
		})
	}
	return lines, subtotal, nil
}

// cartLinesFromItems builds the fingerprint input from an already-read cart.
//
// Used by checkout, which has the cart in hand and must produce exactly the
// same fingerprint the options endpoint produced, or every quote would be
// rejected as belonging to a different cart.
func cartLinesFromItems(items []models.CartItem) []shipping.CartLine {
	lines := make([]shipping.CartLine, 0, len(items))
	for _, item := range items {
		if item.Quantity <= 0 {
			continue
		}
		lines = append(lines, shipping.CartLine{
			ProductID: item.ProductID.Hex(),
			Quantity:  item.Quantity,
		})
	}
	return lines
}
