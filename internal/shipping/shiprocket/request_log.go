package shiprocket

import (
	"encoding/json"
	"log"
	"strings"
)

// redactionMarker stands in for a personal value.
//
// Square brackets rather than angle brackets: encoding/json HTML-escapes the
// characters < and >, so an angle-bracket marker arrives in the log as a
// \u-escaped sequence and is harder to read than the value it replaced.
const redactionMarker = "[redacted]"

// personalFields are redacted before an outgoing request is logged.
//
// A rejected booking is worth logging so the offending field can be found, but
// the payload is almost entirely customer personal data: a name, a home
// address, a phone number and an email. What matters for diagnosing a 422 is
// which keys were sent and whether each held a value -- not the value itself.
var personalFields = map[string]bool{
	"billing_customer_name":  true,
	"billing_last_name":      true,
	"billing_address":        true,
	"billing_address_2":      true,
	"billing_email":          true,
	"billing_phone":          true,
	"shipping_customer_name": true,
	"shipping_last_name":     true,
	"shipping_address":       true,
	"shipping_address_2":     true,
	"shipping_email":         true,
	"shipping_phone":         true,
	"comment":                true,
	"customer_gstin":         true,
}

// logOutgoingRequest logs a redacted copy of a create-order payload.
//
// Credentials cannot appear here by construction: this takes the request
// *body*, and the bearer token lives only in a header that the transport adds
// and this function never sees. The account password is never part of any
// payload but the login body, which is not logged at all.
func logOutgoingRequest(operation string, payload any, status int) {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[SHIPROCKET] %s request could not be serialized for logging: %v", operation, err)
		return
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		log.Printf("[SHIPROCKET] %s request was not an object", operation)
		return
	}

	for key, value := range fields {
		if !personalFields[key] {
			continue
		}
		// Keep the shape of the value -- present and non-empty, or present and
		// empty -- because that distinction is the whole point of the log.
		if s, isString := value.(string); isString && s == "" {
			fields[key] = ""
			continue
		}
		fields[key] = redactionMarker
	}

	// Line items carry a product name and possibly an HSN code; the keys and
	// whether they are populated are what matter.
	if items, isSlice := fields["order_items"].([]any); isSlice {
		for i, entry := range items {
			item, isMap := entry.(map[string]any)
			if !isMap {
				continue
			}
			if name, has := item["name"].(string); has && name != "" {
				item["name"] = redactionMarker
			}
			items[i] = item
		}
	}

	pretty, err := json.Marshal(fields)
	if err != nil {
		return
	}
	log.Printf("[SHIPROCKET] %s rejected with status %d; request sent (personal data redacted): %s",
		operation, status, strings.TrimSpace(string(pretty)))
}
