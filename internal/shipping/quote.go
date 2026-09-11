package shipping

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// QuoteTTL is how long a rate quote stays valid. Long enough for a customer to
// finish checkout, short enough that a stale carrier price is not honoured
// days later.
const QuoteTTL = 30 * time.Minute

// quoteClaims is the signed content of a rate quote.
//
// The signature covers the shipping charge, the courier and the parameters the
// quote was computed for. That is what makes the charge server-authoritative:
// the browser is handed an opaque token it cannot alter, and at order time we
// verify both the signature and that the token was issued for the address and
// parcel we are actually about to ship.
type quoteClaims struct {
	Provider  string  `json:"p"`
	CourierID string  `json:"c"`
	Courier   string  `json:"n"`
	Charge    float64 `json:"r"`
	Days      int     `json:"d,omitempty"`
	ETD       string  `json:"e,omitempty"`
	COD       bool    `json:"cod"`
	// These bind the quote to the exact circumstances it was issued for.
	//
	// UID and Cart are empty for a quote issued to an anonymous caller (the
	// public serviceability probe). Because Verify compares them, such a quote
	// can never satisfy a checkout, which always presents a user and a cart --
	// so a public quote is useless for pricing an order, and one customer's
	// quote is useless to another.
	UID      string `json:"u,omitempty"`
	Cart     string `json:"h,omitempty"`
	Pin      string `json:"pin"`
	Wt       int    `json:"wt"`
	CODAsked bool   `json:"ca"`
	Exp      int64  `json:"x"`
}

// QuoteBinding is everything a quote is tied to.
//
// A quote is only honoured when every field still matches at order time. That
// is what makes the shipping charge unforgeable: the browser holds an opaque
// token it cannot alter, and altering the circumstances invalidates it.
type QuoteBinding struct {
	// UserID scopes the quote to one customer. Empty for anonymous callers.
	UserID string
	// CartHash scopes it to one cart composition, so adding items forces a
	// re-quote rather than shipping a bigger parcel at the old price.
	CartHash string
	// Pincode, WeightGrams and COD are the rate request parameters.
	Pincode     string
	WeightGrams float64
	COD         bool
}

// Quoter signs and verifies rate quotes with a server-only secret.
type Quoter struct {
	secret []byte
}

// NewQuoter builds a Quoter. The secret must never leave the server; the
// application's JWT secret is reused so there is one less key to rotate.
func NewQuoter(secret string) *Quoter {
	return &Quoter{secret: []byte(secret)}
}

// weightBucket rounds a weight to whole grams so floating-point noise in the
// round trip cannot invalidate an otherwise-valid quote.
func weightBucket(grams float64) int {
	return int(math.Round(grams))
}

// Sign returns an opaque token for one rate option, bound to the
// circumstances it was issued in.
func (q *Quoter) Sign(opt RateOption, binding QuoteBinding, now time.Time) (string, error) {
	claims := quoteClaims{
		Provider:  opt.Provider,
		CourierID: opt.ProviderCourierID,
		Courier:   opt.CourierName,
		Charge:    opt.Charge,
		Days:      opt.EstimatedDeliveryDays,
		ETD:       opt.ETD,
		COD:       opt.CODAvailable,
		UID:       strings.TrimSpace(binding.UserID),
		Cart:      strings.TrimSpace(binding.CartHash),
		Pin:       strings.TrimSpace(binding.Pincode),
		Wt:        weightBucket(binding.WeightGrams),
		CODAsked:  binding.COD,
		Exp:       now.Add(QuoteTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Wrap(CodeRateUnavailable, err, "could not encode quote claims")
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + q.mac(body), nil
}

func (q *Quoter) mac(body string) string {
	m := hmac.New(sha256.New, q.secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifiedQuote is a quote whose signature, expiry and binding have all been
// checked. Only these values may be used to price an order.
type VerifiedQuote struct {
	Provider              string
	CourierID             string
	CourierName           string
	Charge                float64
	EstimatedDeliveryDays int
	ETD                   string
	CODAvailable          bool
	DeliveryPincode       string
}

// Verify checks a quote token against the binding it must still satisfy.
//
// Every mismatch is a rejection. A token that verifies cryptographically but
// was issued for a different address, cart, customer or payment mode is not
// honoured: otherwise a customer could quote a cheap metro rate and ship to a
// remote pincode at that price, quote one watch and ship ten, or replay
// someone else's quote.
func (q *Quoter) Verify(token string, binding QuoteBinding, now time.Time) (*VerifiedQuote, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return nil, Errf(CodeInvalidRequest, "malformed shipping quote token")
	}
	if !hmac.Equal([]byte(sig), []byte(q.mac(body))) {
		return nil, Errf(CodeInvalidRequest, "shipping quote signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, Wrap(CodeInvalidRequest, err, "shipping quote payload is not decodable")
	}
	var claims quoteClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, Wrap(CodeInvalidRequest, err, "shipping quote payload is not readable")
	}
	if now.Unix() > claims.Exp {
		return nil, Errf(CodeRateUnavailable, "shipping quote expired at %d", claims.Exp)
	}
	if claims.UID != strings.TrimSpace(binding.UserID) {
		return nil, Errf(CodeInvalidRequest,
			"shipping quote was issued to a different customer")
	}
	if claims.Cart != strings.TrimSpace(binding.CartHash) {
		return nil, Errf(CodeInvalidRequest,
			"the bag changed after this delivery option was quoted")
	}
	if claims.Pin != strings.TrimSpace(binding.Pincode) {
		return nil, Errf(CodeInvalidRequest,
			"shipping quote was issued for a different destination pincode")
	}
	if claims.CODAsked != binding.COD {
		return nil, Errf(CodeInvalidRequest,
			"shipping quote was issued for a different payment mode")
	}
	if got := weightBucket(binding.WeightGrams); claims.Wt != got {
		return nil, Errf(CodeInvalidRequest,
			"shipping quote was issued for a different parcel weight (%d vs %d grams)", claims.Wt, got)
	}
	return &VerifiedQuote{
		Provider:              claims.Provider,
		CourierID:             claims.CourierID,
		CourierName:           claims.Courier,
		Charge:                claims.Charge,
		EstimatedDeliveryDays: claims.Days,
		ETD:                   claims.ETD,
		CODAvailable:          claims.COD,
		DeliveryPincode:       claims.Pin,
	}, nil
}

// CartLine is one line of a cart, for fingerprinting.
type CartLine struct {
	ProductID string
	Quantity  int
}

// CartFingerprint hashes a cart's composition.
//
// Composition only -- product ids and quantities -- deliberately not prices.
// Shipping depends on what is in the parcel, not on what it costs, and binding
// to price would invalidate every quote on an unrelated catalogue edit. Price
// changes are already caught by checkout re-pricing the order from the
// catalogue and by the payment-amount verification.
//
// Order-independent: the same cart read back in a different order produces the
// same fingerprint.
func CartFingerprint(lines []CartLine) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		if line.Quantity <= 0 || strings.TrimSpace(line.ProductID) == "" {
			continue
		}
		parts = append(parts, line.ProductID+":"+strconv.Itoa(line.Quantity))
	}
	sort.Strings(parts)

	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	// 16 hex characters is ample collision resistance for a binding that also
	// carries a user, a pincode and a 30-minute expiry, and keeps the token
	// short.
	return hex.EncodeToString(h[:8])
}

// OptionID builds the stable public id for a rate option. It is derived from
// the provider and courier so the same courier keeps the same id across
// quotes, which lets the frontend preserve a selection across a re-quote.
func OptionID(provider, courierID string) string {
	if courierID == "" {
		return provider
	}
	return provider + ":" + courierID
}

// ParseCourierID is the inverse of OptionID for the courier half.
func ParseCourierID(optionID string) string {
	_, courier, ok := strings.Cut(optionID, ":")
	if !ok {
		return ""
	}
	return courier
}
