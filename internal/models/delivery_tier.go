package models

// Delivery speed, as the customer chose it.
//
// # Why this is a stored classification rather than a computed one
//
// A carrier quotes a number of days. A shopper chooses "Express" or "Economy".
// The mapping between the two is a presentation decision, and it has to be
// recorded at the moment the order is placed: the boundaries below could be
// retuned next quarter, and an order must keep meaning what the customer was
// shown when they paid, not what the current thresholds would say about it.
//
// So the tier is derived here, server-side, from the *signed* quote the
// customer selected -- never from anything the browser sends -- and persisted
// on the order's RateChoice. The admin panel reads it back rather than
// re-deriving it, and falls back to deriving from the stored day count only
// for orders placed before this field existed.
//
// # Where the boundaries come from
//
// They are the ones the system already uses, not new invention:
//
//   - 2 days is the existing "Air" charge tier (models.ShippingTierConfig,
//     AirCharge, ≤ 2 days) and the existing storefront's "Express" label.
//   - 5 days is the existing storefront boundary between "Standard" and
//     everything slower.
//   - 9 days splits the old open-ended "Economy" bucket so a genuinely long
//     lane reads as flexible rather than being lumped in with a one-week one.
//
// Anything without an estimate is Standard. That is deliberate and unchanged:
// "Economy" would assert a slowness the carrier never stated, and "Express" a
// speed it never promised.
const (
	DeliveryTierExpress  = "express"
	DeliveryTierStandard = "standard"
	DeliveryTierEconomy  = "economy"
	DeliveryTierFlexible = "flexible"
)

// DeliveryTierFor classifies a carrier's delivery estimate in days.
//
// `days` is the carrier's own EstimatedDeliveryDays, as carried in the signed
// quote. Zero or negative means the carrier gave no estimate.
func DeliveryTierFor(days int) string {
	switch {
	case days <= 0:
		return DeliveryTierStandard
	case days <= 2:
		return DeliveryTierExpress
	case days <= 5:
		return DeliveryTierStandard
	case days <= 8:
		return DeliveryTierEconomy
	default:
		return DeliveryTierFlexible
	}
}

// DeliveryTierPriority ranks tiers by urgency, highest first.
//
// Used for "which of these orders asked for the fastest delivery" -- the
// question an admin is answering when they decide what to approve next. It is
// a display ordering only: nothing in the approval or dispatch path reads it,
// and a high priority never approves or books anything on its own.
func DeliveryTierPriority(tier string) int {
	switch tier {
	case DeliveryTierExpress:
		return 3
	case DeliveryTierStandard:
		return 2
	case DeliveryTierEconomy:
		return 1
	case DeliveryTierFlexible:
		return 0
	default:
		return -1
	}
}

// DeliveryTierOf reports the tier recorded on a delivery choice, deriving it
// from the stored day count when the field is absent.
//
// The fallback is what keeps historical orders readable: every order placed
// before this field existed still carries EstimatedDeliveryDays, which is what
// the tier was computed from in the first place.
func DeliveryTierOf(choice *RateChoice) string {
	if choice == nil {
		return ""
	}
	if choice.DeliveryTier != "" {
		return choice.DeliveryTier
	}
	return DeliveryTierFor(choice.EstimatedDeliveryDays)
}
