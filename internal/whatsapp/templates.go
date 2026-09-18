package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Flow names. A flow is a moment MAK sends a WhatsApp message at; which
// template it sends, and what fills that template's variables, is the admin's
// choice and lives in the settings document.
const (
	FlowWelcome  = "welcome"
	FlowOrder    = "order"
	FlowDelivery = "delivery"
	FlowCart     = "cart"
)

// ValueSource is one piece of MAK data a template variable can be filled
// from. The list per flow is fixed by what the trigger actually knows at the
// time it fires -- a welcome message has no order number to offer.
type ValueSource struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Example string `json:"example"`
}

var (
	sourceCustomer = ValueSource{"customer_name", "Customer name", "Aarav Shah"}
	sourceStore    = ValueSource{"store_url", "Store link", "https://makwatches.in/"}
)

// FlowSources lists, in order, what each flow can put into a template. The
// order matters: a positional template with no saved mapping is filled from
// this list front to back, which is what the hard-coded senders used to do.
var FlowSources = map[string][]ValueSource{
	FlowWelcome: {sourceCustomer, sourceStore},
	FlowCart: {
		sourceCustomer,
		{"product_name", "Watch left in the cart", "Noise Pulse 2 Max"},
		{"cart_url", "Link back to the cart", "https://makwatches.in/cart"},
		sourceStore,
	},
	FlowOrder: {
		sourceCustomer,
		{"order_number", "Order number", "MAK-20260918-001"},
		{"amount", "Order total", "₹4,249"},
		{"order_url", "Link to the order", "https://makwatches.in/account/orders"},
		sourceStore,
	},
	FlowDelivery: {
		sourceCustomer,
		{"order_number", "Order number", "MAK-20260918-001"},
		{"status", "Shipment status", "Out for delivery"},
		{"awb", "Tracking number", "1490812345678"},
		{"tracking_url", "Tracking link", "https://www.delhivery.com/track/package/1490812345678"},
		sourceStore,
	},
}

// StaticPrefix marks a mapping whose value is typed text rather than a source.
const StaticPrefix = "static:"

// TemplateVariable is one placeholder of a template. Key is what a mapping is
// saved under: the component it sits in plus its name or number, so a body
// {{1}} and a header {{1}} stay distinct.
type TemplateVariable struct {
	Key       string `json:"key"`
	Component string `json:"component"` // "header", "body" or "button"
	Name      string `json:"name"`      // "1", "customer_name", or the button index
	Label     string `json:"label"`
}

// Template is a WhatsApp template as the panel needs it: enough to choose
// one, preview it, and map its variables.
type Template struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Language     string             `json:"language"`
	Status       string             `json:"status"`
	Category     string             `json:"category"`
	Named        bool               `json:"named"`
	HeaderFormat string             `json:"headerFormat"` // "", TEXT, IMAGE, VIDEO, DOCUMENT
	HeaderText   string             `json:"headerText,omitempty"`
	Body         string             `json:"body"`
	Footer       string             `json:"footer,omitempty"`
	Buttons      []string           `json:"buttons"`
	Variables    []TemplateVariable `json:"variables"`
}

// Approved reports whether Meta will actually deliver this template.
func (t Template) Approved() bool { return strings.EqualFold(t.Status, "APPROVED") }

// ErrNotConfigured means there is no FlowSell key, so nothing can be listed
// or sent. The panel turns it into "contact the developer".
var ErrNotConfigured = errors.New("FlowSell API key is not configured")

var placeholderRegex = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// metaTemplate mirrors the Graph API object FlowSell Connect passes through.
type metaTemplate struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Language        string `json:"language"`
	Status          string `json:"status"`
	Category        string `json:"category"`
	ParameterFormat string `json:"parameter_format"`
	Components      []struct {
		Type    string `json:"type"`
		Format  string `json:"format"`
		Text    string `json:"text"`
		Buttons []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URL  string `json:"url"`
		} `json:"buttons"`
	} `json:"components"`
}

func placeholders(text string) []string {
	seen := map[string]bool{}
	var names []string
	for _, match := range placeholderRegex.FindAllStringSubmatch(text, -1) {
		if !seen[match[1]] {
			seen[match[1]] = true
			names = append(names, match[1])
		}
	}
	return names
}

func fromMeta(m metaTemplate) Template {
	t := Template{
		ID:        m.ID,
		Name:      m.Name,
		Language:  m.Language,
		Status:    strings.ToUpper(m.Status),
		Category:  m.Category,
		Named:     strings.EqualFold(m.ParameterFormat, "NAMED"),
		Buttons:   []string{},
		Variables: []TemplateVariable{},
	}
	for _, component := range m.Components {
		switch strings.ToUpper(component.Type) {
		case "HEADER":
			t.HeaderFormat = strings.ToUpper(component.Format)
			t.HeaderText = component.Text
			for _, name := range placeholders(component.Text) {
				t.Variables = append(t.Variables, TemplateVariable{
					Key: "header." + name, Component: "header", Name: name, Label: "Header {{" + name + "}}",
				})
			}
		case "BODY":
			t.Body = component.Text
			for _, name := range placeholders(component.Text) {
				t.Variables = append(t.Variables, TemplateVariable{
					Key: "body." + name, Component: "body", Name: name, Label: "Message {{" + name + "}}",
				})
			}
		case "FOOTER":
			t.Footer = component.Text
		case "BUTTONS":
			for index, button := range component.Buttons {
				t.Buttons = append(t.Buttons, button.Text)
				// Only a URL button can carry a variable, and only one: the
				// tail of its link.
				if strings.EqualFold(button.Type, "URL") && len(placeholders(button.URL)) > 0 {
					position := strconv.Itoa(index)
					t.Variables = append(t.Variables, TemplateVariable{
						Key: "button." + position, Component: "button", Name: position,
						Label: fmt.Sprintf("Button “%s” link ending", button.Text),
					})
				}
			}
		}
	}
	return t
}

// templateCache holds the last list for a minute. The panel asks on every
// page load and a send asks for every message; Meta's list changes when
// someone submits a template, which is not that often.
type templateCache struct {
	mu        sync.Mutex
	templates []Template
	fetchedAt time.Time
}

const templateCacheTTL = time.Minute

// ListTemplates returns every template on the connected WhatsApp account,
// with Meta's review status. fresh skips the cache, for the panel's refresh
// button after the admin has just created one in FlowSell.
func (c *Client) ListTemplates(ctx context.Context, fresh bool) ([]Template, error) {
	if c.APIKey == "" {
		return nil, ErrNotConfigured
	}

	c.templates.mu.Lock()
	if !fresh && c.templates.templates != nil && time.Since(c.templates.fetchedAt) < templateCacheTTL {
		cached := c.templates.templates
		c.templates.mu.Unlock()
		return cached, nil
	}
	c.templates.mu.Unlock()

	url := fmt.Sprintf("%s/v1/templates?phoneNumberId=%s&limit=100", c.BaseURL, c.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("FlowSell Connect templates error (status %d): %s", resp.StatusCode, truncate(string(body), 300))
	}

	var listing struct {
		Data []metaTemplate `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("decode templates: %w", err)
	}

	templates := make([]Template, 0, len(listing.Data))
	for _, item := range listing.Data {
		templates = append(templates, fromMeta(item))
	}

	c.templates.mu.Lock()
	c.templates.templates = templates
	c.templates.fetchedAt = time.Now()
	c.templates.mu.Unlock()
	return templates, nil
}

// FindTemplate looks a template up by name, preferring the given language
// when the same name exists in several.
func (c *Client) FindTemplate(ctx context.Context, name, language string) (Template, bool) {
	templates, err := c.ListTemplates(ctx, false)
	if err != nil {
		return Template{}, false
	}
	var fallback *Template
	for index := range templates {
		if templates[index].Name != name {
			continue
		}
		if language == "" || templates[index].Language == language {
			return templates[index], true
		}
		if fallback == nil {
			fallback = &templates[index]
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Template{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FlowBinding is the admin's choice for one flow.
type FlowBinding struct {
	Enabled        bool
	Template       string
	Language       string
	HeaderImageURL string
	Variables      map[string]string // TemplateVariable.Key -> source key, or "static:<text>"
}

// BindingLoader reads the current binding for a flow. It is called per send
// rather than cached on the client: the API runs on Lambda, where a value
// set in memory by the settings handler lives in one container and no other.
type BindingLoader func(ctx context.Context, flow string) (FlowBinding, bool)

// SetBindingLoader wires the settings store in.
func (c *Client) SetBindingLoader(loader BindingLoader) { c.bindings = loader }

// resolve picks the value for one template variable: the saved mapping if
// there is one, else a source of the same name, else -- for a numbered
// variable -- the flow's sources in order.
func resolve(flow string, variable TemplateVariable, position int, mapping, values map[string]string) string {
	if mapped, ok := mapping[variable.Key]; ok && mapped != "" {
		if strings.HasPrefix(mapped, StaticPrefix) {
			return strings.TrimPrefix(mapped, StaticPrefix)
		}
		return values[mapped]
	}
	if value, ok := values[variable.Name]; ok {
		return value
	}
	sources := FlowSources[flow]
	if position < len(sources) {
		return values[sources[position].Key]
	}
	return ""
}

// buildComponents turns a template definition and a set of values into the
// explicit `components` array. Explicit because FlowSell's `variables` object
// is positional by key order, and Go marshals a map alphabetically -- the
// order would be an accident of spelling.
func buildComponents(flow string, template Template, binding FlowBinding, values map[string]string) []map[string]interface{} {
	var components []map[string]interface{}

	parameter := func(variable TemplateVariable, text string) map[string]interface{} {
		if text == "" {
			text = "-" // Meta rejects an empty parameter outright
		}
		p := map[string]interface{}{"type": "text", "text": text}
		if template.Named {
			p["parameter_name"] = variable.Name
		}
		return p
	}

	var header, body []map[string]interface{}
	bodyPosition := 0
	for _, variable := range template.Variables {
		switch variable.Component {
		case "header":
			header = append(header, parameter(variable, resolve(flow, variable, 0, binding.Variables, values)))
		case "body":
			body = append(body, parameter(variable, resolve(flow, variable, bodyPosition, binding.Variables, values)))
			bodyPosition++
		}
	}

	switch template.HeaderFormat {
	case "IMAGE", "VIDEO", "DOCUMENT":
		if binding.HeaderImageURL != "" {
			kind := strings.ToLower(template.HeaderFormat)
			header = []map[string]interface{}{{"type": kind, kind: map[string]interface{}{"link": binding.HeaderImageURL}}}
		}
	}
	if len(header) > 0 {
		components = append(components, map[string]interface{}{"type": "header", "parameters": header})
	}
	if len(body) > 0 {
		components = append(components, map[string]interface{}{"type": "body", "parameters": body})
	}
	for _, variable := range template.Variables {
		if variable.Component != "button" {
			continue
		}
		mapped := binding.Variables[variable.Key]
		text := ""
		if strings.HasPrefix(mapped, StaticPrefix) {
			text = strings.TrimPrefix(mapped, StaticPrefix)
		} else if mapped != "" {
			text = values[mapped]
		}
		if text == "" {
			continue
		}
		components = append(components, map[string]interface{}{
			"type": "button", "sub_type": "url", "index": variable.Name,
			"parameters": []map[string]interface{}{{"type": "text", "text": text}},
		})
	}
	return components
}

// ErrFlowDisabled is returned when the admin has switched the flow off.
var ErrFlowDisabled = errors.New("flow is disabled")

// SendFlow sends whichever template the admin bound to a flow, filled from
// values. It returns ErrFlowDisabled when the flow is off, and an error when
// the template is missing or not approved -- the caller decides whether a
// plain-text fallback is appropriate.
func (c *Client) SendFlow(ctx context.Context, flow, toPhone string, values map[string]string) error {
	if c.APIKey == "" {
		return ErrNotConfigured
	}
	if c.bindings == nil {
		return errors.New("flow bindings are not wired")
	}
	binding, ok := c.bindings(ctx, flow)
	if !ok || binding.Template == "" {
		return fmt.Errorf("no template selected for the %s flow", flow)
	}
	if !binding.Enabled {
		return ErrFlowDisabled
	}
	return c.SendBinding(ctx, flow, toPhone, binding, values)
}

// SendBinding sends one specific binding, enabled or not. The test button
// uses it directly so an admin can try a flow before switching it on.
func (c *Client) SendBinding(ctx context.Context, flow, toPhone string, binding FlowBinding, values map[string]string) error {
	normalizedPhone, err := NormalizePhoneNumber(toPhone)
	if err != nil {
		return fmt.Errorf("normalize phone: %w", err)
	}
	template, found := c.FindTemplate(ctx, binding.Template, binding.Language)
	if !found {
		return fmt.Errorf("template %q was not found on the WhatsApp account", binding.Template)
	}
	if !template.Approved() {
		return fmt.Errorf("template %q is %s, not approved by Meta yet", template.Name, strings.ToLower(template.Status))
	}
	if values == nil {
		values = map[string]string{}
	}
	if values["customer_name"] == "" {
		values["customer_name"] = "Valued Customer"
	}
	if values["store_url"] == "" {
		values["store_url"] = "https://makwatches.in/"
	}

	payload := map[string]interface{}{
		"channel":       "whatsapp",
		"phoneNumberId": c.PhoneNumberID,
		"to":            normalizedPhone,
		"template":      template.Name,
		"language":      template.Language,
	}
	if components := buildComponents(flow, template, binding, values); len(components) > 0 {
		payload["components"] = components
	}

	log.Printf("[WHATSAPP] Dispatching %s flow template '%s' (%s) to %s", flow, template.Name, template.Language, normalizedPhone)
	return c.postJSON(ctx, "/v1/messages/templates", payload)
}

func (c *Client) postJSON(ctx context.Context, path string, payload interface{}) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, strings.NewReader(string(bodyBytes)))
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

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FlowSell Connect error (status %d): %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	return nil
}
