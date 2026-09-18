package handlers

import (
	"context"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
)

const WhatsAppSettingsCollection = "whatsapp_settings"

// WhatsAppSettingsHandler handles admin flow presetting and FlowSell linking
type WhatsAppSettingsHandler struct {
	DB       *database.DBClient
	Config   *config.Config
	WhatsApp *whatsapp.Client
}

// NewWhatsAppSettingsHandler creates a new instance of WhatsAppSettingsHandler
func NewWhatsAppSettingsHandler(db *database.DBClient, cfg *config.Config, wa *whatsapp.Client) *WhatsAppSettingsHandler {
	h := &WhatsAppSettingsHandler{
		DB:       db,
		Config:   cfg,
		WhatsApp: wa,
	}
	if wa != nil && db != nil {
		wa.SetBindingLoader(h.loadBinding)
	}
	return h
}

// loadSettings reads the stored document, or the defaults when nothing has
// been saved yet. The second value reports whether a document exists.
func (h *WhatsAppSettingsHandler) loadSettings(ctx context.Context) (models.WhatsAppFlowSettings, bool, error) {
	var settings models.WhatsAppFlowSettings
	err := h.DB.MongoDB.Collection(WhatsAppSettingsCollection).FindOne(ctx, bson.M{}).Decode(&settings)
	if err == mongo.ErrNoDocuments {
		return models.DefaultWhatsAppFlowSettings(), false, nil
	}
	return settings, err == nil, err
}

// flowBinding picks one flow's switch, template and mapping out of settings.
func flowBinding(settings models.WhatsAppFlowSettings, flow string) (whatsapp.FlowBinding, bool) {
	var binding whatsapp.FlowBinding
	switch flow {
	case whatsapp.FlowWelcome:
		binding.Enabled, binding.Template = settings.WelcomeEnabled, settings.WelcomeTemplate
		binding.HeaderImageURL = settings.WelcomeImageURL
	case whatsapp.FlowOrder:
		binding.Enabled, binding.Template = settings.OrderConfirmationEnabled, settings.OrderConfirmationTemplate
	case whatsapp.FlowDelivery:
		binding.Enabled, binding.Template = settings.DeliveryUpdatesEnabled, settings.DeliveryUpdatesTemplate
	case whatsapp.FlowCart:
		binding.Enabled, binding.Template = settings.AbandonedCartEnabled, settings.AbandonedCartTemplate
	default:
		return binding, false
	}
	if saved, ok := settings.Bindings[flow]; ok {
		binding.Language = saved.Language
		binding.Variables = saved.Variables
	}
	return binding, true
}

// loadBinding is the whatsapp.BindingLoader. A flow with no saved document
// reports false, which keeps the senders on their built-in templates until
// an admin has actually chosen something.
func (h *WhatsAppSettingsHandler) loadBinding(ctx context.Context, flow string) (whatsapp.FlowBinding, bool) {
	settings, exists, err := h.loadSettings(ctx)
	if err != nil || !exists {
		return whatsapp.FlowBinding{}, false
	}
	return flowBinding(settings, flow)
}

// GetTemplates lists the account's templates for the panel's pickers.
//
// It never answers with an error status for a FlowSell problem: the panel has
// to render either way, and what it renders when the list is unavailable is a
// "contact the developer" notice, so the reason travels in the body.
func (h *WhatsAppSettingsHandler) GetTemplates(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
	defer cancel()

	templates, err := h.WhatsApp.ListTemplates(ctx, c.Query("refresh") == "1")
	data := fiber.Map{
		"available":    err == nil,
		"templates":    templates,
		"sources":      whatsapp.FlowSources,
		"dashboardUrl": models.FlowSellDashboardURL,
	}
	if err != nil {
		log.Printf("[WHATSAPP_SETTINGS] Template list unavailable: %v", err)
		data["templates"] = []whatsapp.Template{}
		data["reason"] = err.Error()
	}
	return c.JSON(fiber.Map{"success": true, "data": data})
}

// GetSettings fetches the current flow presets
func (h *WhatsAppSettingsHandler) GetSettings(c *fiber.Ctx) error {
	ctx := c.Context()
	collection := h.DB.MongoDB.Collection(WhatsAppSettingsCollection)

	var settings models.WhatsAppFlowSettings
	err := collection.FindOne(ctx, bson.M{}).Decode(&settings)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			defaults := models.DefaultWhatsAppFlowSettings()
			if h.Config.WhatsAppPhoneNumberID != "" {
				defaults.PhoneNumberID = h.Config.WhatsAppPhoneNumberID
			}
			if h.Config.WhatsAppWelcomeTemplate != "" {
				defaults.WelcomeTemplate = h.Config.WhatsAppWelcomeTemplate
			}
			if h.Config.WhatsAppCartTemplate != "" {
				defaults.AbandonedCartTemplate = h.Config.WhatsAppCartTemplate
			}
			if h.Config.WhatsAppWelcomeImageURL != "" {
				defaults.WelcomeImageURL = h.Config.WhatsAppWelcomeImageURL
			}
			defaults.Bindings = map[string]models.WhatsAppFlowBinding{}
			// Stable across requests: the panel resets its form when this changes.
			defaults.UpdatedAt = time.Time{}
			return c.JSON(fiber.Map{
				"success": true,
				"data":    defaults,
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve WhatsApp flow settings",
			"error":   err.Error(),
		})
	}

	// Always sync phone ID and dashboard URL from config if missing in document
	if settings.PhoneNumberID == "" && h.Config.WhatsAppPhoneNumberID != "" {
		settings.PhoneNumberID = h.Config.WhatsAppPhoneNumberID
	}
	// Not a stored preference: older documents carry the connect.* host, which
	// is the sending API and has no template builder.
	settings.FlowSellDashboardURL = models.FlowSellDashboardURL
	if settings.Bindings == nil {
		settings.Bindings = map[string]models.WhatsAppFlowBinding{}
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    settings,
	})
}

// UpdateSettings updates the flow presets in MongoDB
func (h *WhatsAppSettingsHandler) UpdateSettings(c *fiber.Ctx) error {
	var req models.UpdateWhatsAppFlowSettingsRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	if message := h.validateTemplateChoices(c.Context(), req); message != "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": message})
	}

	setDoc := bson.M{
		"updated_at": time.Now(),
	}
	for flow, binding := range req.Bindings {
		if _, known := whatsapp.FlowSources[flow]; !known {
			continue
		}
		if binding.Variables == nil {
			binding.Variables = map[string]string{}
		}
		setDoc["bindings."+flow] = binding
	}

	if req.WelcomeEnabled != nil {
		setDoc["welcome_enabled"] = *req.WelcomeEnabled
	}
	if req.WelcomeTemplate != nil {
		setDoc["welcome_template"] = *req.WelcomeTemplate
	}
	if req.WelcomeImageURL != nil {
		setDoc["welcome_image_url"] = *req.WelcomeImageURL
	}
	if req.OrderConfirmationEnabled != nil {
		setDoc["order_confirmation_enabled"] = *req.OrderConfirmationEnabled
	}
	if req.OrderConfirmationTemplate != nil {
		setDoc["order_confirmation_template"] = *req.OrderConfirmationTemplate
	}
	if req.DeliveryUpdatesEnabled != nil {
		setDoc["delivery_updates_enabled"] = *req.DeliveryUpdatesEnabled
	}
	if req.DeliveryUpdatesTemplate != nil {
		setDoc["delivery_updates_template"] = *req.DeliveryUpdatesTemplate
	}
	if req.AbandonedCartEnabled != nil {
		setDoc["abandoned_cart_enabled"] = *req.AbandonedCartEnabled
	}
	if req.AbandonedCartTemplate != nil {
		setDoc["abandoned_cart_template"] = *req.AbandonedCartTemplate
	}

	collection := h.DB.MongoDB.Collection(WhatsAppSettingsCollection)
	opts := options.Update().SetUpsert(true)
	_, err := collection.UpdateOne(c.Context(), bson.M{}, bson.M{"$set": setDoc}, opts)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to save WhatsApp flow settings",
			"error":   err.Error(),
		})
	}

	// Update in-memory WhatsApp client
	welcomeTpl := ""
	if req.WelcomeTemplate != nil {
		welcomeTpl = *req.WelcomeTemplate
	}
	cartTpl := ""
	if req.AbandonedCartTemplate != nil {
		cartTpl = *req.AbandonedCartTemplate
	}
	welcomeImg := ""
	if req.WelcomeImageURL != nil {
		welcomeImg = *req.WelcomeImageURL
	}
	h.WhatsApp.UpdatePresets(welcomeTpl, cartTpl, welcomeImg)

	log.Printf("[WHATSAPP_SETTINGS] Settings updated successfully by admin")
	return h.GetSettings(c)
}

// validateTemplateChoices refuses a template that is not on the account or
// not approved. The panel only offers approved ones, so this is the guard
// against a stale tab or a hand-written request quietly breaking a live flow.
func (h *WhatsAppSettingsHandler) validateTemplateChoices(parent context.Context, req models.UpdateWhatsAppFlowSettingsRequest) string {
	chosen := map[string]*string{
		whatsapp.FlowWelcome:  req.WelcomeTemplate,
		whatsapp.FlowOrder:    req.OrderConfirmationTemplate,
		whatsapp.FlowDelivery: req.DeliveryUpdatesTemplate,
		whatsapp.FlowCart:     req.AbandonedCartTemplate,
	}
	current, _, _ := h.loadSettings(parent)

	var templates []whatsapp.Template
	var listErr error
	listed := false
	for flow, name := range chosen {
		if name == nil || *name == "" {
			continue
		}
		// Re-saving the template already in place needs no check; that keeps
		// the switches usable while FlowSell is unreachable.
		if existing, _ := flowBinding(current, flow); existing.Template == *name {
			continue
		}
		if !listed {
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			templates, listErr = h.WhatsApp.ListTemplates(ctx, true)
			cancel()
			listed = true
		}
		if listErr != nil {
			return "Templates could not be checked with FlowSell right now. Please contact the developer."
		}
		approved := false
		for _, template := range templates {
			if template.Name == *name && template.Approved() {
				approved = true
				break
			}
		}
		if !approved {
			return "Template \"" + *name + "\" is not an approved template on the WhatsApp account."
		}
	}
	return ""
}

// SendTest sends a test WhatsApp message to the specified number
func (h *WhatsAppSettingsHandler) SendTest(c *fiber.Ctx) error {
	var req models.WhatsAppTestRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	if req.Phone == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Phone number is required",
		})
	}

	normalizedPhone, err := whatsapp.NormalizePhoneNumber(req.Phone)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid phone number: " + err.Error(),
		})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
	defer cancel()

	flow := req.Flow
	if flow == "" {
		flow = "welcome"
	}

	log.Printf("[WHATSAPP_TEST] Sending test message for flow '%s' to %s", flow, normalizedPhone)

	settings, _, loadErr := h.loadSettings(ctx)
	if loadErr != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false, "message": "Failed to read WhatsApp flow settings",
		})
	}
	binding, known := flowBinding(settings, flow)
	if !known {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "Unknown flow: " + flow})
	}
	// What is on the admin's screen beats what is saved, so a template can be
	// tried before it is committed to.
	if req.Template != "" {
		binding.Template, binding.Language, binding.Variables = req.Template, req.Language, req.Variables
	}
	if req.HeaderImageURL != "" {
		binding.HeaderImageURL = req.HeaderImageURL
	}
	if binding.Template == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false, "message": "Select a template for this flow first.",
		})
	}

	// The test fills every source with its example value, so the admin sees
	// exactly where each mapped value lands in the message.
	values := map[string]string{}
	for _, source := range whatsapp.FlowSources[flow] {
		values[source.Key] = source.Example
	}
	err = h.WhatsApp.SendBinding(ctx, flow, normalizedPhone, binding, values)

	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success": false,
			"message": "Failed to send test message: " + err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Test message dispatched successfully to " + normalizedPhone,
	})
}
