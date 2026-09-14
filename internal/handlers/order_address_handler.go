package handlers

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
)

// addressUpdateBody is the correctable part of a delivery address.
//
// Deliberately not the whole Address: id, userId and isDefault belong to the
// address book, not to one order's delivery details, and letting a request
// set them here would let a caller re-point an order at another account's
// address record.
type addressUpdateBody struct {
	Name    string `json:"name"`
	Street  string `json:"street"`
	City    string `json:"city"`
	State   string `json:"state"`
	ZipCode string `json:"zipCode"`
	Country string `json:"country"`
	Phone   string `json:"phone"`
}

// addressEditableUntil is the movement rank past which a delivery address can
// no longer be corrected here.
//
// Up to and including manifested/pending-pickup the parcel is still with us:
// the booking can be withdrawn and re-made against the new address. Once a
// courier has physically collected it, changing the address in our database
// would be a lie -- the label on the box still says the old one, and
// redirecting a parcel in transit is a carrier-side operation this system
// does not perform.
const addressEditableUntil = 3 // shipping.StatusPendingPickup

// UpdateOrderAddress corrects the delivery address on an order.
//
// PATCH /orders/:orderID/address -- the customer who placed it, or an admin
// taking the correction over the phone.
//
// This exists because a wrong address is not a rare edge case, and until now
// there was no way to fix one: order MAK-20260914-002 was placed against
// pincode 390035, which does not exist, and the only visible symptom was a
// carrier booking that failed forever with no way for either side to act on
// it. The new pincode is checked for serviceability before it is saved, so a
// correction cannot replace one undeliverable address with another.
func (h *ShippingV1Handler) UpdateOrderAddress(c *fiber.Ctx) error {
	order, err := h.orderForActor(c, false)
	if err != nil {
		return authError(c, err)
	}

	actor, _ := c.Locals("user").(*middleware.TokenMetadata)
	isAdmin := actor != nil && actor.Role == "admin"

	// An order that is finished, one way or the other, has no delivery left
	// to redirect.
	switch strings.ToLower(strings.TrimSpace(order.Status)) {
	case "cancelled", "delivered", "returned":
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"code":    "ORDER_NOT_EDITABLE",
			"message": "This order is " + strings.ToLower(order.Status) +
				", so its delivery address can no longer be changed.",
		})
	}

	// Once the courier has the parcel, the printed label is the truth.
	if info := order.ShippingInfo; info != nil && info.ShipmentStatus != "" {
		if shipping.StatusRank(info.ShipmentStatus) > addressEditableUntil {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"code":    "SHIPMENT_IN_TRANSIT",
				"message": "The courier has already collected this parcel, so the address " +
					"cannot be changed here. Contact the courier with the tracking number " +
					"to request a redirect.",
				"detail": "shipment status is " + info.ShipmentStatus,
			})
		}
	}

	var body addressUpdateBody
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"code":    "INVALID_REQUEST",
			"message": "We could not read the address you sent.",
		})
	}

	next := models.Address{
		Name:    strings.TrimSpace(body.Name),
		Street:  strings.TrimSpace(body.Street),
		City:    strings.TrimSpace(body.City),
		State:   strings.TrimSpace(body.State),
		ZipCode: strings.TrimSpace(body.ZipCode),
		Country: strings.TrimSpace(body.Country),
		Phone:   strings.TrimSpace(body.Phone),
	}
	if next.Country == "" {
		next.Country = firstNonEmptyStr(order.ShippingAddress.Country, "India")
	}

	// Field-level errors are returned together and keyed by field, so a form
	// can mark every bad input at once instead of surfacing them one reload
	// at a time.
	fieldErrors := map[string]string{}
	if next.Name == "" {
		fieldErrors["name"] = "Enter the recipient's name."
	}
	if next.Street == "" {
		fieldErrors["street"] = "Enter the street address."
	}
	if next.City == "" {
		fieldErrors["city"] = "Enter the city."
	}
	if next.State == "" {
		fieldErrors["state"] = "Enter the state."
	}
	if !isSixDigitPincode(next.ZipCode) {
		fieldErrors["zipCode"] = "Enter a valid 6-digit pincode."
	}
	if digitsOnly(next.Phone) == "" || len(digitsOnly(next.Phone)) < 10 {
		fieldErrors["phone"] = "Enter a 10-digit phone number the courier can call."
	}
	if len(fieldErrors) > 0 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success":     false,
			"code":        "INVALID_ADDRESS",
			"message":     "Please correct the highlighted fields.",
			"fieldErrors": fieldErrors,
		})
	}

	// The whole point of this endpoint: never save an address we already know
	// we cannot deliver to. This is the check that was missing when the order
	// was placed.
	isCOD := strings.EqualFold(strings.TrimSpace(order.PaymentInfo.Method), "cod")
	result, svcErr := h.Service.Serviceability(c.UserContext(), "", shipping.RateRequest{
		DeliveryPincode: next.ZipCode,
		COD:             isCOD,
		Package:         h.Service.DefaultPackage(),
	}, shipping.QuoteBinding{})
	if svcErr != nil {
		se := shipping.AsError(svcErr)
		switch se.Code {
		case shipping.CodeNotServiceable, shipping.CodeRateUnavailable, shipping.CodeInvalidPincode:
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
				"success": false,
				"code":    "PINCODE_NOT_SERVICEABLE",
				"message": "We could not find a courier that delivers to " + next.ZipCode +
					". Please check the pincode and try again.",
				"fieldErrors": map[string]string{
					"zipCode": "No courier delivers to this pincode.",
				},
			})
		default:
			// A carrier outage must not look like a bad address: telling
			// someone their correct pincode is wrong sends them chasing a
			// problem that is ours.
			return adminShippingError(c, svcErr, "serviceability for address update on order "+order.ID.Hex())
		}
	}
	if result == nil || len(result.Options) == 0 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"code":    "PINCODE_NOT_SERVICEABLE",
			"message": "We could not find a courier that delivers to " + next.ZipCode +
				". Please check the pincode and try again.",
			"fieldErrors": map[string]string{
				"zipCode": "No courier delivers to this pincode.",
			},
		})
	}
	if isCOD && !result.COD {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"success": false,
			"code":    "COD_NOT_AVAILABLE",
			"message": "This is a cash-on-delivery order, and no courier offers " +
				"cash on delivery to " + next.ZipCode + ". Choose a different address, " +
				"or contact us to switch to online payment.",
			"fieldErrors": map[string]string{
				"zipCode": "Cash on delivery is not available at this pincode.",
			},
		})
	}

	// A booking made against the old address has to go. Leaving it would ship
	// the parcel to where it was already going, with our records claiming
	// otherwise.
	cancelledShipment := false
	if order.ShippingInfo.HasShipment() {
		cancelled, cancelErr := h.Service.CancelIfCancellable(c.UserContext(), order)
		if cancelErr != nil {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"success": false,
				"code":    "SHIPMENT_CANCEL_FAILED",
				"message": "This order already has a booking to the old address and we " +
					"could not withdraw it automatically. Cancel the shipment first, " +
					"then change the address.",
				"detail": shipping.AsError(cancelErr).Detail,
			})
		}
		cancelledShipment = cancelled
	}

	now := time.Now()
	actorLabel := "customer"
	if isAdmin {
		actorLabel = "admin"
	}

	update := bson.M{
		"shipping_address":             next,
		"customer_name":                next.Name,
		"customer_phone":               next.Phone,
		"address_updated_at":           now,
		"address_updated_by":           actorLabel,
		"updated_at":                   now,
		"shipping_info.shipment_error": "",
		"shipping_info.error_code":     "",
	}
	if _, err := h.DB.Collections().Orders.UpdateOne(c.UserContext(),
		bson.M{"_id": order.ID}, bson.M{"$set": update}); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"code":    "ADDRESS_UPDATE_FAILED",
			"message": "We could not save the new address. Please try again.",
		})
	}
	h.invalidateOrderCache(c, order)

	order.ShippingAddress = next
	order.CustomerName = next.Name
	order.CustomerPhone = next.Phone

	message := "Delivery address updated."
	if cancelledShipment {
		message += " The previous booking was cancelled — book the shipment again to ship to the new address."
	}

	return c.JSON(fiber.Map{
		"success":           true,
		"message":           message,
		"shipmentCancelled": cancelledShipment,
		"data": fiber.Map{
			"shippingAddress": next,
			"serviceability": fiber.Map{
				"pincode":     result.Pincode,
				"city":        result.City,
				"state":       result.State,
				"cod":         result.COD,
				"prepaid":     result.Prepaid,
				"optionCount": len(result.Options),
			},
		},
	})
}

// isSixDigitPincode reports whether s is exactly six digits. An Indian
// pincode never starts with 0.
func isSixDigitPincode(s string) bool {
	if len(s) != 6 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// digitsOnly strips everything that is not a digit, so "+91 98765 43210"
// counts as ten digits rather than failing a length check on its spacing.
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
