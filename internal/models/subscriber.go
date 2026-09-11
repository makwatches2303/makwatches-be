package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Subscriber represents a visitor who subscribed to WhatsApp updates/offers
type Subscriber struct {
	ID          primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name        string             `json:"name" bson:"name"`
	Phone       string             `json:"phone" bson:"phone"`
	Source      string             `json:"source" bson:"source"` // "popup", "checkout", "register"
	WelcomeSent bool               `json:"welcomeSent" bson:"welcome_sent"`
	CreatedAt   time.Time          `json:"createdAt" bson:"created_at"`
	UpdatedAt   time.Time          `json:"updatedAt" bson:"updated_at"`
}

// SubscribeRequest is the incoming payload from the website popup
type SubscribeRequest struct {
	Name   string `json:"name"`
	Phone  string `json:"phone" validate:"required"`
	Source string `json:"source"`
}

// SubscribeResponse is returned after subscribing
type SubscribeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Phone   string `json:"phone,omitempty"`
}
