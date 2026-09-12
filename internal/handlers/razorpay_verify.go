package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// razorpayAPIBase is the Razorpay REST root. Overridable in tests only.
const razorpayAPIBase = "https://api.razorpay.com/v1"

// Payment verification outcomes. These are deliberately coarse: the customer
// is told the payment could not be confirmed, and the reason goes to the log.
var (
	ErrPaymentSignatureInvalid = errors.New("payment signature invalid")
	ErrPaymentNotCaptured      = errors.New("payment not captured")
	ErrPaymentAmountMismatch   = errors.New("payment amount does not match the order total")
	ErrPaymentGatewayUnreach   = errors.New("payment gateway unreachable")
)

// razorpayOrder is the subset of Razorpay's order object we rely on.
type razorpayOrder struct {
	ID         string `json:"id"`
	Amount     int64  `json:"amount"`
	AmountPaid int64  `json:"amount_paid"`
	AmountDue  int64  `json:"amount_due"`
	Currency   string `json:"currency"`
	// Status is "created", "attempted" or "paid".
	Status string `json:"status"`
}

// RazorpayVerifier confirms that a payment really happened, for the right
// amount, before an order is created against it.
type RazorpayVerifier struct {
	KeyID   string
	Secret  string
	BaseURL string
	Client  *http.Client
}

// NewRazorpayVerifier builds a verifier from credentials.
func NewRazorpayVerifier(keyID, secret string) *RazorpayVerifier {
	return &RazorpayVerifier{
		KeyID:   keyID,
		Secret:  secret,
		BaseURL: razorpayAPIBase,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// VerifyPaid confirms a Razorpay payment against our own authoritative total.
//
// The signature alone is not sufficient, and treating it as sufficient was the
// hole this closes. An HMAC over (order_id|payment_id) proves only that *some*
// genuine Razorpay order was paid -- it says nothing about how much. A
// customer could create a ₹1,000 intent, pay it, then add ₹10,000 of items to
// the cart and submit the same payment triple: the signature verifies, and the
// order is created as paid for ₹11,000.
//
// So the amount is fetched from Razorpay and compared to the total the server
// computed, and the order must actually be captured.
//
// expectedTotalINR is the authoritative total in rupees.
func (v *RazorpayVerifier) VerifyPaid(ctx context.Context, orderID, paymentID, signature string, expectedTotalINR float64) error {
	if v.KeyID == "" || v.Secret == "" {
		return fmt.Errorf("%w: razorpay credentials are not configured", ErrPaymentGatewayUnreach)
	}
	orderID = strings.TrimSpace(orderID)
	paymentID = strings.TrimSpace(paymentID)
	if orderID == "" || paymentID == "" || strings.TrimSpace(signature) == "" {
		return ErrPaymentSignatureInvalid
	}

	// 1. The signature proves the client did not invent the identifiers.
	mac := hmac.New(sha256.New, []byte(v.Secret))
	mac.Write([]byte(orderID + "|" + paymentID))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(signature)) {
		return ErrPaymentSignatureInvalid
	}

	// 2. The gateway is the authority on what was actually paid.
	order, err := v.fetchOrder(ctx, orderID)
	if err != nil {
		return err
	}

	if order.Status != "paid" {
		return fmt.Errorf("%w: razorpay order %s is %q", ErrPaymentNotCaptured, orderID, order.Status)
	}

	expectedPaise := int64(math.Round(expectedTotalINR * 100))
	// A one-rupee tolerance, matching the rounding allowance the checkout
	// already applies to the client-reported total.
	const tolerancePaise = 100
	paid := order.AmountPaid
	if paid == 0 {
		paid = order.Amount
	}
	if paid+tolerancePaise < expectedPaise {
		// Underpayment is the attack; overpayment is not our problem to
		// reject here, and refunds are a business decision.
		return fmt.Errorf("%w: paid %d paise, order total %d paise",
			ErrPaymentAmountMismatch, paid, expectedPaise)
	}
	return nil
}

// fetchOrder reads a Razorpay order.
func (v *RazorpayVerifier) fetchOrder(ctx context.Context, orderID string) (*razorpayOrder, error) {
	endpoint := strings.TrimRight(v.BaseURL, "/") + "/orders/" + url.PathEscape(orderID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentGatewayUnreach, err)
	}
	req.SetBasicAuth(v.KeyID, v.Secret)
	req.Header.Set("Accept", "application/json")

	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentGatewayUnreach, err)
	}
	defer resp.Body.Close()

	// Bounded read: an error page from a proxy must not be unbounded.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentGatewayUnreach, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// A signature that verifies for an order Razorpay does not have
		// should be impossible; fail closed rather than guess.
		return nil, fmt.Errorf("%w: razorpay does not know order %s", ErrPaymentSignatureInvalid, orderID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: razorpay returned %d", ErrPaymentGatewayUnreach, resp.StatusCode)
	}

	var order razorpayOrder
	if err := json.Unmarshal(body, &order); err != nil {
		return nil, fmt.Errorf("%w: unreadable order response", ErrPaymentGatewayUnreach)
	}
	return &order, nil
}
