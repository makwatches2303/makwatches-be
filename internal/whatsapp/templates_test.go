package whatsapp

import (
	"encoding/json"
	"testing"
)

const sampleTemplate = `{
  "id": "1", "name": "mak_order", "language": "en", "status": "APPROVED", "category": "UTILITY",
  "components": [
    {"type": "HEADER", "format": "IMAGE"},
    {"type": "BODY", "text": "Hi {{1}}, order {{2}} for {{3}} is confirmed. Thanks {{1}}!"},
    {"type": "FOOTER", "text": "MAK Watches"},
    {"type": "BUTTONS", "buttons": [
      {"type": "URL", "text": "Track", "url": "https://makwatches.in/o/{{1}}"},
      {"type": "QUICK_REPLY", "text": "Stop"}
    ]}
  ]
}`

func parseSample(t *testing.T, raw string) Template {
	t.Helper()
	var meta metaTemplate
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	return fromMeta(meta)
}

func TestFromMetaFindsVariablesOnce(t *testing.T) {
	template := parseSample(t, sampleTemplate)
	keys := []string{}
	for _, v := range template.Variables {
		keys = append(keys, v.Key)
	}
	want := []string{"body.1", "body.2", "body.3", "button.0"}
	if len(keys) != len(want) {
		t.Fatalf("variables = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("variables = %v, want %v", keys, want)
		}
	}
	if template.HeaderFormat != "IMAGE" || len(template.Buttons) != 2 || !template.Approved() {
		t.Fatalf("unexpected template: %+v", template)
	}
}

func TestBuildComponentsMappingAndFallbackOrder(t *testing.T) {
	template := parseSample(t, sampleTemplate)
	values := map[string]string{"customer_name": "Aarav", "order_number": "MAK-1", "amount": "₹4,249"}
	binding := FlowBinding{
		HeaderImageURL: "https://example.com/a.jpg",
		// {{3}} mapped explicitly; {{1}} and {{2}} fall back to source order.
		Variables: map[string]string{"body.3": "static:your watch", "button.0": "order_number"},
	}
	components := buildComponents(FlowOrder, template, binding, values)
	if len(components) != 3 {
		t.Fatalf("components = %d, want header, body, button", len(components))
	}
	body := components[1]["parameters"].([]map[string]interface{})
	got := []string{body[0]["text"].(string), body[1]["text"].(string), body[2]["text"].(string)}
	if got[0] != "Aarav" || got[1] != "MAK-1" || got[2] != "your watch" {
		t.Fatalf("body parameters = %v", got)
	}
	button := components[2]
	if button["index"] != "0" || button["parameters"].([]map[string]interface{})[0]["text"] != "MAK-1" {
		t.Fatalf("button = %v", button)
	}
}

func TestNamedTemplateCarriesParameterNames(t *testing.T) {
	template := parseSample(t, `{"name":"n","language":"en","status":"APPROVED","parameter_format":"NAMED",
	  "components":[{"type":"BODY","text":"Hi {{customer_name}}, {{product_name}} waits."}]}`)
	components := buildComponents(FlowCart, template, FlowBinding{}, map[string]string{"customer_name": "A", "product_name": "P"})
	body := components[0]["parameters"].([]map[string]interface{})
	if body[1]["parameter_name"] != "product_name" || body[1]["text"] != "P" {
		t.Fatalf("body = %v", body)
	}
}
