package handlers

import (
	"testing"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// What checkout records about the delivery the customer chose.
//
// This snapshot is the only place the choice survives: the quote token expires
// in thirty minutes, and by the time an admin reviews the order the carrier's
// rate response is long gone. If a field is missing here, nothing downstream
// can recover it.

func TestRateChoiceRecordsTheCustomersDeliveryChoice(t *testing.T) {
	quote := &shipping.VerifiedQuote{
		Provider:              shipping.ProviderShiprocket,
		CourierID:             "bluedart_air",
		CourierName:           "Blue Dart Air",
		Charge:                149,
		EstimatedDeliveryDays: 2,
		ETD:                   "21 Sep 2026",
		CODAvailable:          true,
	}

	choice := rateChoiceFrom(quote)
	if choice == nil {
		t.Fatal("rateChoiceFrom returned nil for a verified quote")
	}

	// The speed the customer picked, in the words they picked it in.
	if choice.DeliveryTier != models.DeliveryTierExpress {
		t.Errorf("DeliveryTier = %q, want %q", choice.DeliveryTier, models.DeliveryTierExpress)
	}
	// ...and everything the admin and the carrier need afterwards.
	if choice.EstimatedDeliveryDays != 2 {
		t.Errorf("EstimatedDeliveryDays = %d, want 2", choice.EstimatedDeliveryDays)
	}
	if choice.ETD != "21 Sep 2026" {
		t.Errorf("ETD = %q, want the carrier's own date", choice.ETD)
	}
	if choice.Charge != 149 {
		t.Errorf("Charge = %v, want 149", choice.Charge)
	}
	if !choice.CODAvailable {
		t.Error("CODAvailable = false, want it carried through from the quote")
	}
	// The internal record of who was quoted. Kept on the order, never shown to
	// the customer, and never binding on the admin's own carrier choice.
	if choice.Provider != shipping.ProviderShiprocket {
		t.Errorf("Provider = %q, want %q", choice.Provider, shipping.ProviderShiprocket)
	}
	if choice.ProviderCourierID != "bluedart_air" {
		t.Errorf("ProviderCourierID = %q, want the quoted courier id", choice.ProviderCourierID)
	}
	if choice.CourierName != "Blue Dart Air" {
		t.Errorf("CourierName = %q, want the quoted courier name", choice.CourierName)
	}
	if want := shipping.OptionID(shipping.ProviderShiprocket, "bluedart_air"); choice.OptionID != want {
		t.Errorf("OptionID = %q, want %q", choice.OptionID, want)
	}
}

// The tier is classified from the signed quote's own day estimate, so each
// speed the customer can choose lands on the tier they were shown.
func TestRateChoiceTierFollowsTheQuotedEstimate(t *testing.T) {
	cases := map[int]string{
		2:  models.DeliveryTierExpress,
		4:  models.DeliveryTierStandard,
		7:  models.DeliveryTierEconomy,
		10: models.DeliveryTierFlexible,
		// A carrier that quoted no estimate.
		0: models.DeliveryTierStandard,
	}
	for days, want := range cases {
		choice := rateChoiceFrom(&shipping.VerifiedQuote{
			Provider:              shipping.ProviderShiprocket,
			EstimatedDeliveryDays: days,
		})
		if choice.DeliveryTier != want {
			t.Errorf("%d-day quote -> tier %q, want %q", days, choice.DeliveryTier, want)
		}
	}
}

// An order placed with no delivery selection records no choice at all, rather
// than a zero-charge option nobody picked.
func TestRateChoiceIsAbsentWithoutASelection(t *testing.T) {
	if choice := rateChoiceFrom(nil); choice != nil {
		t.Fatalf("rateChoiceFrom(nil) = %+v, want nil", choice)
	}
}

// The delivery speed is the customer's decision; the carrier is the shop's.
// Recording the speed must not pre-commit the order to a provider, or the
// admin's choice at approval time would be constrained by a checkout click.
func TestDeliveryTierDoesNotConstrainTheDispatchProvider(t *testing.T) {
	choice := rateChoiceFrom(&shipping.VerifiedQuote{
		Provider:              shipping.ProviderShiprocket,
		CourierID:             "bluedart_air",
		EstimatedDeliveryDays: 2,
	})
	order := &models.Order{
		ShippingOption: choice,
		Approval: &models.OrderApproval{
			Status:   models.DispatchApproved,
			Provider: shipping.ProviderDelhivery,
		},
	}

	// Express was quoted through Shiprocket, the admin approved Delhivery, and
	// that is what must be booked.
	provider, err := shipping.DispatchProviderFor(order, shipping.ProviderDelhivery)
	if err != nil {
		t.Fatalf("dispatching an express order with the other carrier was refused: %v", err)
	}
	if provider != shipping.ProviderDelhivery {
		t.Fatalf("provider = %q, want %q", provider, shipping.ProviderDelhivery)
	}
}
