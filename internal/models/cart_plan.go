package models

import "go.mongodb.org/mongo-driver/bson/primitive"

// CartPlanLine is one line the server intends to store for a cart.
type CartPlanLine struct {
	ProductID primitive.ObjectID
	Size      string
	Quantity  int
}

// CartProductIDs returns the distinct, well-formed product ids referenced by
// the submitted lines, so the caller can load them in a single query.
//
// Malformed ids are skipped here and reported by PlanCart.
func CartProductIDs(inputs []CartLineInput) []primitive.ObjectID {
	seen := map[primitive.ObjectID]bool{}
	ids := []primitive.ObjectID{}
	for _, line := range inputs {
		if line.Quantity <= 0 {
			continue
		}
		id, err := primitive.ObjectIDFromHex(line.ProductID)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// PlanCart decides what a submitted cart should actually become, given the
// products as they exist right now.
//
// Kept free of the database so the rules below can be tested directly:
//
//   - Duplicate (product, size) lines are merged, not overwritten -- a client
//     that sent the same line twice meant two of them.
//   - A product that is missing, unpublished or out of stock is dropped with an
//     "unavailable" adjustment rather than failing the whole cart. One item
//     selling out must not cost the customer the rest of their bag.
//   - Stock is shared across the sizes of a product, so quantities are counted
//     against a running remainder; the first line does not get to see the full
//     figure twice.
//   - Nothing is silently reduced: every difference from what was submitted is
//     reported so the customer can be told before they pay.
//
// Order is preserved: lines come back in the order they were first submitted.
func PlanCart(inputs []CartLineInput, products map[primitive.ObjectID]Product) ([]CartPlanLine, []CartAdjustment) {
	type key struct {
		productID primitive.ObjectID
		size      string
	}

	order := []key{}
	wanted := map[key]int{}
	adjustments := []CartAdjustment{}

	for _, line := range inputs {
		if line.Quantity <= 0 {
			continue
		}
		id, err := primitive.ObjectIDFromHex(line.ProductID)
		if err != nil {
			adjustments = append(adjustments, CartAdjustment{
				ProductID: line.ProductID,
				Size:      line.Size,
				Requested: line.Quantity,
				Applied:   0,
				Reason:    CartAdjustUnavailable,
			})
			continue
		}
		k := key{productID: id, size: line.Size}
		if _, seen := wanted[k]; !seen {
			order = append(order, k)
		}
		wanted[k] += line.Quantity
	}

	remaining := map[primitive.ObjectID]int{}
	plan := []CartPlanLine{}

	for _, k := range order {
		requested := wanted[k]
		product, exists := products[k.productID]

		if !exists || !product.IsPublished() {
			adjustments = append(adjustments, CartAdjustment{
				ProductID: k.productID.Hex(),
				Size:      k.size,
				Requested: requested,
				Applied:   0,
				Reason:    CartAdjustUnavailable,
			})
			continue
		}

		left, counted := remaining[k.productID]
		if !counted {
			left = product.Stock
		}

		applied := requested
		if applied > left {
			applied = left
		}
		if applied < 0 {
			applied = 0
		}
		remaining[k.productID] = left - applied

		if applied == 0 {
			adjustments = append(adjustments, CartAdjustment{
				ProductID: k.productID.Hex(),
				Size:      k.size,
				Requested: requested,
				Applied:   0,
				Reason:    CartAdjustUnavailable,
			})
			continue
		}
		if applied < requested {
			adjustments = append(adjustments, CartAdjustment{
				ProductID: k.productID.Hex(),
				Size:      k.size,
				Requested: requested,
				Applied:   applied,
				Reason:    CartAdjustStock,
			})
		}

		plan = append(plan, CartPlanLine{ProductID: k.productID, Size: k.size, Quantity: applied})
	}

	return plan, adjustments
}
