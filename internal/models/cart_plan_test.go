package models

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func product(id primitive.ObjectID, stock int, status string) Product {
	return Product{ID: id, Stock: stock, Status: status, Price: 1000}
}

func TestDuplicateLinesAreMergedNotOverwritten(t *testing.T) {
	id := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{id: product(id, 10, "")}

	plan, adj := PlanCart([]CartLineInput{
		{ProductID: id.Hex(), Quantity: 1},
		{ProductID: id.Hex(), Quantity: 2},
	}, products)

	if len(plan) != 1 {
		t.Fatalf("want one merged line, got %d", len(plan))
	}
	if plan[0].Quantity != 3 {
		t.Errorf("want quantity 3 (1+2), got %d", plan[0].Quantity)
	}
	if len(adj) != 0 {
		t.Errorf("merging is not an adjustment, got %+v", adj)
	}
}

func TestQuantityIsClampedToStockAndReported(t *testing.T) {
	id := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{id: product(id, 2, "")}

	plan, adj := PlanCart([]CartLineInput{{ProductID: id.Hex(), Quantity: 5}}, products)

	if len(plan) != 1 || plan[0].Quantity != 2 {
		t.Fatalf("want quantity clamped to 2, got %+v", plan)
	}
	if len(adj) != 1 || adj[0].Reason != CartAdjustStock {
		t.Fatalf("want one stock adjustment, got %+v", adj)
	}
	if adj[0].Requested != 5 || adj[0].Applied != 2 {
		t.Errorf("adjustment must report 5 requested / 2 applied, got %+v", adj[0])
	}
}

func TestStockIsSharedAcrossSizesOfOneProduct(t *testing.T) {
	id := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{id: product(id, 3, "")}

	plan, adj := PlanCart([]CartLineInput{
		{ProductID: id.Hex(), Size: "S", Quantity: 2},
		{ProductID: id.Hex(), Size: "M", Quantity: 2},
	}, products)

	total := 0
	for _, line := range plan {
		total += line.Quantity
	}
	if total != 3 {
		t.Fatalf("want 3 units total across sizes (stock is shared), got %d in %+v", total, plan)
	}
	if len(adj) != 1 || adj[0].Size != "M" || adj[0].Applied != 1 {
		t.Errorf("want the second size clamped to 1, got %+v", adj)
	}
}

func TestUnavailableProductIsDroppedNotFatal(t *testing.T) {
	live := primitive.NewObjectID()
	gone := primitive.NewObjectID()
	archived := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{
		live:     product(live, 5, ""),
		archived: product(archived, 5, ProductStatusArchived),
	}

	plan, adj := PlanCart([]CartLineInput{
		{ProductID: live.Hex(), Quantity: 1},
		{ProductID: gone.Hex(), Quantity: 1},
		{ProductID: archived.Hex(), Quantity: 1},
	}, products)

	if len(plan) != 1 || plan[0].ProductID != live {
		t.Fatalf("the rest of the cart must survive, got %+v", plan)
	}
	if len(adj) != 2 {
		t.Fatalf("want both bad lines reported, got %+v", adj)
	}
	for _, a := range adj {
		if a.Reason != CartAdjustUnavailable || a.Applied != 0 {
			t.Errorf("want unavailable/0, got %+v", a)
		}
	}
}

func TestOutOfStockProductIsUnavailable(t *testing.T) {
	id := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{id: product(id, 0, "")}

	plan, adj := PlanCart([]CartLineInput{{ProductID: id.Hex(), Quantity: 1}}, products)

	if len(plan) != 0 {
		t.Fatalf("want nothing stored, got %+v", plan)
	}
	if len(adj) != 1 || adj[0].Reason != CartAdjustUnavailable {
		t.Fatalf("want an unavailable adjustment, got %+v", adj)
	}
}

func TestMalformedIdIsReportedNotPanicking(t *testing.T) {
	plan, adj := PlanCart([]CartLineInput{{ProductID: "not-an-object-id", Quantity: 1}}, nil)

	if len(plan) != 0 {
		t.Fatalf("want nothing stored, got %+v", plan)
	}
	if len(adj) != 1 || adj[0].Reason != CartAdjustUnavailable {
		t.Fatalf("want an unavailable adjustment, got %+v", adj)
	}
	if ids := CartProductIDs([]CartLineInput{{ProductID: "nope", Quantity: 1}}); len(ids) != 0 {
		t.Errorf("malformed ids must not reach the query, got %v", ids)
	}
}

func TestEmptyCartClearsWithoutAdjustments(t *testing.T) {
	plan, adj := PlanCart(nil, nil)
	if len(plan) != 0 || len(adj) != 0 {
		t.Fatalf("want an empty plan and no adjustments, got %+v / %+v", plan, adj)
	}
}

func TestNonPositiveQuantitiesAreIgnored(t *testing.T) {
	id := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{id: product(id, 5, "")}

	plan, adj := PlanCart([]CartLineInput{
		{ProductID: id.Hex(), Quantity: 0},
		{ProductID: id.Hex(), Quantity: -3},
	}, products)

	if len(plan) != 0 || len(adj) != 0 {
		t.Fatalf("a zero/negative line is not a request, got %+v / %+v", plan, adj)
	}
}

func TestSubmittedOrderIsPreserved(t *testing.T) {
	a := primitive.NewObjectID()
	b := primitive.NewObjectID()
	products := map[primitive.ObjectID]Product{
		a: product(a, 5, ""),
		b: product(b, 5, ""),
	}

	plan, _ := PlanCart([]CartLineInput{
		{ProductID: b.Hex(), Quantity: 1},
		{ProductID: a.Hex(), Quantity: 1},
	}, products)

	if len(plan) != 2 || plan[0].ProductID != b || plan[1].ProductID != a {
		t.Fatalf("want submission order preserved, got %+v", plan)
	}
}
