package catalog

import (
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// Walking a catalogue batch by batch, with Mongo stood in for by a slice.
//
// The find is the one part that needs a database; everything that decides
// where a batch stops and what the next one asks for does not. `catalogue`
// below applies the same sort and the same keyset predicate the query builds,
// so a cursor minted by assemble is resolved the way the server would resolve
// it -- which is what makes "24, 48, 72, 96" a real assertion rather than a
// restatement of the code.

// catalogue is an in-memory products collection, ordered newest first with an
// _id tie-break, exactly as List sorts.
type catalogue struct {
	products []models.Product
}

func newCatalogue(size int) *catalogue {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	products := make([]models.Product, 0, size)

	for i := 0; i < size; i++ {
		products = append(products, models.Product{
			ID:   primitive.NewObjectID(),
			Name: "Watch",
			// Deliberately coarse: every fourth product shares a timestamp, so
			// the tie-break is exercised on most batch boundaries rather than
			// never. A cursor that ignored _id would skip or repeat here.
			CreatedAt: base.Add(time.Duration(i/4) * time.Minute),
		})
	}

	// Newest first, then _id descending -- the total order List imposes.
	sort.SliceStable(products, func(a, b int) bool {
		if !products[a].CreatedAt.Equal(products[b].CreatedAt) {
			return products[a].CreatedAt.After(products[b].CreatedAt)
		}
		return products[a].ID.Hex() > products[b].ID.Hex()
	})

	return &catalogue{products: products}
}

// find returns up to limit+1 products at or after the cursor, the way the
// keyset predicate and SetLimit(limit+1) do together.
func (c *catalogue) find(cur *Cursor, limit int) []models.Product {
	out := make([]models.Product, 0, limit+1)

	for _, p := range c.products {
		if cur != nil && !after(p, *cur) {
			continue
		}
		out = append(out, p)
		if len(out) == limit+1 {
			break
		}
	}
	return out
}

// after is keysetFilter's predicate, for a descending (createdAt, _id) sort.
func after(p models.Product, cur Cursor) bool {
	boundary := cur.Value.(time.Time)
	if p.CreatedAt.Before(boundary) {
		return true
	}
	if p.CreatedAt.Equal(boundary) {
		return p.ID.Hex() < cur.ID.Hex()
	}
	return false
}

func TestCursorWalkLoadsEveryProductExactlyOnce(t *testing.T) {
	const size, limit = 100, 24

	shop := newCatalogue(size)
	q := Query{SortBy: "createdAt", Order: "desc", Page: 1, Limit: limit}
	q.normalize()

	var (
		loaded  []models.Product
		sizes   []int
		cursors []string
		cur     *Cursor
	)

	// Bounded so a listing that never terminates fails rather than hangs.
	for batch := 0; batch < size; batch++ {
		page, err := assemble(q, shop.find(cur, limit), 0, cur == nil && false)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}

		loaded = append(loaded, page.Items...)
		sizes = append(sizes, len(loaded))

		if !page.HasMore {
			if page.NextCursor != "" {
				t.Error("a finished listing must not offer a cursor to continue from")
			}
			break
		}

		if page.NextCursor == "" {
			t.Fatal("HasMore without a cursor leaves the client unable to continue")
		}
		cursors = append(cursors, page.NextCursor)

		decoded, err := decodeCursor(page.NextCursor, q)
		if err != nil {
			t.Fatalf("the server would reject its own cursor: %v", err)
		}
		cur = decoded
	}

	want := []int{24, 48, 72, 96, 100}
	if len(sizes) != len(want) {
		t.Fatalf("batches = %v, want %v", sizes, want)
	}
	for i, n := range want {
		if sizes[i] != n {
			t.Errorf("after batch %d: %d products, want %d", i+1, sizes[i], n)
		}
	}

	// Every product once, in catalogue order.
	if len(loaded) != size {
		t.Fatalf("loaded %d products, want %d", len(loaded), size)
	}
	seen := make(map[primitive.ObjectID]bool, size)
	for _, p := range loaded {
		if seen[p.ID] {
			t.Fatalf("product %s was served twice", p.ID.Hex())
		}
		seen[p.ID] = true
	}
	for i := range loaded {
		if loaded[i].ID != shop.products[i].ID {
			t.Fatalf("position %d is out of catalogue order", i)
		}
	}

	// A repeated cursor would mean a repeated window.
	distinct := map[string]bool{}
	for _, c := range cursors {
		if distinct[c] {
			t.Fatal("the same cursor was issued twice")
		}
		distinct[c] = true
	}
}

func TestAssembleDropsTheOverFetchedDocument(t *testing.T) {
	q := Query{Limit: 24}
	q.normalize()

	// 25 fetched: 24 to serve, one to prove there is more.
	page, err := assemble(q, make([]models.Product, 25), 0, false)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	if len(page.Items) != 24 {
		t.Errorf("served %d products, want 24", len(page.Items))
	}
	if !page.HasMore {
		t.Error("HasMore = false, want true")
	}
}

func TestAssembleEndsOnAShortBatch(t *testing.T) {
	q := Query{Limit: 24}
	q.normalize()

	page, err := assemble(q, make([]models.Product, 10), 0, false)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	if len(page.Items) != 10 {
		t.Errorf("served %d products, want 10", len(page.Items))
	}
	if page.HasMore {
		t.Error("HasMore = true on a short batch, want false")
	}
	if page.NextCursor != "" {
		t.Error("a short batch must not offer a cursor")
	}
}

// The first request a listing makes is a page request, and its response is
// what the feed continues from. It has to carry both the count the page
// header renders and the cursor the feed needs.
func TestFirstPageCarriesTotalsAndACursor(t *testing.T) {
	q := Query{SortBy: "createdAt", Order: "desc", Page: 1, Limit: 24}
	q.normalize()

	shop := newCatalogue(887)
	page, err := assemble(q, shop.find(nil, 24), 887, true)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	if page.Total != 887 {
		t.Errorf("total = %d, want 887", page.Total)
	}
	if page.Pages != 37 {
		t.Errorf("pages = %d, want 37", page.Pages)
	}
	if !page.TotalKnown {
		t.Error("TotalKnown = false on a page request")
	}
	if !page.HasMore {
		t.Error("HasMore = false with 887 pieces across 24-item pages")
	}
	if page.NextCursor == "" {
		t.Fatal("the first page must hand the feed a cursor to continue from")
	}
	if _, err := decodeCursor(page.NextCursor, q); err != nil {
		t.Errorf("the server would reject its own first cursor: %v", err)
	}
}

// Page mode still answers the way it always did: a page in the middle of a
// listing reports more to come even though its own batch was not over-fetched
// past the count.
func TestPageModeUsesTheCountForHasMore(t *testing.T) {
	q := Query{Page: 37, Limit: 24}
	q.normalize()

	// The last page of 887: 23 products, nothing after them.
	page, err := assemble(q, make([]models.Product, 23), 887, true)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if page.HasMore {
		t.Error("the last page reports more to come")
	}

	q.Page = 2
	page, err = assemble(q, make([]models.Product, 25), 887, true)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !page.HasMore {
		t.Error("page 2 of 37 reports no more to come")
	}
}
