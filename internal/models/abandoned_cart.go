package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// AbandonedCart records a customer's shopping bag for recovery notifications
type AbandonedCart struct {
	ID             primitive.ObjectID  `json:"id,omitempty" bson:"_id,omitempty"`
	CartToken      string              `json:"cartToken" bson:"cart_token"`
	UserID         *primitive.ObjectID `json:"userId,omitempty" bson:"user_id,omitempty"`
	Phone          string              `json:"phone" bson:"phone"`
	CustomerName   string              `json:"customerName" bson:"customer_name"`
	Items          []TrackedCartItem   `json:"items" bson:"items"`
	Total          float64             `json:"total" bson:"total"`
	Status         string              `json:"status" bson:"status"` // "active", "reminded", "recovered"
	ReminderSent   bool                `json:"reminderSent" bson:"reminder_sent"`
	ReminderSentAt *time.Time          `json:"reminderSentAt,omitempty" bson:"reminder_sent_at,omitempty"`
	LastActivityAt time.Time           `json:"lastActivityAt" bson:"last_activity_at"`
	CreatedAt      time.Time           `json:"createdAt" bson:"created_at"`
	UpdatedAt      time.Time           `json:"updatedAt" bson:"updated_at"`
}

// TrackedCartItem holds minimal item details for WhatsApp templates
type TrackedCartItem struct {
	ProductID string  `json:"productId" bson:"product_id"`
	Name      string  `json:"name" bson:"name"`
	Price     float64 `json:"price" bson:"price"`
	Quantity  int     `json:"quantity" bson:"quantity"`
	Size      string  `json:"size,omitempty" bson:"size,omitempty"`
}

// TrackCartRequest represents the payload from the client cart store
type TrackCartRequest struct {
	CartToken    string            `json:"cartToken"`
	Phone        string            `json:"phone" validate:"required"`
	CustomerName string            `json:"customerName"`
	Items        []TrackedCartItem `json:"items"`
	Total        float64           `json:"total"`
}
