package scrape

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// extractJSONLD reads schema.org Product data out of <script
// type="application/ld+json"> blocks.
//
// This is the extractor to trust when it fires. It is the same data the site
// hands Google, so it is maintained, structured, and describes the product the
// page is actually about -- unlike CSS selectors, which describe today's
// layout and break on next week's redesign. Flipkart, most Shopify themes and
// nearly every brand store emit it; Amazon does not.
func extractJSONLD(doc *html.Node, base *url.URL) *Draft {
	for _, script := range elementsByTag(doc, "script") {
		if !strings.Contains(strings.ToLower(attr(script, "type")), "ld+json") {
			continue
		}
		var raw strings.Builder
		for child := script.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				raw.WriteString(child.Data)
			}
		}
		for _, candidate := range decodeJSONLD(raw.String()) {
			if draft := productFromJSONLD(candidate, base); draft != nil {
				return draft
			}
		}
	}
	return nil
}

// decodeJSONLD flattens the shapes a block can take -- one object, an array of
// them, or a container with an "@graph" list -- into a flat list of objects to
// inspect.
func decodeJSONLD(raw string) []map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}

	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil
	}

	var out []map[string]any
	var visit func(any, int)
	visit = func(v any, depth int) {
		if depth > 6 {
			return
		}
		switch typed := v.(type) {
		case map[string]any:
			out = append(out, typed)
			if graph, ok := typed["@graph"]; ok {
				visit(graph, depth+1)
			}
			// A Product is sometimes nested under the page's WebPage node as
			// mainEntity or itemOffered.
			for _, key := range []string{"mainEntity", "itemOffered", "item"} {
				if nested, ok := typed[key]; ok {
					visit(nested, depth+1)
				}
			}
		case []any:
			for _, item := range typed {
				visit(item, depth+1)
			}
		}
	}
	visit(value, 0)
	return out
}

// productFromJSONLD converts one schema.org node into a Draft, or nil when the
// node is not a Product.
func productFromJSONLD(node map[string]any, base *url.URL) *Draft {
	if !jsonTypeIs(node["@type"], "Product") {
		return nil
	}

	draft := &Draft{Extractor: "schema.org"}
	draft.Name = jsonString(node["name"])
	draft.Description = cleanHTMLText(jsonString(node["description"]))
	draft.SKU = jsonString(node["sku"])
	draft.ModelNo = firstNonEmpty(jsonString(node["mpn"]), jsonString(node["model"]))
	draft.Category = jsonString(node["category"])

	switch brand := node["brand"].(type) {
	case string:
		draft.Brand = brand
	case map[string]any:
		draft.Brand = jsonString(brand["name"])
	case []any:
		if len(brand) > 0 {
			if first, ok := brand[0].(map[string]any); ok {
				draft.Brand = jsonString(first["name"])
			} else {
				draft.Brand = jsonString(brand[0])
			}
		}
	}

	for _, image := range jsonStrings(node["image"]) {
		draft.Images = append(draft.Images, UpgradeImageURL(absolute(base, image)))
	}

	draft.Price, draft.MRP, draft.Currency = offersFromJSONLD(node["offers"])

	// additionalProperty is schema.org's own key/value bag, and is where a
	// well-marked-up watch page puts case diameter, movement and water
	// resistance.
	for _, prop := range jsonList(node["additionalProperty"]) {
		asMap, ok := prop.(map[string]any)
		if !ok {
			continue
		}
		label := jsonString(asMap["name"])
		value := firstNonEmpty(jsonString(asMap["value"]), jsonString(asMap["unitText"]))
		if label != "" && value != "" {
			draft.Specs = append(draft.Specs, Spec{Label: label, Value: value})
		}
	}

	// A handful of schema fields are watch attributes by another name, and are
	// more useful listed as specs than dropped.
	for label, key := range map[string]string{
		"Colour":   "color",
		"Material": "material",
		"Size":     "size",
		"Width":    "width",
		"Depth":    "depth",
	} {
		if value := jsonString(node[key]); value != "" {
			draft.Specs = append(draft.Specs, Spec{Label: label, Value: value})
		}
	}

	if draft.Empty() {
		return nil
	}
	return draft
}

// offersFromJSONLD pulls price, list price and currency out of an Offer, an
// AggregateOffer, or a list of either.
func offersFromJSONLD(value any) (price, mrp float64, currency string) {
	for _, item := range jsonList(value) {
		offer, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if nested, ok := offer["offers"]; ok {
			// AggregateOffer wrapping individual Offers.
			if p, m, c := offersFromJSONLD(nested); p > 0 {
				return p, m, c
			}
		}
		if price == 0 {
			price = ParsePrice(firstNonEmpty(
				jsonString(offer["price"]),
				jsonString(offer["lowPrice"]),
			))
		}
		if mrp == 0 {
			// Schema has no "MRP": a struck-through list price shows up as
			// highPrice on an aggregate, or as a priceSpecification of type
			// ListPrice.
			mrp = ParsePrice(jsonString(offer["highPrice"]))
			if mrp == 0 {
				for _, spec := range jsonList(offer["priceSpecification"]) {
					specMap, ok := spec.(map[string]any)
					if !ok {
						continue
					}
					if jsonTypeIs(specMap["@type"], "UnitPriceSpecification") ||
						jsonTypeIs(specMap["priceType"], "ListPrice") {
						if candidate := ParsePrice(jsonString(specMap["price"])); candidate > 0 {
							mrp = candidate
							break
						}
					}
				}
			}
		}
		if currency == "" {
			currency = jsonString(offer["priceCurrency"])
		}
	}
	return price, mrp, currency
}

// jsonTypeIs reports whether a schema.org @type field (string or list)
// mentions the wanted type.
func jsonTypeIs(value any, want string) bool {
	for _, t := range jsonStrings(value) {
		// Types arrive both bare ("Product") and as full URLs
		// ("http://schema.org/Product").
		if strings.EqualFold(t, want) || strings.HasSuffix(t, "/"+want) {
			return true
		}
	}
	return false
}

// jsonString coerces a JSON value to a string, because these documents are
// written by hand and a price is as likely to be 4395 as "4395".
func jsonString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%g", typed)
	case bool:
		return ""
	case map[string]any:
		// {"@value": "..."} and {"url": "..."} both appear in the wild.
		return firstNonEmpty(jsonString(typed["@value"]), jsonString(typed["url"]), jsonString(typed["name"]))
	case []any:
		if len(typed) > 0 {
			return jsonString(typed[0])
		}
	}
	return ""
}

// jsonStrings coerces a value that may be a string or a list of them.
func jsonStrings(value any) []string {
	switch typed := value.(type) {
	case []any:
		var out []string
		for _, item := range typed {
			if s := jsonString(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := jsonString(value); s != "" {
			return []string{s}
		}
	}
	return nil
}

// jsonList normalizes a value that may be a single object or a list.
func jsonList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case nil:
		return nil
	default:
		return []any{typed}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// cleanHTMLText turns a description that arrived as markup into plain text.
// Shopify and several marketplaces put full HTML in the description field.
func cleanHTMLText(s string) string {
	if !strings.Contains(s, "<") {
		return collapseSpace(s)
	}
	doc, err := parseHTML([]byte(s))
	if err != nil {
		return collapseSpace(s)
	}
	return textOf(doc)
}
