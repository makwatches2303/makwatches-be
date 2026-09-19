package models

import "testing"

// How an order's dispatch-approval state is read.
//
// These three helpers are what the shipping gate, the admin listing and the
// panel all branch on, and the awkward cases are historical documents: an
// order written before approval existed has no record at all, and one written
// before that already has a parcel.

func TestApprovalStateDefaultsToPending(t *testing.T) {
	cases := map[string]*Order{
		"no approval record":   {},
		"empty status string":  {Approval: &OrderApproval{}},
		"explicitly pending":   {Approval: &OrderApproval{Status: DispatchPending}},
		"pending with carrier": {Approval: &OrderApproval{Status: DispatchPending, Provider: "shiprocket"}},
	}
	for name, order := range cases {
		if got := order.ApprovalState(); got != DispatchPending {
			t.Errorf("%s: ApprovalState() = %q, want %q", name, got, DispatchPending)
		}
		if order.DispatchApproved() {
			t.Errorf("%s: DispatchApproved() = true, want false", name)
		}
	}
}

func TestApprovalStateReadsAnApprovedRecord(t *testing.T) {
	order := &Order{Approval: &OrderApproval{
		Status:   DispatchApproved,
		Provider: "delhivery",
	}}

	if got := order.ApprovalState(); got != DispatchApproved {
		t.Fatalf("ApprovalState() = %q, want %q", got, DispatchApproved)
	}
	if !order.DispatchApproved() {
		t.Fatal("DispatchApproved() = false for an approved order")
	}
	if got := order.ApprovedProvider(); got != "delhivery" {
		t.Fatalf("ApprovedProvider() = %q, want %q", got, "delhivery")
	}
}

// An order booked before the approval gate existed has no record, but it does
// have a parcel -- and that parcel still has to be trackable, re-AWB-able,
// labellable and cancellable. Every one of those paths runs through the same
// authorization, so treating a booked order as unapproved would break all of
// them at once.
func TestAlreadyBookedOrdersCountAsApproved(t *testing.T) {
	cases := map[string]*ShippingInfo{
		"legacy delhivery waybill": {Provider: "delhivery", Waybill: "1491110022334"},
		"modern tracking number":   {Provider: "shiprocket", TrackingNumber: "1491110022334"},
		"booked, awaiting an AWB":  {Provider: "shiprocket", ProviderShipmentID: "998877"},
		"carrier order id only":    {Provider: "shiprocket", ProviderOrderID: "665544"},
	}
	for name, info := range cases {
		order := &Order{ShippingInfo: info}
		if !order.DispatchApproved() {
			t.Errorf("%s: DispatchApproved() = false; a booked parcel must stay operable", name)
		}
		// It is still *reported* as pending: nobody clicked approve, and the
		// panel should say so rather than inventing an approver.
		if got := order.ApprovalState(); got != DispatchPending {
			t.Errorf("%s: ApprovalState() = %q, want %q", name, got, DispatchPending)
		}
	}
}

// A shipping record that exists but holds no carrier identifier is a failed or
// in-flight booking, not a dispatched one.
func TestAnEmptyShippingRecordIsNotApproval(t *testing.T) {
	order := &Order{ShippingInfo: &ShippingInfo{
		Provider:      "shiprocket",
		ShipmentError: "carrier rejected the pickup location",
	}}
	if order.DispatchApproved() {
		t.Fatal("DispatchApproved() = true for an order whose booking failed")
	}
}

func TestApprovalHelpersTolerateNil(t *testing.T) {
	var order *Order
	if got := order.ApprovalState(); got != DispatchPending {
		t.Errorf("ApprovalState() on nil = %q, want %q", got, DispatchPending)
	}
	if order.DispatchApproved() {
		t.Error("DispatchApproved() on nil = true, want false")
	}
	if got := order.ApprovedProvider(); got != "" {
		t.Errorf("ApprovedProvider() on nil = %q, want empty", got)
	}
}
