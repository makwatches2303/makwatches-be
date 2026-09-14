package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Subscriber represents a visitor who subscribed to WhatsApp updates/offers
type Subscriber struct {
	ID   primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name string             `json:"name" bson:"name"`
	// Phone is set by the WhatsApp popup. Empty on an email-only subscriber.
	Phone string `json:"phone,omitempty" bson:"phone,omitempty"`
	// Email is set by the newsletter block. Empty on a WhatsApp-only
	// subscriber. A record carries whichever channel the visitor gave us; the
	// two capture paths upsert on different keys and never overwrite each
	// other, so subscribing by one does not clear the other.
	Email       string    `json:"email,omitempty" bson:"email,omitempty"`
	Source      string    `json:"source" bson:"source"` // "popup", "checkout", "register", "newsletter"
	WelcomeSent bool      `json:"welcomeSent" bson:"welcome_sent"`
	CreatedAt   time.Time `json:"createdAt" bson:"created_at"`
	UpdatedAt   time.Time `json:"updatedAt" bson:"updated_at"`
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

// SubscribeEmailRequest is the incoming payload from the newsletter block.
//
// Separate from SubscribeRequest because the two capture different things and
// have different consequences: a phone subscription triggers a WhatsApp welcome
// template, and an email one must not. Sharing a struct would make it far too
// easy for an email-only signup to fall into the messaging path.
type SubscribeEmailRequest struct {
	Name   string `json:"name"`
	Email  string `json:"email" validate:"required"`
	Source string `json:"source"`
}
