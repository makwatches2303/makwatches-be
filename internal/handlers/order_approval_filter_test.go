package handlers

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// The admin listing's ?approval= filter.
//
// It has to agree with models.Order.DispatchApproved, because the panel draws
// its "Awaiting approval" chip from that predicate and its rows from this
// query. A disagreement shows up as shipped orders sitting in the review queue
// -- which is what reading approval.status on its own would do to every order
// placed before approvals existed.

func TestApprovalFilterIsAbsentByDefault(t *testing.T) {
	for _, raw := range []string{"", "   ", "everything", "yes"} {
		if clause := approvalFilterClause(raw); clause != nil {
			t.Errorf("approvalFilterClause(%q) = %v, want nil (no filter)", raw, clause)
		}
	}
}

// Pending means "not approved AND nothing booked": both halves must be in the
// query, or an already-shipped legacy order joins the review queue.
func TestPendingFilterExcludesBookedOrders(t *testing.T) {
	clause := approvalFilterClause(models.DispatchPending)
	if clause == nil {
		t.Fatal("approvalFilterClause(pending) = nil")
	}

	var sawStatus, sawTracking, sawWaybill, sawShipmentID, sawOrderID bool
	for _, c := range clause {
		for field, condition := range c {
			switch field {
			case "approval.status":
				sawStatus = true
				if _, ok := condition.(bson.M)["$ne"]; !ok {
					t.Errorf("approval.status condition = %v, want a $ne", condition)
				}
			case "shipping_info.tracking_number":
				sawTracking = true
			case "shipping_info.waybill":
				sawWaybill = true
			case "shipping_info.provider_shipment_id":
				sawShipmentID = true
			case "shipping_info.provider_order_id":
				sawOrderID = true
			default:
				t.Errorf("unexpected field %q in the pending filter", field)
			}
			// Every shipment clause must admit a missing field, not just an
			// empty string: most orders have no shipping_info at all.
			if field != "approval.status" {
				values, ok := condition.(bson.M)["$in"].([]interface{})
				if !ok {
					t.Errorf("%s: condition = %v, want an $in", field, condition)
					continue
				}
				if len(values) == 0 || values[0] != nil {
					t.Errorf("%s: $in = %v, want nil first so a missing field matches", field, values)
				}
			}
		}
	}

	if !sawStatus {
		t.Error("the pending filter does not constrain approval.status")
	}
	// All four identifiers ShippingInfo.HasShipment reads have to be covered;
	// missing one lets an order booked through that field alone slip in.
	if !sawTracking || !sawWaybill || !sawShipmentID || !sawOrderID {
		t.Errorf("the pending filter misses a shipment identifier: tracking=%v waybill=%v shipmentID=%v orderID=%v",
			sawTracking, sawWaybill, sawShipmentID, sawOrderID)
	}
}

// Approved means "approved OR already booked", so historical orders that were
// dispatched before approvals existed are not reported as unreviewed.
func TestApprovedFilterIncludesAlreadyBookedOrders(t *testing.T) {
	clause := approvalFilterClause(models.DispatchApproved)
	if len(clause) != 1 {
		t.Fatalf("approvalFilterClause(approved) has %d clauses, want 1", len(clause))
	}

	alternatives, ok := clause[0]["$or"].([]bson.M)
	if !ok {
		t.Fatalf("the approved filter is not an $or: %v", clause[0])
	}
	// One for the approval record plus one per shipment identifier.
	if len(alternatives) != 5 {
		t.Fatalf("the approved filter has %d alternatives, want 5", len(alternatives))
	}

	if got := alternatives[0]["approval.status"]; got != models.DispatchApproved {
		t.Errorf("first alternative = %v, want approval.status == %q", got, models.DispatchApproved)
	}
	for _, alt := range alternatives[1:] {
		for field, condition := range alt {
			if _, ok := condition.(bson.M)["$nin"]; !ok {
				t.Errorf("%s: condition = %v, want a $nin (a real identifier is present)", field, condition)
			}
		}
	}
}

// The two filters must partition the orders: nothing may match both, and the
// shipment half of each must be the exact negation of the other.
func TestPendingAndApprovedUseOppositeShipmentTests(t *testing.T) {
	notBooked := bookedShipmentClauses(false)
	booked := bookedShipmentClauses(true)

	if len(notBooked) != len(booked) {
		t.Fatalf("clause counts differ: %d vs %d", len(notBooked), len(booked))
	}
	for i := range notBooked {
		for field, condition := range notBooked[i] {
			opposite, present := booked[i][field]
			if !present {
				t.Errorf("%s is tested for absence but not for presence", field)
				continue
			}
			if _, ok := condition.(bson.M)["$in"]; !ok {
				t.Errorf("%s: not-booked condition = %v, want $in", field, condition)
			}
			if _, ok := opposite.(bson.M)["$nin"]; !ok {
				t.Errorf("%s: booked condition = %v, want $nin", field, opposite)
			}
		}
	}
}
