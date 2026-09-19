package shipping

import (
	"context"
	"strings"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// Dispatch approval, enforced at the one place a parcel can be booked.
//
// # Why the gate lives here
//
// Three HTTP paths could previously reach a carrier: the checkout goroutine,
// the admin retry endpoint and the v1 create endpoint. Checking approval in
// each of them would mean three copies of the rule and three chances to forget
// it. CreateApprovedShipmentForOrder is a single door all three now go
// through, so "nothing reaches a carrier without a named human behind it" is
// one function rather than a convention.
//
// CreateShipmentForOrder itself is deliberately left ungated. It is the
// mechanism -- claim the slot, call the carrier, record the result -- and the
// automatic-dispatch escape hatch and the existing tests use it directly. The
// policy is this file.

// ProviderIsKnown reports whether a name is one of the carriers this system
// speaks to at all. It is a vocabulary check, not a configuration check: see
// Service.Provider for whether the adapter is actually registered.
func ProviderIsKnown(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ProviderShiprocket, ProviderDelhivery:
		return true
	default:
		return false
	}
}

// RequireDispatchApproval refuses an order that no admin has approved.
//
// An order carrying a booked shipment passes whatever its approval record
// says -- see models.Order.DispatchApproved. That is what keeps a parcel
// booked before this gate existed retryable, cancellable and labellable.
func RequireDispatchApproval(order *models.Order) error {
	if order == nil {
		return Errf(CodeInvalidRequest, "cannot dispatch a nil order")
	}
	if order.DispatchApproved() {
		return nil
	}
	return Errf(CodeApprovalRequired,
		"order %s has not been approved for dispatch", order.ID.Hex())
}

// DispatchProviderFor resolves which carrier an order may be booked with.
//
// `requested` is what the caller asked for, which may be empty. An approved
// order names its carrier, and a request for a *different* one is refused
// rather than quietly honoured: an admin approving Shiprocket and a second
// request booking Delhivery is two parcels for one order, and the
// (order, provider) uniqueness index cannot catch it because the pairs differ.
//
// Switching carriers is still possible -- it goes through re-approval, which
// checks that nothing has been booked yet.
func DispatchProviderFor(order *models.Order, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested != "" && !ProviderIsKnown(requested) {
		return "", Errf(CodeInvalidRequest, "unknown shipping provider %q", requested)
	}

	approved := strings.ToLower(strings.TrimSpace(order.ApprovedProvider()))
	switch {
	case approved == "":
		// No carrier was nominated -- an order booked before approval existed,
		// or one approved without naming one. The service falls back to the
		// customer's selection and then to the configured primary, exactly as
		// it always has.
		return requested, nil
	case requested == "", requested == approved:
		return approved, nil
	default:
		return "", Errf(CodeInvalidRequest,
			"order %s was approved for dispatch with %s, not %s",
			order.ID.Hex(), approved, requested)
	}
}

// CreateApprovedShipmentForOrder books a parcel for an order an admin has
// approved, with the carrier they nominated.
//
// Everything else -- idempotency through the (order_id, provider) claim, the
// single request mapping, AWB assignment, failure recording -- is unchanged
// and still lives in CreateShipmentForOrder. This adds the authorization in
// front of it and nothing more.
func (s *Service) CreateApprovedShipmentForOrder(
	ctx context.Context,
	order *models.Order,
	opts CreateOptions,
) (*models.Shipment, error) {
	if err := RequireDispatchApproval(order); err != nil {
		return nil, err
	}

	provider, err := DispatchProviderFor(order, opts.Provider)
	if err != nil {
		return nil, err
	}
	opts.Provider = provider

	return s.CreateShipmentForOrder(ctx, order, opts)
}
