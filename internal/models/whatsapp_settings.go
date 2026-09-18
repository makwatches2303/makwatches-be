package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// WhatsAppFlowSettings stores admin-controlled template and trigger presets.
// Persisted as a single document in collection "whatsapp_settings".
type WhatsAppFlowSettings struct {
	ID primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`

	// Welcome Flow (Triggered on new user signup or popup opt-in)
	WelcomeEnabled  bool   `bson:"welcome_enabled" json:"welcomeEnabled"`
	WelcomeTemplate string `bson:"welcome_template" json:"welcomeTemplate"`
	WelcomeImageURL string `bson:"welcome_image_url" json:"welcomeImageUrl"`

	// Order Confirmation Flow (Triggered on successful checkout)
	OrderConfirmationEnabled  bool   `bson:"order_confirmation_enabled" json:"orderConfirmationEnabled"`
	OrderConfirmationTemplate string `bson:"order_confirmation_template" json:"orderConfirmationTemplate"`

	// Delivery Updates Flow (Triggered on Delhivery shipment updates)
	DeliveryUpdatesEnabled  bool   `bson:"delivery_updates_enabled" json:"deliveryUpdatesEnabled"`
	DeliveryUpdatesTemplate string `bson:"delivery_updates_template" json:"deliveryUpdatesTemplate"`

	// Abandoned Cart Recovery Flow (Triggered by background worker)
	AbandonedCartEnabled  bool   `bson:"abandoned_cart_enabled" json:"abandonedCartEnabled"`
	AbandonedCartTemplate string `bson:"abandoned_cart_template" json:"abandonedCartTemplate"`

	// Bindings holds, per flow ("welcome", "order", "delivery", "cart"), the
	// language of the chosen template and what fills each of its variables.
	// The template name itself stays in the per-flow field above, where the
	// senders have always read it.
	Bindings map[string]WhatsAppFlowBinding `bson:"bindings,omitempty" json:"bindings"`

	// FlowSell Account & App Linking
	PhoneNumberID        string    `bson:"phone_number_id" json:"phoneNumberId"`
	FlowSellDashboardURL string    `bson:"flowsell_dashboard_url" json:"flowsellDashboardUrl"`
	UpdatedAt            time.Time `bson:"updated_at" json:"updatedAt"`
}

// WhatsAppFlowBinding maps a template's variables onto MAK data. A key is a
// template variable ("body.1", "header.customer_name", "button.0"); a value is
// a source key ("customer_name") or "static:<text>".
type WhatsAppFlowBinding struct {
	Language  string            `bson:"language" json:"language"`
	Variables map[string]string `bson:"variables" json:"variables"`
}

// FlowSellDashboardURL is where templates and automations are built. The
// connect.* host serves the sending API only and has no template builder.
const FlowSellDashboardURL = "https://dashboard.flowsell.in"

// DefaultWhatsAppFlowSettings returns initial presets if none exist in DB.
func DefaultWhatsAppFlowSettings() WhatsAppFlowSettings {
	return WhatsAppFlowSettings{
		WelcomeEnabled:            true,
		WelcomeTemplate:           "mak_watches_welcome_modern",
		WelcomeImageURL:           "https://storage.googleapis.com/mak-watches.firebasestorage.app/1789059897255846000-welcome-banner.jpg",
		OrderConfirmationEnabled:  false,
		OrderConfirmationTemplate: "mak_order_confirmation",
		DeliveryUpdatesEnabled:    false,
		DeliveryUpdatesTemplate:   "mak_shipment_update",
		AbandonedCartEnabled:      true,
		AbandonedCartTemplate:     "mak_watches_cart_reminder",
		PhoneNumberID:             "1265231066679663",
		FlowSellDashboardURL:      FlowSellDashboardURL,
		UpdatedAt:                 time.Now(),
	}
}

// UpdateWhatsAppFlowSettingsRequest represents incoming payload to update flow settings.
type UpdateWhatsAppFlowSettingsRequest struct {
	WelcomeEnabled            *bool   `json:"welcomeEnabled,omitempty"`
	WelcomeTemplate           *string `json:"welcomeTemplate,omitempty"`
	WelcomeImageURL           *string `json:"welcomeImageUrl,omitempty"`
	OrderConfirmationEnabled  *bool   `json:"orderConfirmationEnabled,omitempty"`
	OrderConfirmationTemplate *string `json:"orderConfirmationTemplate,omitempty"`
	DeliveryUpdatesEnabled    *bool   `json:"deliveryUpdatesEnabled,omitempty"`
	DeliveryUpdatesTemplate   *string `json:"deliveryUpdatesTemplate,omitempty"`
	AbandonedCartEnabled      *bool   `json:"abandonedCartEnabled,omitempty"`
	AbandonedCartTemplate     *string `json:"abandonedCartTemplate,omitempty"`

	Bindings map[string]WhatsAppFlowBinding `json:"bindings,omitempty"`
}

// WhatsAppTestRequest represents request to send a test message.
type WhatsAppTestRequest struct {
	Phone    string `json:"phone" validate:"required"`
	Template string `json:"template"`
	Flow     string `json:"flow"` // "welcome", "cart", "order", "delivery"

	// Language, Variables and HeaderImageURL let the panel test what is on
	// screen before it is saved. Omitted, the saved binding is used.
	Language       string            `json:"language"`
	Variables      map[string]string `json:"variables"`
	HeaderImageURL string            `json:"headerImageUrl"`
}
