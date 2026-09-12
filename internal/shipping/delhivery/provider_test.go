package delhivery

import (
	"context"
	"strings"
	"testing"

	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// newTestProvider builds the adapter over an unconfigured Delhivery service.
//
// The webhook tests never reach the carrier, so no HTTP stub is needed: the
// authentication decision happens entirely inside ParseWebhook, before any
// state is read.
func newTestProvider(secret string) *Provider {
	return New(services.NewDelhiveryService(services.DelhiveryConfig{
		BaseURL: "https://track.delhivery.example",
	}), Config{
		WebhookSecret:  secret,
		PickupLocation: "Shree Ganesh Watch",
	})
}

var validBody = []byte(`{
  "Waybill": "1491110022334",
  "Status": "Delivered",
  "StatusType": "DL",
  "StatusDateTime": "2024-07-05T15:42:00.000",
  "StatusLocation": "Jetpur",
  "ReferenceNo": "MAK-20240701-001",
  "ExpectedDate": "2024-07-05"
}`)

func TestProviderName(t *testing.T) {
	if got := newTestProvider("s").Name(); got != shipping.ProviderDelhivery {
		t.Fatalf("name = %q", got)
	}
}

// ------------------------------------------------------------------ webhook auth

func TestWebhookAcceptsSharedSecret(t *testing.T) {
	p := newTestProvider("delhivery-shared-secret")

	for _, header := range []string{"x-delhivery-token", "X-Delhivery-Token", "x-api-key", "token"} {
		event, err := p.ParseWebhook(map[string]string{header: "delhivery-shared-secret"}, validBody)
		if err != nil {
			t.Fatalf("header %q was not accepted: %v", header, err)
		}
		if event.Provider != shipping.ProviderDelhivery {
			t.Errorf("provider = %q", event.Provider)
		}
		if event.TrackingNumber != "1491110022334" {
			t.Errorf("waybill = %q", event.TrackingNumber)
		}
		if event.Status != shipping.StatusDelivered {
			t.Errorf("status = %q, want %q", event.Status, shipping.StatusDelivered)
		}
		if event.EventKey == "" {
			t.Error("eventKey is required for duplicate suppression")
		}
	}
}

// TestWebhookRejectsWhenNoSecretConfigured is the fix for the audit finding:
// the endpoint used to accept any anonymous caller, and a forged "delivered"
// callback could move an order to delivered.
func TestWebhookRejectsWhenNoSecretConfigured(t *testing.T) {
	p := newTestProvider("")

	if _, err := p.ParseWebhook(map[string]string{"x-api-key": "anything"}, validBody); err == nil {
		t.Fatal("with no configured secret the callback must be refused, not trusted")
	}
}

func TestWebhookRejectsMissingSecret(t *testing.T) {
	p := newTestProvider("delhivery-shared-secret")

	if _, err := p.ParseWebhook(map[string]string{}, validBody); err == nil {
		t.Fatal("an unauthenticated callback must be rejected")
	}
}

func TestWebhookRejectsWrongSecret(t *testing.T) {
	p := newTestProvider("delhivery-shared-secret")

	if _, err := p.ParseWebhook(map[string]string{"x-api-key": "guessed"}, validBody); err == nil {
		t.Fatal("a wrong secret must be rejected")
	}
}

func TestWebhookRejectsMalformedBody(t *testing.T) {
	p := newTestProvider("s")

	if _, err := p.ParseWebhook(map[string]string{"token": "s"}, []byte("<html>")); err == nil {
		t.Fatal("a malformed body must be rejected")
	}
}

func TestWebhookRejectsEmptyBody(t *testing.T) {
	p := newTestProvider("s")

	if _, err := p.ParseWebhook(map[string]string{"token": "s"}, nil); err == nil {
		t.Fatal("an empty body must be rejected")
	}
}

func TestWebhookRejectsBodyWithoutWaybill(t *testing.T) {
	p := newTestProvider("s")

	body := []byte(`{"Status":"Delivered","StatusType":"DL"}`)
	if _, err := p.ParseWebhook(map[string]string{"token": "s"}, body); err == nil {
		t.Fatal("a callback with no waybill must be rejected")
	}
}

func TestWebhookEventKeyIsStablePerEvent(t *testing.T) {
	p := newTestProvider("s")
	headers := map[string]string{"token": "s"}

	first, err := p.ParseWebhook(headers, validBody)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.ParseWebhook(headers, validBody)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventKey != again.EventKey {
		t.Fatal("a redelivered event must produce the same key")
	}

	other := []byte(strings.Replace(string(validBody), `"StatusType": "DL"`, `"StatusType": "IT"`, 1))
	different, err := p.ParseWebhook(headers, other)
	if err != nil {
		t.Fatal(err)
	}
	if different.EventKey == first.EventKey {
		t.Fatal("a different event must produce a different key")
	}
}

func TestWebhookSetsDeliveredTimestamp(t *testing.T) {
	p := newTestProvider("s")

	event, err := p.ParseWebhook(map[string]string{"token": "s"}, validBody)
	if err != nil {
		t.Fatal(err)
	}
	if event.DeliveredAt == nil {
		t.Fatal("deliveredAt should be set for a delivery event")
	}
	if event.OccurredAt.IsZero() {
		t.Fatal("occurredAt was not parsed from StatusDateTime")
	}
	if event.OccurredAt.Year() != 2024 || event.OccurredAt.Day() != 5 {
		t.Fatalf("occurredAt = %v", event.OccurredAt)
	}
}

// ------------------------------------------------------------------ capability

// TestAssignAWBIsUnsupported records that Delhivery has no separate assignment
// step. The service relies on this classification to skip it rather than treat
// a missing capability as a booking failure.
func TestAssignAWBIsUnsupported(t *testing.T) {
	p := newTestProvider("s")

	_, err := p.AssignAWB(context.Background(), shipping.AssignAWBRequest{ProviderShipmentID: "x"})
	if shipping.CodeOf(err) != shipping.CodeUnsupported {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeUnsupported)
	}
}

func TestPickupLocationsReportsConfiguredLocation(t *testing.T) {
	p := newTestProvider("s")

	locations, err := p.PickupLocations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].Name != "Shree Ganesh Watch" {
		t.Fatalf("locations = %+v", locations)
	}
}

func TestPickupLocationsFailsWhenUnconfigured(t *testing.T) {
	p := New(services.NewDelhiveryService(services.DelhiveryConfig{}), Config{})

	if _, err := p.PickupLocations(context.Background()); err == nil {
		t.Fatal("expected an error with no pickup location configured")
	}
}

func TestRatesRejectsMalformedPincode(t *testing.T) {
	p := newTestProvider("s")

	_, err := p.Rates(context.Background(), shipping.RateRequest{DeliveryPincode: "4001"})
	if shipping.CodeOf(err) != shipping.CodeInvalidPincode {
		t.Fatalf("code = %s, want %s", shipping.CodeOf(err), shipping.CodeInvalidPincode)
	}
}

func TestOperationsRequireAnIdentifier(t *testing.T) {
	p := newTestProvider("s")
	ctx := context.Background()

	if _, err := p.Track(ctx, shipping.ShipmentRef{}); err == nil {
		t.Error("Track must require a waybill")
	}
	if err := p.Cancel(ctx, shipping.ShipmentRef{}); err == nil {
		t.Error("Cancel must require a waybill")
	}
	if _, err := p.Label(ctx, shipping.ShipmentRef{}); err == nil {
		t.Error("Label must require a waybill")
	}
}

func TestCancelledContextStopsCarrierCall(t *testing.T) {
	p := newTestProvider("s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Rates(ctx, shipping.RateRequest{DeliveryPincode: "360370"}); err == nil {
		t.Fatal("a cancelled context must not start a carrier call")
	}
}

func TestTrackingURLIsUnchangedFromLegacyIntegration(t *testing.T) {
	// Historical orders carry links built with this exact form; changing it
	// would break every tracking link already sent to a customer.
	if got := TrackingURL("1491110022334"); got != "https://www.delhivery.com/track/package/1491110022334" {
		t.Fatalf("tracking url = %q", got)
	}
	if got := TrackingURL(""); got != "" {
		t.Fatalf("empty waybill should yield no url, got %q", got)
	}
}
