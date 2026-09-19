package handlers

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// Admin order approval: the gate between a placed order and a booked parcel.
//
// # The flow
//
//	customer checks out
//	  -> the order is created in MAK Watches, approval = pending
//	  -> it appears in the admin panel with the customer's chosen delivery
//	     option, the items, the total and the payment status
//	  -> an admin reviews it and POSTs here, naming the carrier
//	  -> and only then is a shipment created with that carrier
//
// Nothing below knows anything about Shiprocket or Delhivery beyond their
// names. The booking itself still goes through shipping.Service, so tracking,
// AWB assignment, labels, pickups, cancellation and the webhooks are exactly
// the code they were.

// approvalOutcome is what the conditional claim below actually did, so the
// response can say "approved" or "already approved" truthfully.
type approvalOutcome string

const (
	// approvalClaimed: this request is the one that approved the order.
	approvalClaimed approvalOutcome = "claimed"
	// approvalReplayed: it was already approved for this same carrier. A
	// double-clicked button, a retried request, a refreshed tab.
	approvalReplayed approvalOutcome = "replayed"
	// approvalSwitched: it was approved for a different carrier, nothing had
	// been booked yet, and the admin moved it.
	approvalSwitched approvalOutcome = "switched"
)

// orderStatesThatCannotDispatch are the states where sending a parcel is
// simply wrong. A cancelled order has no delivery to make, and a returned or
// delivered one has already had its.
var orderStatesThatCannotDispatch = map[string]bool{
	"cancelled": true,
	"returned":  true,
	"delivered": true,
}

// approveOrderRequest is the admin's decision.
type approveOrderRequest struct {
	// Provider is the carrier to dispatch with: "shiprocket" or "delhivery".
	// Empty falls back to the carrier the customer's delivery option named,
	// and then to the configured primary.
	Provider string `json:"provider"`
	// Note is optional free text recorded with the approval.
	Note string `json:"note"`
	// SkipAWB books the parcel without asking for a tracking number straight
	// away, for carriers where assignment is a separate step an operator wants
	// to control.
	SkipAWB bool `json:"skipAwb"`
}

// ApproveOrder approves an order for dispatch and books it with the chosen
// carrier. Admin only.
//
// POST /admin/orders/:orderID/approve
//
// Idempotent. The approval is claimed with a conditional update, so two
// simultaneous clicks cannot both approve, and the booking underneath is
// already idempotent through the (order_id, provider) unique index -- so a
// repeated request returns the existing shipment rather than creating a second
// parcel.
func (h *OrderHandler) ApproveOrder(c *fiber.Ctx) error {
	// The route group already enforces JWT auth plus the admin role. Repeating
	// the check here means a future routing mistake cannot quietly expose a
	// carrier-booking endpoint to a signed-in customer.
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || user == nil {
		return authError(c, errNotAuthenticated)
	}
	if user.Role != "admin" {
		return authError(c, errNotAdmin)
	}

	if h.Shipping == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeShippingUnavailable),
			"message": "Shipping is not configured on this server.",
		})
	}

	orderID, err := primitive.ObjectIDFromHex(strings.TrimSpace(c.Params("orderID")))
	if err != nil {
		return authError(c, errBadOrderID)
	}

	var body approveOrderRequest
	// A malformed or absent body is not fatal: it just means no carrier was
	// named, and the fallback chain below handles that.
	_ = c.BodyParser(&body)

	ctx := c.UserContext()

	var order models.Order
	if err := h.DB.Collections().Orders.FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
		return authError(c, errOrderNotVisible)
	}

	if orderStatesThatCannotDispatch[strings.ToLower(strings.TrimSpace(order.Status))] {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"code":    string(shipping.CodeInvalidRequest),
			"message": "This order is " + order.Status + " and cannot be dispatched.",
		})
	}

	provider, err := h.resolveDispatchProvider(&order, body.Provider)
	if err != nil {
		return adminShippingError(c, err, "approve order "+order.ID.Hex())
	}

	approved, outcome, err := h.claimApproval(ctx, &order, provider, user.UserID.Hex(), body.Note)
	if err != nil {
		return adminShippingError(c, err, "approve order "+order.ID.Hex())
	}

	log.Printf("[ORDER-APPROVAL] order=%s provider=%s admin=%s outcome=%s",
		approved.ID.Hex(), provider, user.UserID.Hex(), outcome)

	// Book the parcel now that the order is authorised. This goes through the
	// approved-only door, so the authorization is enforced by the shipping
	// service itself rather than only by this handler.
	shipment, bookErr := h.Shipping.CreateApprovedShipmentForOrder(ctx, approved, shipping.CreateOptions{
		Provider:  provider,
		AssignAWB: !body.SkipAWB,
	})
	h.invalidateOrderCaches(ctx, approved)

	if bookErr != nil {
		// The approval stands. The decision was made and recorded; only the
		// carrier call failed, and the existing retry endpoint can pick it up
		// without a second review. The failure is already written onto the
		// order and the shipment record by shipping.Service.
		return adminShippingError(c, bookErr, "dispatch order "+approved.ID.Hex())
	}

	message := "Order approved and dispatched with " + provider + "."
	switch outcome {
	case approvalReplayed:
		message = "Order was already approved for " + provider + "; the existing shipment was returned."
	case approvalSwitched:
		message = "Order re-approved for dispatch with " + provider + "."
	}

	return c.JSON(fiber.Map{
		"success":     true,
		"message":     message,
		"outcome":     string(outcome),
		"provider":    provider,
		"awbAssigned": shipment.TrackingNumber != "",
		"approval":    approved.Approval,
		"data":        shipment,
	})
}

// resolveDispatchProvider decides which carrier to book with.
//
// Preference order: what the admin picked, then the carrier behind the
// delivery option the customer chose at checkout, then the configured primary.
// Whatever comes out must be a carrier this deployment actually has registered
// -- naming one that is not configured fails later, deep in the booking, with
// an error that does not say which choice was wrong.
func (h *OrderHandler) resolveDispatchProvider(order *models.Order, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))

	if requested != "" && !shipping.ProviderIsKnown(requested) {
		return "", shipping.Errf(shipping.CodeInvalidRequest,
			"unknown shipping provider %q", requested)
	}

	// An approved order's carrier is authoritative once a parcel exists:
	// asking for a different one then is a second shipment for one order,
	// which the per-(order, provider) uniqueness index cannot catch because
	// the pairs differ. Switching before anything is booked is handled in
	// claimApproval.
	if approved := order.ApprovedProvider(); approved != "" && requested != "" && approved != requested {
		if order.ShippingInfo.HasShipment() {
			return "", shipping.Errf(shipping.CodeAlreadyExists,
				"order %s is already dispatched with %s", order.ID.Hex(), approved)
		}
	}

	provider := requested
	if provider == "" {
		// A repeated approval that names nothing must land on the carrier
		// already chosen, not re-derive one -- otherwise a second click could
		// read as a request to switch carriers.
		provider = strings.ToLower(strings.TrimSpace(order.ApprovedProvider()))
	}
	if provider == "" && order.ShippingOption != nil {
		provider = strings.ToLower(strings.TrimSpace(order.ShippingOption.Provider))
	}
	if provider == "" {
		provider = h.Shipping.PrimaryProviderName()
	}
	if _, err := h.Shipping.Provider(provider); err != nil {
		return "", err
	}
	return provider, nil
}

// claimApproval records the approval, exactly once.
//
// The write is conditional on the state this handler read, which is what makes
// it safe against two admins (or two clicks) arriving together: whichever
// update matches first wins, and the loser re-reads and is told what actually
// happened rather than overwriting it.
//
// Returns the order as stored after the claim, so the caller books against the
// approved document rather than the pre-approval snapshot it loaded.
func (h *OrderHandler) claimApproval(
	ctx context.Context,
	order *models.Order,
	provider, adminID, note string,
) (*models.Order, approvalOutcome, error) {
	now := time.Now()
	approval := models.OrderApproval{
		Status:     models.DispatchApproved,
		Provider:   provider,
		ApprovedBy: adminID,
		ApprovedAt: now,
		Note:       strings.TrimSpace(note),
	}

	priorProvider := order.ApprovedProvider()
	alreadyApproved := order.ApprovalState() == models.DispatchApproved

	outcome := approvalClaimed
	var filter bson.M

	switch {
	case alreadyApproved && priorProvider != "" && priorProvider != provider:
		// Moving an approved order to a different carrier. Only legitimate
		// while nothing has been booked.
		if order.ShippingInfo.HasShipment() {
			return nil, "", shipping.Errf(shipping.CodeAlreadyExists,
				"order %s is already dispatched with %s; cancel that shipment before switching carrier",
				order.ID.Hex(), priorProvider)
		}
		outcome = approvalSwitched
		filter = bson.M{
			"_id":               order.ID,
			"approval.status":   models.DispatchApproved,
			"approval.provider": priorProvider,
		}

	case alreadyApproved && priorProvider == provider:
		outcome = approvalReplayed
		fallthrough

	default:
		// Matches an order with no approval record at all ($ne also matches a
		// missing field), one still pending, one already approved for this
		// same carrier -- the replay case, where rewriting the record is
		// harmless and keeps the claim a single round trip -- and one approved
		// without a carrier named, which is an incomplete decision rather than
		// a competing one.
		filter = bson.M{
			"_id": order.ID,
			"$or": []bson.M{
				{"approval.status": bson.M{"$ne": models.DispatchApproved}},
				{"approval.provider": provider},
				{"approval.provider": bson.M{"$in": []interface{}{nil, ""}}},
			},
		}
	}

	res, err := h.DB.Collections().Orders.UpdateOne(ctx, filter, bson.M{"$set": bson.M{
		"approval":   approval,
		"updated_at": now,
	}})
	if err != nil {
		return nil, "", shipping.Wrap(shipping.CodeShipmentCreationFailed, err,
			"could not record the approval for order %s", order.ID.Hex())
	}

	if res.MatchedCount == 0 {
		// Someone else moved it between the read and the write. Re-read and
		// report the truth rather than assuming.
		var current models.Order
		readErr := h.DB.Collections().Orders.FindOne(ctx, bson.M{"_id": order.ID}).Decode(&current)
		if errors.Is(readErr, mongo.ErrNoDocuments) {
			return nil, "", shipping.Errf(shipping.CodeInvalidRequest,
				"order %s no longer exists", order.ID.Hex())
		}
		if readErr != nil {
			return nil, "", shipping.Wrap(shipping.CodeShipmentCreationFailed, readErr,
				"could not re-read order %s after a contended approval", order.ID.Hex())
		}
		if current.ApprovalState() == models.DispatchApproved && current.ApprovedProvider() == provider {
			// It landed on the state we wanted anyway.
			return &current, approvalReplayed, nil
		}
		return nil, "", shipping.Errf(shipping.CodeAlreadyExists,
			"order %s was approved for dispatch with %s by another request",
			order.ID.Hex(), current.ApprovedProvider())
	}

	updated := *order
	updated.Approval = &approval
	updated.UpdatedAt = now
	return &updated, outcome, nil
}

// invalidateOrderCaches drops the cached order views an approval affects.
func (h *OrderHandler) invalidateOrderCaches(ctx context.Context, order *models.Order) {
	h.DB.CacheDel(ctx, "order:"+order.ID.Hex())
	h.DB.CacheDel(ctx, "orders:"+order.UserID.Hex())
}
