package shipping

import "testing"

// TestStatusRankGatesAddressEditing pins the ordering the delivery-address
// endpoint relies on to decide whether a parcel has physically moved.
//
// The rule it encodes: up to pending_pickup the booking can still be
// withdrawn and re-made against a corrected address; from picked_up onward
// the label on the box is the truth and only the carrier can redirect it.
// Getting this boundary wrong in either direction is a real-world failure --
// too strict and nobody can fix a typo, too loose and the database disagrees
// with the parcel.
func TestStatusRankGatesAddressEditing(t *testing.T) {
	const editableUntil = 3 // must match addressEditableUntil in the handler

	editable := []string{
		StatusPending,
		StatusProcessing,
		StatusManifested,
		StatusPendingPickup,
	}
	for _, s := range editable {
		if got := StatusRank(s); got > editableUntil {
			t.Errorf("StatusRank(%q) = %d, want <= %d so the address stays correctable",
				s, got, editableUntil)
		}
	}

	frozen := []string{
		StatusPickedUp,
		StatusInTransit,
		StatusOutForDelivery,
		StatusDelivered,
		StatusReturned,
	}
	for _, s := range frozen {
		if got := StatusRank(s); got <= editableUntil {
			t.Errorf("StatusRank(%q) = %d, want > %d so the address is frozen once the parcel moves",
				s, got, editableUntil)
		}
	}
}

// TestStatusRankHandlesUnknownAndCasing keeps the gate from failing open.
//
// An unknown status must not rank 0: a carrier sending something we do not
// model would otherwise look like "not started yet" and let an address be
// edited under a parcel that is already out for delivery.
func TestStatusRankHandlesUnknownAndCasing(t *testing.T) {
	if got := StatusRank("something-we-do-not-model"); got != -1 {
		t.Errorf("StatusRank(unknown) = %d, want -1", got)
	}
	if got := StatusRank(""); got != -1 {
		t.Errorf("StatusRank(empty) = %d, want -1", got)
	}
	// Carriers are not consistent about casing or padding.
	if StatusRank("  IN_TRANSIT  ") != StatusRank(StatusInTransit) {
		t.Error("StatusRank must normalize casing and surrounding whitespace")
	}
}
