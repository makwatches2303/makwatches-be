package shipping

import (
	"context"
	"testing"
	"time"
)

// unconfiguredProvider is a carrier with no credentials -- the state that
// makes Serviceability consider standing in placeholder rates.
type unconfiguredProvider struct {
	*fakeProvider
}

func (unconfiguredProvider) Configured() bool { return false }

func devFallbackTestService(allowFallback bool) *Service {
	p := unconfiguredProvider{newFakeProvider(ProviderShiprocket)}
	return NewService(nil, ServiceConfig{
		Primary:               ProviderShiprocket,
		PickupPincode:         "360370",
		Package:               PackageSpec{WeightGrams: 500},
		AllowDevFallbackRates: allowFallback,
	}, NewQuoter("test-secret"), p)
}

// TestDevFallbackRatesAreNotOfferedInProduction is the regression guard for a
// real order that could be placed but never shipped.
//
// With no Shiprocket credentials in production, Serviceability was quoting
// invented options -- "Shiprocket - Blue Dart Air", a price, a two-day ETA,
// one of them flagged Recommended -- for a carrier that had never been asked.
// Checkout took the order happily; booking it then failed with
// SHIPMENT_CREATION_FAILED and nothing on the order said why. An unconfigured
// carrier must simply not be offered.
func TestDevFallbackRatesAreNotOfferedInProduction(t *testing.T) {
	s := devFallbackTestService(false)

	result, err := s.Serviceability(context.Background(), "", RateRequest{
		DeliveryPincode: "390010",
		Package:         PackageSpec{WeightGrams: 500},
	}, QuoteBinding{Pincode: "390010", WeightGrams: 500})
	if err != nil {
		t.Fatalf("Serviceability: %v", err)
	}

	if len(result.Options) != 0 {
		t.Fatalf("got %d option(s) from an unconfigured carrier, want none: %+v",
			len(result.Options), result.Options)
	}
	// Nothing quoted the lane, so it must not read as deliverable either --
	// that flag is what the storefront gates the pay button on.
	if result.Prepaid || result.COD {
		t.Errorf("prepaid=%v cod=%v, want both false when nothing quoted",
			result.Prepaid, result.COD)
	}
}

// TestDevFallbackRatesStillWorkOutsideProduction keeps the local/staging
// convenience the flag exists for: without it, a developer with no carrier
// account cannot reach the payment step at all.
func TestDevFallbackRatesStillWorkOutsideProduction(t *testing.T) {
	s := devFallbackTestService(true)
	s.now = func() time.Time { return time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC) }

	result, err := s.Serviceability(context.Background(), "", RateRequest{
		DeliveryPincode: "390010",
		Package:         PackageSpec{WeightGrams: 500},
	}, QuoteBinding{Pincode: "390010", WeightGrams: 500})
	if err != nil {
		t.Fatalf("Serviceability: %v", err)
	}

	if len(result.Options) == 0 {
		t.Fatal("expected placeholder options when the dev fallback is allowed")
	}
	for _, opt := range result.Options {
		if opt.Provider != ProviderShiprocket {
			t.Errorf("option provider = %q, want %q", opt.Provider, ProviderShiprocket)
		}
		if opt.Quote == "" {
			t.Error("placeholder option carries no signed quote")
		}
	}
}
