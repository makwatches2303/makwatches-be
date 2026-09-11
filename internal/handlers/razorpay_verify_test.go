package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testKeyID  = "rzp_test_key"
	testSecret = "rzp_test_secret"
)

// signFor produces the signature Razorpay's checkout widget would return.
func signFor(orderID, paymentID string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(orderID + "|" + paymentID))
	return hex.EncodeToString(mac.Sum(nil))
}

// mockRazorpay serves one order object.
func mockRazorpay(t *testing.T, orderID string, status string, amountPaise, paidPaise int64) *RazorpayVerifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/"+orderID {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"description":"not found"}}`))
			return
		}
		// Razorpay authenticates with HTTP basic; a verifier that forgot it
		// would get a 401 here.
		user, pass, ok := r.BasicAuth()
		if !ok || user != testKeyID || pass != testSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"amount":%d,"amount_paid":%d,"amount_due":%d,"currency":"INR","status":%q}`,
			orderID, amountPaise, paidPaise, amountPaise-paidPaise, status)
	}))
	t.Cleanup(srv.Close)

	return &RazorpayVerifier{
		KeyID:   testKeyID,
		Secret:  testSecret,
		BaseURL: srv.URL,
		Client:  srv.Client(),
	}
}

func TestVerifyPaidAcceptsAFullyPaidOrder(t *testing.T) {
	v := mockRazorpay(t, "order_ABC", "paid", 499900, 499900)

	err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
		signFor("order_ABC", "pay_XYZ"), 4999)
	if err != nil {
		t.Fatalf("a fully-paid order must verify: %v", err)
	}
}

// TestVerifyPaidRejectsUnderpayment is the regression test for the audit's
// payment finding.
//
// The old check verified only the HMAC, so a genuine ₹1,000 payment could be
// submitted against an ₹11,000 cart: the signature was valid and the order was
// created as paid. The amount now comes from the gateway, not from the client.
func TestVerifyPaidRejectsUnderpayment(t *testing.T) {
	// The customer really did pay -- but only ₹1,000.
	v := mockRazorpay(t, "order_ABC", "paid", 100000, 100000)

	err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
		signFor("order_ABC", "pay_XYZ"), 11000) // authoritative total ₹11,000
	if !errors.Is(err, ErrPaymentAmountMismatch) {
		t.Fatalf("err = %v, want ErrPaymentAmountMismatch", err)
	}
}

func TestVerifyPaidRejectsUncapturedOrder(t *testing.T) {
	for _, status := range []string{"created", "attempted"} {
		v := mockRazorpay(t, "order_ABC", status, 499900, 0)
		err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
			signFor("order_ABC", "pay_XYZ"), 4999)
		if !errors.Is(err, ErrPaymentNotCaptured) {
			t.Errorf("status %q: err = %v, want ErrPaymentNotCaptured", status, err)
		}
	}
}

func TestVerifyPaidRejectsForgedSignature(t *testing.T) {
	v := mockRazorpay(t, "order_ABC", "paid", 499900, 499900)

	cases := map[string]string{
		"wrong signature":     "deadbeef",
		"signature for other": signFor("order_OTHER", "pay_XYZ"),
		"empty":               "",
	}
	for name, sig := range cases {
		err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ", sig, 4999)
		if !errors.Is(err, ErrPaymentSignatureInvalid) {
			t.Errorf("%s: err = %v, want ErrPaymentSignatureInvalid", name, err)
		}
	}
}

// TestVerifyPaidDoesNotCallGatewayOnBadSignature: the signature is checked
// first, so a forged request costs no gateway round trip.
func TestVerifyPaidDoesNotCallGatewayOnBadSignature(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	v := &RazorpayVerifier{KeyID: testKeyID, Secret: testSecret, BaseURL: srv.URL, Client: srv.Client()}

	_ = v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ", "forged", 4999)
	if calls != 0 {
		t.Fatalf("gateway was called %d time(s) for a forged signature", calls)
	}
}

// TestVerifyPaidRejectsOrderTheGatewayDoesNotKnow: a signature can only be
// produced with the secret, so this should be unreachable -- but it fails
// closed rather than assuming the payment is good.
func TestVerifyPaidRejectsUnknownOrder(t *testing.T) {
	v := mockRazorpay(t, "order_REAL", "paid", 499900, 499900)

	err := v.VerifyPaid(context.Background(), "order_GHOST", "pay_XYZ",
		signFor("order_GHOST", "pay_XYZ"), 4999)
	if !errors.Is(err, ErrPaymentSignatureInvalid) {
		t.Fatalf("err = %v, want ErrPaymentSignatureInvalid for an unknown order", err)
	}
}

func TestVerifyPaidReportsGatewayFailureSeparately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	v := &RazorpayVerifier{KeyID: testKeyID, Secret: testSecret, BaseURL: srv.URL, Client: srv.Client()}

	err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
		signFor("order_ABC", "pay_XYZ"), 4999)
	// "We could not ask" must not be reported as "the payment is invalid":
	// the customer's money may well have been taken.
	if !errors.Is(err, ErrPaymentGatewayUnreach) {
		t.Fatalf("err = %v, want ErrPaymentGatewayUnreach", err)
	}
}

func TestVerifyPaidRequiresCredentials(t *testing.T) {
	v := &RazorpayVerifier{}
	err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ", "sig", 4999)
	if !errors.Is(err, ErrPaymentGatewayUnreach) {
		t.Fatalf("err = %v, want ErrPaymentGatewayUnreach with no credentials", err)
	}
}

// TestVerifyPaidToleratesRounding: a one-rupee allowance matches the tolerance
// checkout already applies to the client-reported total, so paise rounding
// does not reject a legitimate payment.
func TestVerifyPaidToleratesRounding(t *testing.T) {
	v := mockRazorpay(t, "order_ABC", "paid", 499900, 499900)

	if err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
		signFor("order_ABC", "pay_XYZ"), 4999.50); err != nil {
		t.Fatalf("a half-rupee difference must not reject the payment: %v", err)
	}
}

// TestVerifyPaidAllowsOverpayment: rejecting it here would strand a customer
// whose money has already been taken. Refunds are a business decision.
func TestVerifyPaidAllowsOverpayment(t *testing.T) {
	v := mockRazorpay(t, "order_ABC", "paid", 600000, 600000)

	if err := v.VerifyPaid(context.Background(), "order_ABC", "pay_XYZ",
		signFor("order_ABC", "pay_XYZ"), 4999); err != nil {
		t.Fatalf("overpayment must not block the order: %v", err)
	}
}
