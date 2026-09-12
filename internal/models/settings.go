package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ShippingTierConfig stores the admin-controlled delivery surcharge per speed tier.
// Persisted as a single document (upserted) in collection "shipping_tier_config".
//
// Tier logic applied to each option at checkout time:
//   - Air tier   : EstimatedDeliveryDays <= 2  → AirCharge
//   - Express tier: EstimatedDeliveryDays <= 3  → ExpressCharge
//   - Surface tier: EstimatedDeliveryDays >= 4
//                   or no ETA                   → SurfaceCharge (default 0, free)
type ShippingTierConfig struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"   json:"id,omitempty"`
	AirCharge     float64            `bson:"air_charge"      json:"airCharge"`     // ≤2 days
	ExpressCharge float64            `bson:"express_charge"  json:"expressCharge"` // ≤3 days
	SurfaceCharge float64            `bson:"surface_charge"  json:"surfaceCharge"` // 4+ days (usually 0)
	UpdatedAt     time.Time          `bson:"updated_at"      json:"updatedAt"`
}

// DefaultShippingTierConfig returns sensible defaults when no admin config exists.
func DefaultShippingTierConfig() ShippingTierConfig {
	return ShippingTierConfig{
		AirCharge:     149,
		ExpressCharge: 99,
		SurfaceCharge: 0,
	}
}

// ShippingTierConfigCollection is the MongoDB collection name.
const ShippingTierConfigCollection = "shipping_tier_config"

// Settings represents system settings
type Settings struct {
	ID                 primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	StoreName          string             `json:"storeName" bson:"store_name"`
	StoreDescription   string             `json:"storeDescription" bson:"store_description"`
	ContactEmail       string             `json:"contactEmail" bson:"contact_email"`
	ContactPhone       string             `json:"contactPhone" bson:"contact_phone"`
	Address            string             `json:"address" bson:"address"`
	Logo               string             `json:"logo" bson:"logo"`
	Currency           string             `json:"currency" bson:"currency"`
	TaxRate            float64            `json:"taxRate" bson:"tax_rate"`
	ShippingMethods    []ShippingMethod   `json:"shippingMethods" bson:"shipping_methods"`
	PaymentGateways    []PaymentGateway   `json:"paymentGateways" bson:"payment_gateways"`
	SocialMedia        SocialMedia        `json:"socialMedia" bson:"social_media"`
	PrivacyPolicy      string             `json:"privacyPolicy" bson:"privacy_policy"`
	TermsOfService     string             `json:"termsOfService" bson:"terms_of_service"`
	RefundPolicy       string             `json:"refundPolicy" bson:"refund_policy"`
	EnableRegistration bool               `json:"enableRegistration" bson:"enable_registration"`
	MaintenanceMode    bool               `json:"maintenanceMode" bson:"maintenance_mode"`
	CreatedAt          time.Time          `json:"createdAt" bson:"created_at"`
	UpdatedAt          time.Time          `json:"updatedAt" bson:"updated_at"`
}

// ShippingMethod represents a shipping option
type ShippingMethod struct {
	Name        string  `json:"name" bson:"name"`
	Description string  `json:"description" bson:"description"`
	Cost        float64 `json:"cost" bson:"cost"`
	Enabled     bool    `json:"enabled" bson:"enabled"`
}

// PaymentGateway represents a payment method
type PaymentGateway struct {
	Name        string `json:"name" bson:"name"`
	Description string `json:"description" bson:"description"`
	Enabled     bool   `json:"enabled" bson:"enabled"`
}

// SocialMedia represents social media links
type SocialMedia struct {
	Facebook  string `json:"facebook" bson:"facebook"`
	Instagram string `json:"instagram" bson:"instagram"`
	Twitter   string `json:"twitter" bson:"twitter"`
	LinkedIn  string `json:"linkedin" bson:"linkedin"`
	YouTube   string `json:"youtube" bson:"youtube"`
}

// UpdateSettingsRequest represents data for updating settings
type UpdateSettingsRequest struct {
	StoreName          *string          `json:"storeName,omitempty"`
	StoreDescription   *string          `json:"storeDescription,omitempty"`
	ContactEmail       *string          `json:"contactEmail,omitempty"`
	ContactPhone       *string          `json:"contactPhone,omitempty"`
	Address            *string          `json:"address,omitempty"`
	Currency           *string          `json:"currency,omitempty"`
	TaxRate            *float64         `json:"taxRate,omitempty"`
	ShippingMethods    []ShippingMethod `json:"shippingMethods,omitempty"`
	PaymentGateways    []PaymentGateway `json:"paymentGateways,omitempty"`
	SocialMedia        *SocialMedia     `json:"socialMedia,omitempty"`
	PrivacyPolicy      *string          `json:"privacyPolicy,omitempty"`
	TermsOfService     *string          `json:"termsOfService,omitempty"`
	RefundPolicy       *string          `json:"refundPolicy,omitempty"`
	EnableRegistration *bool            `json:"enableRegistration,omitempty"`
	MaintenanceMode    *bool            `json:"maintenanceMode,omitempty"`
}
