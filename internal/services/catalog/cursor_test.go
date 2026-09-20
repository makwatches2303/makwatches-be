package catalog

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// These tests cover cursor encoding, validation and the keyset predicate.
// None of them touch Mongo: the point of moving query construction into this
// package is that the parts that are easy to get wrong are the parts that can
// be checked without a database.

// newest is the query the storefront issues by default, normalized the way
// List normalizes it before a cursor is ever minted.
func newest() Query {
	q := Query{SortBy: "createdAt", Order: "desc", Limit: 24}
	q.normalize()
	return q
}

func TestCursorRoundTripsEachSortField(t *testing.T) {
	id := primitive.NewObjectID()
	created := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

	cases := []struct {
		name  string
		sort  string
		value any
		want  any
	}{
		{"createdAt", "createdAt", created, created},
		{"price", "price", 2499.5, 2499.5},
		{"name", "name", "Fastrack Groove", "Fastrack Groove"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := Query{SortBy: tc.sort, Order: "desc"}
			q.normalize()

			raw, err := encodeCursor(q, tc.value, id)
			if err != nil {
				t.Fatalf("encodeCursor: %v", err)
			}

			got, err := decodeCursor(raw, q)
			if err != nil {
				t.Fatalf("decodeCursor: %v", err)
			}
			if got.ID != id {
				t.Errorf("id = %v, want %v", got.ID, id)
			}
			if got.Value != tc.want {
				t.Errorf("value = %#v, want %#v", got.Value, tc.want)
			}
		})
	}
}

// A second-resolution round trip would put every product sharing that second
// on the wrong side of the seek, so the encoding has to keep nanoseconds.
func TestCursorPreservesSubSecondPrecision(t *testing.T) {
	q := newest()
	id := primitive.NewObjectID()
	created := time.Date(2026, 1, 1, 0, 0, 0, 987654321, time.UTC)

	raw, err := encodeCursor(q, created, id)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	got, err := decodeCursor(raw, q)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}

	if !got.Value.(time.Time).Equal(created) {
		t.Fatalf("value = %v, want %v", got.Value, created)
	}
}

// The guarantee this whole mechanism rests on: a cursor minted for one listing
// must not window another.
func TestCursorRejectedAcrossQueries(t *testing.T) {
	men := Query{MainCategory: "Men", SortBy: "createdAt", Order: "desc"}
	men.normalize()

	raw, err := encodeCursor(men, time.Now().UTC(), primitive.NewObjectID())
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}

	// Each of these differs from `men` in exactly one way a shopper can cause.
	others := map[string]Query{
		"different scope":       {MainCategory: "Women", SortBy: "createdAt", Order: "desc"},
		"no scope":              {SortBy: "createdAt", Order: "desc"},
		"different sort field":  {MainCategory: "Men", SortBy: "price", Order: "desc"},
		"different sort order":  {MainCategory: "Men", SortBy: "createdAt", Order: "asc"},
		"added search term":     {MainCategory: "Men", Search: "fastrack", SortBy: "createdAt", Order: "desc"},
		"added brand filter":    {MainCategory: "Men", Brands: []string{"Fastrack"}, SortBy: "createdAt", Order: "desc"},
		"added price floor":     {MainCategory: "Men", MinPrice: f(1000), SortBy: "createdAt", Order: "desc"},
		"added in-stock filter": {MainCategory: "Men", InStock: true, SortBy: "createdAt", Order: "desc"},
		"added subcategory":     {MainCategory: "Men", Subcategory: "Metal watch", SortBy: "createdAt", Order: "desc"},
	}

	for name, other := range others {
		t.Run(name, func(t *testing.T) {
			other.normalize()
			if _, err := decodeCursor(raw, other); err != ErrInvalidCursor {
				t.Fatalf("err = %v, want ErrInvalidCursor", err)
			}
		})
	}
}

// Page and Limit say where the caller is, not which results they are looking
// at, so they must not invalidate a cursor.
func TestCursorSurvivesPageAndLimitChange(t *testing.T) {
	issued := Query{MainCategory: "Men", SortBy: "createdAt", Order: "desc", Page: 1, Limit: 24}
	issued.normalize()

	raw, err := encodeCursor(issued, time.Now().UTC(), primitive.NewObjectID())
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}

	later := Query{MainCategory: "Men", SortBy: "createdAt", Order: "desc", Page: 7, Limit: 48}
	later.normalize()

	if _, err := decodeCursor(raw, later); err != nil {
		t.Fatalf("decodeCursor: %v, want success", err)
	}
}

func TestCursorRejectsMalformedInput(t *testing.T) {
	q := newest()
	valid, err := encodeCursor(q, time.Now().UTC(), primitive.NewObjectID())
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}

	cases := map[string]string{
		"empty":           "",
		"whitespace":      "   ",
		"not base64":      "!!!!not-base64!!!!",
		"not json":        base64.RawURLEncoding.EncodeToString([]byte("plain text")),
		"truncated":       valid[:len(valid)/2],
		"oversized":       strings.Repeat("A", maxCursorLength+1),
		"bad object id":   mutate(t, q, func(p *cursorPayload) { p.I = "not-an-object-id" }),
		"wrong value typ": mutate(t, q, func(p *cursorPayload) { p.V = json.RawMessage(`"not a timestamp"`) }),
		"forged finger":   mutate(t, q, func(p *cursorPayload) { p.F = "deadbeefdeadbeef" }),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(raw, q); err != ErrInvalidCursor {
				t.Fatalf("err = %v, want ErrInvalidCursor", err)
			}
		})
	}
}

// A filter value containing the fingerprint's separator must not be able to
// impersonate a different query's field layout.
func TestFingerprintSeparatorCannotBeForged(t *testing.T) {
	a := Query{Search: "men\x1f", MainCategory: "", SortBy: "createdAt", Order: "desc"}
	b := Query{Search: "men", MainCategory: "", SortBy: "createdAt", Order: "desc"}
	a.normalize()
	b.normalize()

	if fingerprint(a) == fingerprint(b) {
		t.Fatal("queries differing only by a separator character share a fingerprint")
	}
}

func TestKeysetFilterDescending(t *testing.T) {
	id := primitive.NewObjectID()
	when := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	got := keysetFilter("created_at", true, Cursor{Value: when, ID: id})
	branches, ok := got["$or"].([]bson.M)
	if !ok || len(branches) != 2 {
		t.Fatalf("filter = %#v, want two $or branches", got)
	}

	// Strictly past the boundary value.
	strict, ok := branches[0]["created_at"].(bson.M)
	if !ok || strict["$lt"] != when {
		t.Errorf("strict branch = %#v, want created_at $lt %v", branches[0], when)
	}
	// Tied on the value, strictly past the id. Without this branch every
	// product sharing the boundary timestamp would be skipped.
	if branches[1]["created_at"] != when {
		t.Errorf("tie branch value = %#v, want %v", branches[1]["created_at"], when)
	}
	tie, ok := branches[1]["_id"].(bson.M)
	if !ok || tie["$lt"] != id {
		t.Errorf("tie branch = %#v, want _id $lt %v", branches[1], id)
	}
}

func TestKeysetFilterAscendingMirrorsDescending(t *testing.T) {
	got := keysetFilter("price", false, Cursor{Value: 1999.0, ID: primitive.NewObjectID()})
	branches := got["$or"].([]bson.M)

	if _, ok := branches[0]["price"].(bson.M)["$gt"]; !ok {
		t.Errorf("ascending strict branch = %#v, want $gt", branches[0])
	}
	if _, ok := branches[1]["_id"].(bson.M)["$gt"]; !ok {
		t.Errorf("ascending tie branch = %#v, want $gt", branches[1])
	}
}

// normalize is what stops a caller asking for the whole catalogue in one
// request, which is the failure mode progressive loading exists to avoid.
func TestNormalizeClampsBatchSize(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, defaultLimit},
		{-5, defaultLimit},
		{24, 24},
		{maxLimit, maxLimit},
		{maxLimit + 1, maxLimit},
		{100000, maxLimit},
	}

	for _, tc := range cases {
		q := Query{Limit: tc.in}
		q.normalize()
		if q.Limit != tc.want {
			t.Errorf("limit %d normalized to %d, want %d", tc.in, q.Limit, tc.want)
		}
	}
}

func TestNormalizeDefaultsSort(t *testing.T) {
	q := Query{SortBy: "nonsense", Order: "sideways"}
	q.normalize()

	if q.SortBy != "createdAt" {
		t.Errorf("sortBy = %q, want createdAt", q.SortBy)
	}
	if q.Order != "desc" {
		t.Errorf("order = %q, want desc", q.Order)
	}
}

// The status test and a free-text search are both $or clauses. A bson.M holds
// one "$or" key, so a keyset seek has to join them under $and rather than
// overwrite one of them.
func TestFilterCombinesEveryOrClause(t *testing.T) {
	s := &Service{}
	q := Query{MainCategory: "Men", Search: "fastrack"}
	q.normalize()

	f := s.filter(q)
	and(f, keysetFilter("created_at", true, Cursor{Value: time.Now().UTC(), ID: primitive.NewObjectID()}))

	if _, ok := f["$or"]; !ok {
		t.Error("status $or was dropped")
	}
	clauses, ok := f["$and"].([]bson.M)
	if !ok || len(clauses) != 2 {
		t.Fatalf("$and = %#v, want the search clause and the keyset clause", f["$and"])
	}
	for i, clause := range clauses {
		if _, ok := clause["$or"]; !ok {
			t.Errorf("$and[%d] = %#v, want an $or clause", i, clause)
		}
	}
}

func TestSortFieldMapping(t *testing.T) {
	for sortBy, want := range map[string]string{
		"createdAt": "created_at",
		"price":     "price",
		"name":      "name",
	} {
		if got := sortFieldFor(sortBy); got != want {
			t.Errorf("sortFieldFor(%q) = %q, want %q", sortBy, got, want)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func f(v float64) *float64 { return &v }

// mutate encodes a valid cursor, alters one field of its payload, and
// re-encodes it, so a test can name exactly what it corrupted.
func mutate(t *testing.T, q Query, change func(*cursorPayload)) string {
	t.Helper()

	raw, err := encodeCursor(q, time.Now().UTC(), primitive.NewObjectID())
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var payload cursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	change(&payload)

	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(out)
}
