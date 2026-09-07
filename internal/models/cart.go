package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CartItem represents an item in a user's cart
type CartItem struct {
	ID        primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	UserID    primitive.ObjectID `json:"userId" bson:"user_id"`
	ProductID primitive.ObjectID `json:"productId" bson:"product_id"`
	Product   *Product           `json:"product,omitempty" bson:"product,omitempty"`
	// Size selected by user (e.g., S, M, L). Optional to not break existing carts
	Size      string    `json:"size,omitempty" bson:"size,omitempty"`
	Quantity  int       `json:"quantity" bson:"quantity"`
	CreatedAt time.Time `json:"createdAt" bson:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" bson:"updated_at"`
}

// CartItemRequest represents the data required for adding a product to cart
type CartItemRequest struct {
	ProductID string `json:"productId" validate:"required"`
	Quantity  int    `json:"quantity" validate:"required,min=1"`
	Size      string `json:"size,omitempty"`
}

// CartResponse represents the response for cart operations
type CartResponse struct {
	Items []CartItem `json:"items"`
	Total float64    `json:"total"`
}

// CartLineInput is one line of a whole-cart replacement (PUT /cart).
type CartLineInput struct {
	ProductID string `json:"productId" validate:"required"`
	Size      string `json:"size,omitempty"`
	Quantity  int    `json:"quantity" validate:"required,min=1"`
}

// CartReplaceRequest replaces the caller's entire cart with Items.
//
// This exists because POST /cart *adds* to the quantity already held. That is
// the right behaviour for an "add to bag" button but the wrong one for
// synchronising a client-side cart, where sending the same state twice would
// double it. PUT /cart is idempotent: the cart afterwards is exactly Items,
// whatever it held before. An empty Items list clears the cart.
type CartReplaceRequest struct {
	Items []CartLineInput `json:"items"`
}

// Reasons a submitted cart line could not be stored as sent.
const (
	// CartAdjustUnavailable: the product no longer exists, is not published,
	// or is out of stock. Applied is 0 and the line is dropped.
	CartAdjustUnavailable = "unavailable"
	// CartAdjustStock: fewer units remain than were requested. Applied is the
	// number actually held.
	CartAdjustStock = "stock"
)

// CartAdjustment records a line the server could not honour exactly as sent.
//
// Sync must not fail the whole request because one product sold out while it
// sat in someone's bag -- the rest of the cart is still valid, and the customer
// needs to be told what changed rather than silently losing an item.
type CartAdjustment struct {
	ProductID string `json:"productId"`
	Size      string `json:"size,omitempty"`
	Requested int    `json:"requested"`
	Applied   int    `json:"applied"`
	Reason    string `json:"reason"`
}

// CartSyncResponse is the payload returned by PUT /cart.
//
// Adjustments travel inside the data envelope alongside the cart they describe,
// so a caller cannot read the new cart without also seeing what was changed
// about it.
type CartSyncResponse struct {
	Items       []CartItem       `json:"items"`
	Total       float64          `json:"total"`
	Adjustments []CartAdjustment `json:"adjustments"`
}
