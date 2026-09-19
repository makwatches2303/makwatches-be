package shipping

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// The dispatch-approval gate. These are the rules that decide whether an order
// may be handed to a carrier at all, so they are tested without a database, a
// network, or a provider that can reach one.

func orderWithApproval(status, provider string) *models.Order {
	o := &models.Order{ID: primitive.NewObjectID()}
	if status != "" {
		o.Approval = &models.OrderApproval{Status: status, Provider: provider}
	}
	return o
}

func TestRequireDispatchApprovalRefusesAnUnreviewedOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order *models.Order
	}{
		{"no approval record at all", orderWithApproval("", "")},
		{"explicitly pending", orderWithApproval(models.DispatchPending, "")},
		{"pending with a carrier already suggested", orderWithApproval(models.DispatchPending, ProviderShiprocket)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireDispatchApproval(tc.order)
			if err == nil {
				t.Fatal("an unapproved order was allowed to dispatch")
			}
			if got := CodeOf(err); got != CodeApprovalRequired {
				t.Fatalf("code = %s, want %s", got, CodeApprovalRequired)
			}
		})
	}
}

func TestRequireDispatchApprovalAllowsAnApprovedOrder(t *testing.T) {
	order := orderWithApproval(models.DispatchApproved, ProviderDelhivery)
	if err := RequireDispatchApproval(order); err != nil {
		t.Fatalf("an approved order was refused: %v", err)
	}
}

// An order booked before this gate existed has no approval record. Refusing it
// would strand every historical order: no retry, no AWB recovery, and the
// admin panel's existing buttons would all start failing.
func TestRequireDispatchApprovalAllowsAnAlreadyBookedOrder(t *testing.T) {
	order := orderWithApproval("", "")
	order.ShippingInfo = &models.ShippingInfo{
		Provider: ProviderDelhivery,
		Waybill:  "1234567890",
	}
	if err := RequireDispatchApproval(order); err != nil {
		t.Fatalf("a historical booked order was refused: %v", err)
	}
}

func TestRequireDispatchApprovalRejectsNil(t *testing.T) {
	if err := RequireDispatchApproval(nil); err == nil {
		t.Fatal("a nil order was allowed to dispatch")
	}
}

func TestDispatchProviderForHonoursTheApprovedCarrier(t *testing.T) {
	order := orderWithApproval(models.DispatchApproved, ProviderShiprocket)

	// No explicit request: the approved carrier is used.
	got, err := DispatchProviderFor(order, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ProviderShiprocket {
		t.Fatalf("provider = %q, want %q", got, ProviderShiprocket)
	}

	// The same carrier, spelled differently: still the approved one.
	got, err = DispatchProviderFor(order, "ShipRocket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ProviderShiprocket {
		t.Fatalf("provider = %q, want %q", got, ProviderShiprocket)
	}
}

// The double-dispatch case the uniqueness index cannot catch: the index is on
// (order_id, provider), so approving Shiprocket and then booking Delhivery
// collides with nothing and produces two parcels for one order.
func TestDispatchProviderForRefusesADifferentCarrierThanApproved(t *testing.T) {
	order := orderWithApproval(models.DispatchApproved, ProviderShiprocket)

	if _, err := DispatchProviderFor(order, ProviderDelhivery); err == nil {
		t.Fatal("booking a carrier other than the approved one was allowed")
	} else if CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("code = %s, want %s", CodeOf(err), CodeInvalidRequest)
	}
}

func TestDispatchProviderForRejectsAnUnknownCarrier(t *testing.T) {
	order := orderWithApproval(models.DispatchApproved, ProviderShiprocket)
	if _, err := DispatchProviderFor(order, "bluedart-direct"); err == nil {
		t.Fatal("an unknown provider name was accepted")
	}
}

// An order with no nominated carrier keeps the old fallback chain: whatever
// the caller asked for, then the customer's selection, then the primary.
func TestDispatchProviderForPassesThroughWhenNoCarrierWasNominated(t *testing.T) {
	order := orderWithApproval(models.DispatchApproved, "")

	got, err := DispatchProviderFor(order, ProviderDelhivery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ProviderDelhivery {
		t.Fatalf("provider = %q, want %q", got, ProviderDelhivery)
	}

	got, err = DispatchProviderFor(order, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("provider = %q, want the empty fallback", got)
	}
}

func TestProviderIsKnown(t *testing.T) {
	for _, name := range []string{ProviderShiprocket, ProviderDelhivery, "  Delhivery  ", "SHIPROCKET"} {
		if !ProviderIsKnown(name) {
			t.Errorf("ProviderIsKnown(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "bluedart", "dtdc", "shiprocket2"} {
		if ProviderIsKnown(name) {
			t.Errorf("ProviderIsKnown(%q) = true, want false", name)
		}
	}
}

// The gate must sit in front of the carrier call, not after it: an unapproved
// order must not reach a provider even once.
func TestCreateApprovedShipmentDoesNotTouchTheCarrierWhenUnapproved(t *testing.T) {
	provider := &countingProvider{name: ProviderShiprocket}
	// A nil Mongo database is deliberate: if the gate ever let this through,
	// the claim would panic or error rather than quietly book something.
	svc := NewService(nil, ServiceConfig{Primary: ProviderShiprocket}, NewQuoter("test-secret"), provider)

	order := orderWithApproval(models.DispatchPending, "")
	_, err := svc.CreateApprovedShipmentForOrder(context.Background(), order, CreateOptions{})
	if err == nil {
		t.Fatal("an unapproved order was booked")
	}
	if CodeOf(err) != CodeApprovalRequired {
		t.Fatalf("code = %s, want %s", CodeOf(err), CodeApprovalRequired)
	}
	if provider.creates != 0 {
		t.Fatalf("the carrier was called %d time(s) for an unapproved order", provider.creates)
	}
}

// countingProvider records whether a carrier call was attempted. Every method
// reports unsupported rather than fabricating a result -- nothing in these
// tests should be reaching it.
type countingProvider struct {
	name    string
	creates int
}

func (p *countingProvider) Name() string { return p.name }

func (p *countingProvider) Rates(context.Context, RateRequest) ([]RateOption, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) CreateShipment(context.Context, CreateShipmentRequest) (*CreateShipmentResult, error) {
	p.creates++
	return &CreateShipmentResult{ProviderShipmentID: "should-not-happen"}, nil
}

func (p *countingProvider) AssignAWB(context.Context, AssignAWBRequest) (*AssignAWBResult, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) Track(context.Context, ShipmentRef) (*Tracking, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) Cancel(context.Context, ShipmentRef) error {
	return Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) Label(context.Context, ShipmentRef) (*Label, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) SchedulePickup(context.Context, ShipmentRef) (*Pickup, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) PickupLocations(context.Context) ([]PickupLocation, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}

func (p *countingProvider) ParseWebhook(map[string]string, []byte) (*WebhookEvent, error) {
	return nil, Errf(CodeUnsupported, "not used in this test")
}
