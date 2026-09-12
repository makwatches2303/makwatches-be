package handlers

import (
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
)

// registerCartRoutes wires the authenticated cart.
//
// GET /cart/:userID keeps its path for backward compatibility, but the handler
// no longer trusts that id: it must match the authenticated subject, or belong
// to an admin. See GetCart and cart_handler_test.go.
func registerCartRoutes(d *routeDeps) {
	cart := d.protectedGroup("/cart")
	cart.Post("/", d.cart.AddToCart)
	// Idempotent whole-cart replacement. See ReplaceCart: the storefront's
	// client-side bag is synced here before checkout, and POST would double it.
	cart.Put("/", d.cart.ReplaceCart)
	// Both forms resolve to the caller's own cart: GetCart takes its identity
	// from the token and treats any :userID as a restatement to be verified.
	// The collection-level route was missing, so `GET /cart` -- the form the
	// storefront client actually calls -- answered 405.
	cart.Get("/", d.cart.GetCart)
	cart.Get("/:userID", d.cart.GetCart)
	cart.Delete("/:userID/:productID", d.cart.RemoveFromCart)
}

// registerWishlistRoutes wires the authenticated wishlist.
func registerWishlistRoutes(d *routeDeps) {
	wishlist := d.protectedGroup("/wishlist")
	wishlist.Get("/", d.wishlist.GetWishlist)
	wishlist.Post("/", d.wishlist.AddToWishlist)
	wishlist.Delete("/:id", d.wishlist.RemoveFromWishlist)
	wishlist.Delete("/", d.wishlist.ClearWishlist)
}

// registerOrderRoutes wires order reads, cancellation and checkout, plus the
// admin-only listing and status transitions.
func registerOrderRoutes(d *routeDeps) {
	orders := d.protectedGroup("/orders")
	orders.Get("/user/:userID", d.order.GetOrders)
	orders.Get("/:orderID", d.order.GetOrder)
	orders.Post("/:orderID/cancel", d.order.CancelOrder)

	// Admin-only order operations.
	orders.Get("/", middleware.Role("admin"), d.order.GetAllOrders)
	orders.Patch("/:orderID/status", middleware.Role("admin"), d.order.UpdateOrderStatus)

	d.app.Post("/checkout", d.protectedRoute(d.order.Checkout)...)
}

// registerPaymentRoutes wires authenticated payment initiation. The Razorpay
// webhook is public and lives in registerWebhookRoutes.
func registerPaymentRoutes(d *routeDeps) {
	payments := d.protectedGroup("/payments")
	payments.Post("/razorpay/order", rateLimit(20, time.Minute), d.payment.CreateRazorpayOrder)
}

// registerShippingRoutes wires public pincode/tracking lookups and the
// authenticated per-order tracking read.
func registerShippingRoutes(d *routeDeps) {
	d.app.Get("/shipping/check-pincode/:pincode", d.shipping.CheckPincode)
	d.app.Get("/shipping/check-pincode", d.shipping.CheckPincode)
	d.app.Post("/checkout/shipping-options", d.shipping.GetCheckoutShippingOptions)
	d.app.Get("/checkout/shipping-options", d.shipping.GetCheckoutShippingOptions)
	d.app.Get("/shipping/track/:waybill", d.shipping.TrackByWaybill)

	shipping := d.protectedGroup("/shipping")
	shipping.Get("/track/order/:orderID", d.shipping.TrackShipment)
}

// registerWebhookRoutes wires the public endpoints called by payment and
// logistics providers. These are unauthenticated by design; each handler is
// responsible for verifying its own provider signature.
func registerWebhookRoutes(d *routeDeps) {
	d.app.Post("/webhooks/razorpay", d.payment.RazorpayWebhook)
	d.app.Post("/webhooks/delhivery", d.shipping.DelhiveryWebhook)
}

// registerRecommendationRoutes wires the authenticated recommendation surface.
func registerRecommendationRoutes(d *routeDeps) {
	recommendations := d.protectedGroup("/recommendations")
	recommendations.Get("/", d.rec.GetRecommendations)
	recommendations.Post("/feedback", d.rec.SubmitFeedback)
}

// registerCouponRoutes wires public coupon validation endpoints
func registerCouponRoutes(d *routeDeps) {
	d.app.Post("/coupons/validate", d.coupon.ValidateCoupon)
}
