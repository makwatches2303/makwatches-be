package shipping

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is the stable, customer-safe classification of a shipping failure.
//
// Carrier APIs answer with their own vocabulary -- Shiprocket returns a bare
// 422 with a field-level message, Delhivery returns a 200 carrying an error
// array. Neither is safe or useful to hand a customer, so every provider maps
// its failures onto these codes and the HTTP layer only ever serializes the
// code plus a sanitized message.
type ErrorCode string

const (
	CodeShippingUnavailable    ErrorCode = "SHIPPING_UNAVAILABLE"
	CodeInvalidPincode         ErrorCode = "INVALID_PINCODE"
	CodeNotServiceable         ErrorCode = "PINCODE_NOT_SERVICEABLE"
	CodeRateUnavailable        ErrorCode = "RATE_UNAVAILABLE"
	CodeProviderAuthFailed     ErrorCode = "PROVIDER_AUTH_FAILED"
	CodeShipmentCreationFailed ErrorCode = "SHIPMENT_CREATION_FAILED"
	CodeAWBAssignmentFailed    ErrorCode = "AWB_ASSIGNMENT_FAILED"
	CodeTrackingUnavailable    ErrorCode = "TRACKING_UNAVAILABLE"
	CodeLabelUnavailable       ErrorCode = "LABEL_UNAVAILABLE"
	CodePickupFailed           ErrorCode = "PICKUP_FAILED"
	CodeRateLimited            ErrorCode = "PROVIDER_RATE_LIMITED"
	CodeInvalidRequest         ErrorCode = "INVALID_REQUEST"
	CodeShipmentNotFound       ErrorCode = "SHIPMENT_NOT_FOUND"
	CodeUnsupported            ErrorCode = "OPERATION_UNSUPPORTED"
	CodeAlreadyExists          ErrorCode = "SHIPMENT_ALREADY_EXISTS"
	// CodeApprovalRequired is returned when something tries to hand an order to
	// a carrier before an admin has reviewed and approved it for dispatch.
	CodeApprovalRequired ErrorCode = "DISPATCH_NOT_APPROVED"
)

// customerMessage is what a customer or admin browser is allowed to read for a
// given code. Provider text never reaches this map.
var customerMessage = map[ErrorCode]string{
	CodeShippingUnavailable:    "Shipping is temporarily unavailable. Please try again shortly.",
	CodeInvalidPincode:         "Please enter a valid 6-digit pincode.",
	CodeNotServiceable:         "We do not deliver to this pincode yet.",
	CodeRateUnavailable:        "No delivery options are available for this address right now.",
	CodeProviderAuthFailed:     "Shipping is temporarily unavailable. Please try again shortly.",
	CodeShipmentCreationFailed: "We could not book the shipment for this order.",
	CodeAWBAssignmentFailed:    "We could not assign a tracking number for this order.",
	CodeTrackingUnavailable:    "Tracking is not available for this shipment right now.",
	CodeLabelUnavailable:       "The shipping label is not available right now.",
	CodePickupFailed:           "The pickup could not be scheduled.",
	CodeRateLimited:            "Shipping is busy right now. Please try again in a moment.",
	CodeInvalidRequest:         "The shipping request was not valid.",
	CodeShipmentNotFound:       "No shipment was found for this order.",
	CodeUnsupported:            "This shipping operation is not supported by the current carrier.",
	CodeAlreadyExists:          "A shipment already exists for this order.",
	CodeApprovalRequired:       "This order has not been approved for dispatch yet.",
}

// Error is a shipping failure split into a public half and a private half.
//
// Message is safe to serialize. Detail holds the provider's own words and the
// request context an operator needs, and is written only to server logs -- a
// provider error body can echo an address, a phone number or a token fragment,
// none of which belongs in an API response.
type Error struct {
	Code           ErrorCode
	Message        string
	Detail         string
	ProviderStatus int
	wrapped        error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// Public returns only the half that may cross the API boundary.
func (e *Error) Public() (ErrorCode, string) { return e.Code, e.Message }

// HTTPStatus maps a shipping error onto the status the MAK API should answer
// with. Provider availability problems are 502/503 rather than 500: the caller
// can retry, and our own service is healthy.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeInvalidPincode, CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeNotServiceable, CodeRateUnavailable:
		return http.StatusNotFound
	case CodeShipmentNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists:
		return http.StatusConflict
	case CodeApprovalRequired:
		// Conflict, not forbidden: the caller is allowed to do this, the order
		// is simply not in a state where it can be done yet.
		return http.StatusConflict
	case CodeUnsupported:
		return http.StatusNotImplemented
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeProviderAuthFailed, CodeShippingUnavailable:
		return http.StatusBadGateway
	case CodeTrackingUnavailable, CodeLabelUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// Errf builds a shipping error with a customer-safe message drawn from the
// code and operator detail formatted from the arguments.
func Errf(code ErrorCode, format string, args ...any) *Error {
	msg, ok := customerMessage[code]
	if !ok {
		msg = customerMessage[CodeShippingUnavailable]
	}
	return &Error{Code: code, Message: msg, Detail: fmt.Sprintf(format, args...)}
}

// Wrap attaches a cause so errors.Is/As still work through the boundary.
func Wrap(code ErrorCode, cause error, format string, args ...any) *Error {
	e := Errf(code, format, args...)
	e.wrapped = cause
	return e
}

// WithStatus records the provider HTTP status that produced this error.
func (e *Error) WithStatus(status int) *Error {
	e.ProviderStatus = status
	return e
}

// CodeOf extracts the classification from any error, defaulting to
// SHIPPING_UNAVAILABLE so an unmapped failure still fails safe.
func CodeOf(err error) ErrorCode {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return CodeShippingUnavailable
}

// AsError returns the shipping error carried by err, synthesizing one when err
// came from somewhere that does not use this type.
func AsError(err error) *Error {
	var se *Error
	if errors.As(err, &se) {
		return se
	}
	return Wrap(CodeShippingUnavailable, err, "unclassified shipping failure: %v", err)
}

// ErrUnsupported marks an operation a particular carrier does not have. It is
// not a failure: Delhivery issues a waybill during manifestation and has no
// separate AWB-assignment call, so the service skips that step rather than
// reporting an error.
var ErrUnsupported = Errf(CodeUnsupported, "operation not supported by this provider")

// ClassifyHTTPStatus maps a provider HTTP status onto the closest code for the
// operation being attempted. failure is the code to use for a 4xx that is the
// provider rejecting our request rather than an availability problem.
func ClassifyHTTPStatus(status int, failure ErrorCode) ErrorCode {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return CodeProviderAuthFailed
	case status == http.StatusNotFound:
		return CodeShipmentNotFound
	case status == http.StatusTooManyRequests:
		return CodeRateLimited
	case status == http.StatusUnprocessableEntity || status == http.StatusBadRequest:
		return failure
	case status >= 500:
		return CodeShippingUnavailable
	default:
		return failure
	}
}
