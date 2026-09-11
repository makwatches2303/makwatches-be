package shiprocket

import (
	"strings"

	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// statusByCode maps Shiprocket's numeric shipment status onto MAK fulfillment
// states.
//
// Only codes with an unambiguous fulfillment meaning are listed. An unlisted
// code deliberately falls through to StatusProcessing, which has the lowest
// rank of the non-initial states -- combined with shipping.CanAdvance, that
// means an unrecognized code can never walk an order backwards from a state we
// do understand.
var statusByCode = map[int]string{
	1:  shipping.StatusProcessing,     // NEW
	2:  shipping.StatusProcessing,     // INVOICED
	3:  shipping.StatusManifested,     // READY TO SHIP
	4:  shipping.StatusPendingPickup,  // PICKUP SCHEDULED
	5:  shipping.StatusCancelled,      // CANCELED
	6:  shipping.StatusInTransit,      // SHIPPED
	7:  shipping.StatusDelivered,      // DELIVERED
	8:  shipping.StatusReturned,       // RETURNED
	9:  shipping.StatusReturned,       // RTO INITIATED
	10: shipping.StatusReturned,       // RTO DELIVERED
	11: shipping.StatusReturned,       // RTO ACKNOWLEDGED
	12: shipping.StatusFailed,         // LOST
	13: shipping.StatusPendingPickup,  // PICKUP ERROR
	14: shipping.StatusReturned,       // RTO NDR
	15: shipping.StatusReturned,       // RTO OFD
	16: shipping.StatusProcessing,     // CANCELLATION REQUESTED
	17: shipping.StatusOutForDelivery, // OUT FOR DELIVERY
	18: shipping.StatusInTransit,      // IN TRANSIT
	19: shipping.StatusPendingPickup,  // OUT FOR PICKUP
	20: shipping.StatusPendingPickup,  // PICKUP EXCEPTION
	21: shipping.StatusUndelivered,    // UNDELIVERED
	22: shipping.StatusInTransit,      // DELAYED
	23: shipping.StatusInTransit,      // PARTIAL DELIVERED
	24: shipping.StatusFailed,         // DESTROYED
	25: shipping.StatusFailed,         // DAMAGED
	26: shipping.StatusDelivered,      // FULFILLED
	27: shipping.StatusPendingPickup,  // PICKUP BOOKED
	38: shipping.StatusInTransit,      // REACHED DESTINATION HUB
	42: shipping.StatusPickedUp,       // PICKED UP
	44: shipping.StatusFailed,         // DISPOSED OFF
	45: shipping.StatusCancelled,      // CANCELLED BEFORE DISPATCH
	46: shipping.StatusReturned,       // RTO IN TRANSIT
	50: shipping.StatusInTransit,      // IN FLIGHT
	51: shipping.StatusPickedUp,       // HANDOVER TO COURIER
	52: shipping.StatusManifested,     // SHIPMENT BOOKED
	76: shipping.StatusUndelivered,    // UNTRACEABLE
	77: shipping.StatusUndelivered,    // ISSUE RELATED TO RECIPIENT
	78: shipping.StatusReturned,       // REACHED BACK AT SELLER CITY
}

// statusByLabel is the fallback when a payload carries only human-readable
// text. Matched on a normalized substring because Shiprocket's labels vary in
// punctuation and case between endpoints.
var statusByLabel = []struct {
	needle string
	status string
}{
	{"rto", shipping.StatusReturned},
	{"return", shipping.StatusReturned},
	{"out for delivery", shipping.StatusOutForDelivery},
	{"out for pickup", shipping.StatusPendingPickup},
	{"pickup scheduled", shipping.StatusPendingPickup},
	{"pickup booked", shipping.StatusPendingPickup},
	{"pickup error", shipping.StatusPendingPickup},
	{"pickup exception", shipping.StatusPendingPickup},
	{"picked up", shipping.StatusPickedUp},
	{"handover", shipping.StatusPickedUp},
	// "undelivered" must be tested before "delivered": it contains it as a
	// substring, and matching the wrong one would report a failed delivery
	// attempt as a completed delivery.
	{"undelivered", shipping.StatusUndelivered},
	{"not delivered", shipping.StatusUndelivered},
	{"untraceable", shipping.StatusUndelivered},
	{"delivered", shipping.StatusDelivered},
	{"fulfilled", shipping.StatusDelivered},
	{"cancel", shipping.StatusCancelled},
	{"lost", shipping.StatusFailed},
	{"damaged", shipping.StatusFailed},
	{"destroyed", shipping.StatusFailed},
	{"in transit", shipping.StatusInTransit},
	{"in flight", shipping.StatusInTransit},
	{"shipped", shipping.StatusInTransit},
	{"reached", shipping.StatusInTransit},
	{"ready to ship", shipping.StatusManifested},
	{"shipment booked", shipping.StatusManifested},
	{"invoiced", shipping.StatusProcessing},
	{"new", shipping.StatusProcessing},
}

// mapStatus resolves a Shiprocket status to a MAK fulfillment state, trying
// the numeric code first and the label second.
//
// "delivered" here means the parcel moved, nothing more. Payment state is
// decided by the payment domain; a carrier callback never settles money.
func mapStatus(code int, label string) string {
	if s, ok := statusByCode[code]; ok {
		return s
	}
	norm := strings.ToLower(strings.TrimSpace(label))
	norm = strings.ReplaceAll(norm, "_", " ")
	norm = strings.ReplaceAll(norm, "-", " ")
	if norm != "" {
		// Longest-first ordering matters: "out for delivery" must win over
		// "delivered", so the table is scanned in declaration order and the
		// more specific needles are listed above the general ones.
		for _, entry := range statusByLabel {
			if strings.Contains(norm, entry.needle) {
				return entry.status
			}
		}
	}
	return shipping.StatusProcessing
}
