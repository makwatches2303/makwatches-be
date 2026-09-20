package catalog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Keyset pagination for catalog listings.
//
// Progressive loading cannot use skip/limit. Skip makes the server walk and
// discard every preceding document, so the cost of a batch grows with how far
// the shopper has scrolled, and a catalogue that changes underneath them --
// one product added while they browse -- shifts every subsequent page by one,
// duplicating a product at each boundary.
//
// A keyset cursor names the last document instead of a count: "everything
// after this sort value, this id". The server seeks straight to that point in
// the index, so every batch costs the same, and an insert elsewhere in the
// catalogue cannot shift the window.
//
// The tie-breaker is what makes it correct. Sorting by created_at alone leaves
// documents sharing a timestamp in an arbitrary order, and two batches may
// disagree about that order -- which is how a product appears twice or not at
// all. Sorting by (field, _id) is a total order over the collection, so the
// "after this point" predicate is unambiguous.

// ErrInvalidCursor is returned when a cursor is malformed, corrupt, or belongs
// to a different query than the one being served.
var ErrInvalidCursor = errors.New("catalog: invalid cursor")

// maxCursorLength bounds how much work a bad cursor can cause before it is
// rejected. A real cursor is well under 200 bytes.
const maxCursorLength = 512

// cursorPayload is what a cursor carries across the wire.
//
// Field names are single letters because this is encoded into a URL on every
// batch request, not because it is meant to be opaque -- a cursor is position,
// not a secret, and anyone may decode one.
type cursorPayload struct {
	// F fingerprints the query this cursor was issued for. See fingerprint.
	F string `json:"f"`
	// S is the sort the cursor was issued under, as "field:dir".
	S string `json:"s"`
	// V is the sort value of the last document in the previous batch. Typed
	// per sort field: RFC3339 nanoseconds for createdAt, a number for price, a
	// string for name.
	V json.RawMessage `json:"v"`
	// I is the _id of the last document in the previous batch, as hex.
	I string `json:"i"`
}

// Cursor is a decoded, validated position in a listing.
type Cursor struct {
	// Value is the sort field's value on the last document of the previous
	// batch, already converted to the type the field holds in Mongo.
	Value any
	// ID is that document's _id.
	ID primitive.ObjectID
}

// fingerprint hashes everything about a query except its position within the
// results.
//
// This is what stops a cursor from one listing being replayed against another.
// A cursor issued for Men sorted newest-first names a created_at and an _id;
// nothing about those values would look wrong if they arrived on a Women
// request, and the shopper would silently get a window of the wrong catalogue.
// Page and Limit are excluded deliberately: they say where in the results the
// caller is and how many they want, not which results those are.
func fingerprint(q Query) string {
	// Written field by field rather than reflecting over the struct, so adding
	// a filter to Query is a deliberate decision here too. A filter missing
	// from this list would be a filter a cursor could be carried across.
	parts := []string{
		q.Category,
		q.MainCategory,
		q.Subcategory,
		q.Collection,
		q.VariantGroupID,
		q.Gender,
		strings.TrimSpace(q.Search),
		strings.Join(q.Brands, ","),
		q.DialColor,
		q.DialShape,
		q.DialType,
		q.StrapColor,
		q.StrapMaterial,
		q.Style,
		q.DialThickness,
		floatPtrKey(q.MinPrice),
		floatPtrKey(q.MaxPrice),
		strconv.FormatBool(q.InStock),
		strconv.FormatBool(q.Featured),
		strconv.FormatBool(q.NewArrival),
		strconv.FormatBool(q.Bestseller),
		q.SortBy,
		q.Order,
	}

	// The separator is a unit separator rather than a comma so that a filter
	// value containing the separator cannot forge a different query's
	// fingerprint.
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	// Half the digest. This guards against accidental reuse, not against an
	// attacker, and a cursor rides in a URL on every batch.
	return hex.EncodeToString(sum[:8])
}

func floatPtrKey(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// sortKey names the sort a cursor was issued under, so a cursor cannot survive
// a change of sort even if every filter stayed the same.
func sortKey(q Query) string {
	return q.SortBy + ":" + q.Order
}

// encodeCursor builds the cursor pointing just past the given document.
//
// value must be the document's value for the query's sort field.
func encodeCursor(q Query, value any, id primitive.ObjectID) (string, error) {
	raw, err := json.Marshal(normalizeCursorValue(value))
	if err != nil {
		return "", fmt.Errorf("catalog: encode cursor value: %w", err)
	}

	payload, err := json.Marshal(cursorPayload{
		F: fingerprint(q),
		S: sortKey(q),
		V: raw,
		I: id.Hex(),
	})
	if err != nil {
		return "", fmt.Errorf("catalog: encode cursor: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// normalizeCursorValue renders a sort value in a form that survives JSON.
//
// Times go out as RFC3339 with nanoseconds so the round trip is lossless: a
// second-resolution timestamp would put every document sharing that second on
// the wrong side of the seek.
func normalizeCursorValue(value any) any {
	switch v := value.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case primitive.DateTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return value
	}
}

// decodeCursor validates a cursor against the query it arrived with.
//
// Every failure is ErrInvalidCursor: a caller cannot tell a truncated cursor
// from one belonging to another query, and does not need to. All of them mean
// the same thing -- start this listing again from the top.
func decodeCursor(raw string, q Query) (*Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxCursorLength {
		return nil, ErrInvalidCursor
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}

	var payload cursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, ErrInvalidCursor
	}

	if payload.F != fingerprint(q) || payload.S != sortKey(q) {
		return nil, ErrInvalidCursor
	}

	id, err := primitive.ObjectIDFromHex(payload.I)
	if err != nil {
		return nil, ErrInvalidCursor
	}

	value, err := decodeCursorValue(q.SortBy, payload.V)
	if err != nil {
		return nil, ErrInvalidCursor
	}

	return &Cursor{Value: value, ID: id}, nil
}

// decodeCursorValue converts the encoded sort value back to the type the
// field holds in Mongo. A value of the wrong shape for the sort field is a
// corrupt cursor, not a value to coerce.
func decodeCursorValue(sortBy string, raw json.RawMessage) (any, error) {
	switch sortBy {
	case "price":
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, ErrInvalidCursor
		}
		return v, nil
	case "name":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, ErrInvalidCursor
		}
		return v, nil
	case "createdAt":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, ErrInvalidCursor
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		return t, nil
	default:
		return nil, ErrInvalidCursor
	}
}

// keysetFilter is the "everything strictly after the cursor" predicate.
//
// For a descending sort it reads: the sort value is below the cursor's, or it
// ties and the _id is below. Ascending is the mirror. Both halves are needed --
// dropping the tie branch skips every document sharing the boundary value, and
// dropping the strict branch returns only the ties.
func keysetFilter(field string, descending bool, c Cursor) bson.M {
	op := "$gt"
	if descending {
		op = "$lt"
	}

	return bson.M{"$or": []bson.M{
		{field: bson.M{op: c.Value}},
		{field: c.Value, "_id": bson.M{op: c.ID}},
	}}
}
