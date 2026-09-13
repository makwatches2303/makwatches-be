package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
)

var nonDigitRegex = regexp.MustCompile(`[^\d+]`)

// Client manages WhatsApp communication via FlowSell Connect
type Client struct {
	APIKey          string
	BaseURL         string
	PhoneNumberID   string
	WelcomeTemplate string
	CartTemplate    string
	WelcomeImageURL string
	httpClient      *http.Client
}

// NewClient creates a new instance of WhatsApp client
func NewClient(cfg *config.Config) *Client {
	baseURL := strings.TrimRight(cfg.FlowSellAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://cmwhd2hdarqpip3krnua6ep3le0qhstm.lambda-url.ap-south-1.on.aws"
	}

	welcomeTemplate := cfg.WhatsAppWelcomeTemplate
	if welcomeTemplate == "" {
		welcomeTemplate = "mak_watches_welcome_modern"
	}

	cartTemplate := cfg.WhatsAppCartTemplate
	if cartTemplate == "" {
		cartTemplate = "mak_watches_cart_reminder"
	}

	phoneID := cfg.WhatsAppPhoneNumberID
	if phoneID == "" {
		phoneID = "1265231066679663"
	}

	return &Client{
		APIKey:          cfg.FlowSellAPIKey,
		BaseURL:         baseURL,
		PhoneNumberID:   phoneID,
		WelcomeTemplate: welcomeTemplate,
		CartTemplate:    cartTemplate,
		WelcomeImageURL: cfg.WhatsAppWelcomeImageURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// UpdatePresets dynamically updates the active template names and media link
func (c *Client) UpdatePresets(welcomeTemplate, cartTemplate, welcomeImageURL string) {
	if welcomeTemplate != "" {
		c.WelcomeTemplate = welcomeTemplate
	}
	if cartTemplate != "" {
		c.CartTemplate = cartTemplate
	}
	if welcomeImageURL != "" {
		c.WelcomeImageURL = welcomeImageURL
	}
}

// NormalizePhoneNumber standardizes phone numbers into E.164 format (+91...)
func NormalizePhoneNumber(raw string) (string, error) {
	cleaned := nonDigitRegex.ReplaceAllString(strings.TrimSpace(raw), "")
	if cleaned == "" {
		return "", errors.New("phone number cannot be empty")
	}

	// If starts with +, strip to inspect digits
	digits := strings.TrimPrefix(cleaned, "+")

	// If 10 digits, assume India (+91)
	if len(digits) == 10 {
		return "+91" + digits, nil
	}

	// If 12 digits starting with 91 (Indian number without +)
	if len(digits) == 12 && strings.HasPrefix(digits, "91") {
		return "+" + digits, nil
	}

	// If starts with 0 and followed by 10 digits
	if len(digits) == 11 && strings.HasPrefix(digits, "0") {
		return "+91" + digits[1:], nil
	}

	// For any other international number with digits >= 7 and <= 15
	if len(digits) >= 7 && len(digits) <= 15 {
		return "+" + digits, nil
	}

	return "", fmt.Errorf("invalid phone number format: %s", raw)
}

// TemplatePayload represents the request body to FlowSell Connect /v1/messages/templates
type TemplatePayload struct {
	Channel         string                 `json:"channel"`
	PhoneNumberID   string                 `json:"phoneNumberId,omitempty"`
	To              string                 `json:"to"`
	Template        string                 `json:"template"`
	Language        string                 `json:"language"`
	HeaderType      string                 `json:"headerType,omitempty"`
	HeaderMediaLink string                 `json:"headerMediaLink,omitempty"`
	Variables       map[string]interface{} `json:"variables"`
}

// SendResponse represents the response from FlowSell Connect
type SendResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

// SendWelcomeTemplate sends the greeting image template to a new subscriber or registered user
func (c *Client) SendWelcomeTemplate(ctx context.Context, toPhone, customerName string) error {
	if c.APIKey == "" {
		log.Printf("[WHATSAPP] Warning: FLOWSELL_API_KEY is not configured; skipping welcome message to %s", toPhone)
		return nil
	}

	normalizedPhone, err := NormalizePhoneNumber(toPhone)
	if err != nil {
		return fmt.Errorf("normalize phone: %w", err)
	}

	name := strings.TrimSpace(customerName)
	if name == "" {
		name = "Valued Customer"
	}

	payload := TemplatePayload{
		Channel:       "whatsapp",
		PhoneNumberID: c.PhoneNumberID,
		To:            normalizedPhone,
		Template:      c.WelcomeTemplate,
		Language:      "en_US",
		Variables: map[string]interface{}{
			"customer_name": name,
		},
	}

	if c.WelcomeImageURL != "" {
		payload.HeaderType = "image"
		payload.HeaderMediaLink = c.WelcomeImageURL
	}

	log.Printf("[WHATSAPP] Dispatching welcome template '%s' to %s (name: %s)", c.WelcomeTemplate, normalizedPhone, name)
	if err := c.postTemplate(ctx, payload); err != nil {
		log.Printf("[WHATSAPP] Template '%s' failed (likely pending Meta review): %v. Attempting interactive card with image, CTA, and opt-out...", c.WelcomeTemplate, err)
		if intErr := c.SendInteractiveWelcome(ctx, normalizedPhone, name); intErr == nil {
			return nil
		} else {
			log.Printf("[WHATSAPP] Interactive card fallback failed: %v. Falling back to direct text...", intErr)
			welcomeText := fmt.Sprintf("Welcome to MAK Watches, %s! ⌚✨\n\nThank you for connecting with us. At MAK Watches, every timepiece reflects precision craftsmanship, sophistication, and timeless luxury.\n\nAs a member of our inner circle, you will enjoy VIP access to new releases, private drops, and exclusive styling updates.\n\nExplore our collection:\nhttps://makwatches.in/", name)
			return c.SendTextMessage(ctx, normalizedPhone, welcomeText)
		}
	}
	return nil
}

// SendAbandonedCartTemplate sends the cart recovery template message
func (c *Client) SendAbandonedCartTemplate(ctx context.Context, toPhone, customerName, productName string) error {
	if c.APIKey == "" {
		log.Printf("[WHATSAPP] Warning: FLOWSELL_API_KEY is not configured; skipping cart reminder to %s", toPhone)
		return nil
	}

	normalizedPhone, err := NormalizePhoneNumber(toPhone)
	if err != nil {
		return fmt.Errorf("normalize phone: %w", err)
	}

	name := strings.TrimSpace(customerName)
	if name == "" {
		name = "Valued Customer"
	}

	product := strings.TrimSpace(productName)
	if product == "" {
		product = "selected timepiece"
	}

	payload := TemplatePayload{
		Channel:       "whatsapp",
		PhoneNumberID: c.PhoneNumberID,
		To:            normalizedPhone,
		Template:      c.CartTemplate,
		Language:      "en_US",
		Variables: map[string]interface{}{
			"customer_name": name,
			"product_name":  product,
		},
	}

	log.Printf("[WHATSAPP] Dispatching abandoned cart template '%s' to %s (product: %s)", c.CartTemplate, normalizedPhone, product)
	if err := c.postTemplate(ctx, payload); err != nil {
		log.Printf("[WHATSAPP] Cart template '%s' failed (likely pending Meta review): %v. Falling back to direct WhatsApp message...", c.CartTemplate, err)
		cartText := fmt.Sprintf("Hi %s, you left something exquisite in your cart! ⌚\n\nYour selected timepiece \"%s\" is currently reserved in your bag. Complete your order now with free insured delivery and authentic brand warranty:\nhttps://makwatches.in/cart", name, product)
		return c.SendTextMessage(ctx, normalizedPhone, cartText)
	}
	return nil
}

// SendInteractiveWelcome sends the rich interactive welcome message with image header and buttons
func (c *Client) SendInteractiveWelcome(ctx context.Context, toPhone, customerName string) error {
	normalizedPhone, err := NormalizePhoneNumber(toPhone)
	if err != nil {
		return fmt.Errorf("normalize phone: %w", err)
	}

	name := strings.TrimSpace(customerName)
	if name == "" {
		name = "Valued Customer"
	}

	imageUrl := c.WelcomeImageURL
	if imageUrl == "" {
		imageUrl = "https://storage.googleapis.com/mak-watches.firebasestorage.app/1789059897255846000-welcome-banner.jpg"
	}

	rawPayload := map[string]interface{}{
		"payload": map[string]interface{}{
			"to":   normalizedPhone,
			"type": "interactive",
			"interactive": map[string]interface{}{
				"type": "button",
				"header": map[string]interface{}{
					"type": "image",
					"image": map[string]interface{}{
						"link": imageUrl,
					},
				},
				"body": map[string]interface{}{
					"text": fmt.Sprintf("Welcome to MAK Watches, %s! ⌚✨\n\nThank you for connecting with us. At MAK Watches, every timepiece reflects precision craftsmanship, sophistication, and timeless luxury.\n\nAs a member of our inner circle, you will enjoy VIP access to new releases, private drops, and exclusive styling updates.", name),
				},
				"footer": map[string]interface{}{
					"text": "MAK Watches • The Precision House",
				},
				"action": map[string]interface{}{
					"buttons": []map[string]interface{}{
						{
							"type": "reply",
							"reply": map[string]interface{}{
								"id":    "explore_collection",
								"title": "Explore Collection ⌚",
							},
						},
						{
							"type": "reply",
							"reply": map[string]interface{}{
								"id":    "stop_promotions",
								"title": "Stop Promotions",
							},
						},
					},
				},
			},
		},
	}

	return c.sendRawMessage(ctx, rawPayload)
}

func (c *Client) sendRawMessage(ctx context.Context, payload map[string]interface{}) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal raw payload: %w", err)
	}

	url := fmt.Sprintf("%s/v1/messages/raw", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FlowSell Connect raw error (status %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[WHATSAPP] Successfully delivered interactive message. Response: %s", string(respBody))
	return nil
}

// TextPayload represents the request body to FlowSell Connect /v1/messages/text
type TextPayload struct {
	To            string `json:"to"`
	Text          string `json:"text"`
	PhoneNumberID string `json:"phoneNumberId,omitempty"`
}

// SendTextMessage sends a direct text message via FlowSell Connect
func (c *Client) SendTextMessage(ctx context.Context, toPhone, message string) error {
	if c.APIKey == "" {
		return nil
	}

	normalizedPhone, err := NormalizePhoneNumber(toPhone)
	if err != nil {
		return fmt.Errorf("normalize phone: %w", err)
	}

	payload := TextPayload{
		To:            normalizedPhone,
		Text:          message,
		PhoneNumberID: c.PhoneNumberID,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal text payload: %w", err)
	}

	url := fmt.Sprintf("%s/v1/messages/text", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FlowSell Connect text error (status %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[WHATSAPP] Successfully delivered direct text message to %s. Response: %s", normalizedPhone, string(respBody))
	return nil
}

func (c *Client) postTemplate(ctx context.Context, payload TemplatePayload) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/v1/messages/templates", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FlowSell Connect error (status %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[WHATSAPP] Successfully delivered template '%s' to %s. Response: %s", payload.Template, payload.To, string(respBody))
	return nil
}
