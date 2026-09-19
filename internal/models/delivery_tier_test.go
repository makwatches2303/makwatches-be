package models

import "testing"

// Delivery-speed classification.
//
// The four tiers are what a customer picks between at checkout and what an
// admin prioritises by, so the boundaries are pinned here rather than left to
// be re-derived from the switch by whoever reads it next.

func TestDeliveryTierForMatchesTheOfferedSpeeds(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		// No estimate is Standard, never Economy: the carrier did not say it
		// would be slow, so neither do we.
		{-3, DeliveryTierStandard},
		{0, DeliveryTierStandard},

		// Express is the existing Air charge tier (ShippingTierConfig, ≤ 2 days).
		{1, DeliveryTierExpress},
		{2, DeliveryTierExpress},

		{3, DeliveryTierStandard},
		{4, DeliveryTierStandard},
		{5, DeliveryTierStandard},

		{6, DeliveryTierEconomy},
		{7, DeliveryTierEconomy},
		{8, DeliveryTierEconomy},

		{9, DeliveryTierFlexible},
		{10, DeliveryTierFlexible},
		{21, DeliveryTierFlexible},
	}
	for _, tc := range cases {
		if got := DeliveryTierFor(tc.days); got != tc.want {
			t.Errorf("DeliveryTierFor(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

// The tiers must be monotonic in speed: a faster estimate can never classify
// as a slower tier. A boundary typo would otherwise put a two-day option in
// the economy queue.
func TestDeliveryTierPriorityNeverDecreasesWithSpeed(t *testing.T) {
	previous := DeliveryTierPriority(DeliveryTierFor(1))
	for days := 2; days <= 30; days++ {
		current := DeliveryTierPriority(DeliveryTierFor(days))
		if current > previous {
			t.Fatalf("day %d ranks %d, higher than day %d's %d: a slower estimate became a faster tier",
				days, current, days-1, previous)
		}
		previous = current
	}
}

func TestDeliveryTierPriorityOrdersExpressFirst(t *testing.T) {
	ordered := []string{
		DeliveryTierExpress,
		DeliveryTierStandard,
		DeliveryTierEconomy,
		DeliveryTierFlexible,
	}
	for i := 1; i < len(ordered); i++ {
		if DeliveryTierPriority(ordered[i-1]) <= DeliveryTierPriority(ordered[i]) {
			t.Errorf("%s does not rank above %s", ordered[i-1], ordered[i])
		}
	}
	// An unknown tier must not outrank a real one.
	if DeliveryTierPriority("overnight-drone") >= DeliveryTierPriority(DeliveryTierFlexible) {
		t.Error("an unrecognised tier outranks a real one")
	}
}

// Orders placed before the tier was stored still carry the day count it was
// computed from, so the admin panel can show the same answer for them.
func TestDeliveryTierOfFallsBackToTheStoredEstimate(t *testing.T) {
	if got := DeliveryTierOf(nil); got != "" {
		t.Errorf("DeliveryTierOf(nil) = %q, want empty", got)
	}

	historical := &RateChoice{EstimatedDeliveryDays: 2}
	if got := DeliveryTierOf(historical); got != DeliveryTierExpress {
		t.Errorf("historical order = %q, want %q", got, DeliveryTierExpress)
	}

	// A stored tier wins over re-derivation, which is the whole point: the
	// order keeps meaning what the customer was shown even if the boundaries
	// are retuned afterwards.
	pinned := &RateChoice{EstimatedDeliveryDays: 7, DeliveryTier: DeliveryTierExpress}
	if got := DeliveryTierOf(pinned); got != DeliveryTierExpress {
		t.Errorf("stored tier = %q, want it to win over re-derivation", got)
	}
}
