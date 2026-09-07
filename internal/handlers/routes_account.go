package handlers

// registerAccountRoutes wires the consolidated customer account surface:
// overview, reviews, wishlist, orders, profile, preferences and addresses.
func registerAccountRoutes(d *routeDeps) {
	account := d.protectedGroup("/account")
	account.Get("/overview", d.account.GetAccountOverview)
	account.Get("/reviews", d.account.GetAccountReviews)
	account.Delete("/reviews/:id", d.account.DeleteAccountReview)
	account.Post("/reviews", d.review.CreateReview)
	account.Get("/wishlist", d.account.GetAccountWishlist)
	account.Delete("/wishlist/:id", d.account.RemoveAccountWishlistItem)
	account.Get("/orders", d.account.GetAccountOrders)
	account.Get("/orders/:orderID", d.account.GetAccountOrder)
	account.Post("/orders/:orderID/cancel", d.order.CancelOrder)

	profiles := d.protectedGroup("/profiles")
	profiles.Get("/", d.userProfile.GetProfile)
	profiles.Put("/", d.userProfile.UpdateProfile)

	preferences := d.protectedGroup("/preferences")
	preferences.Put("/", d.userProfile.UpdatePreferences)

	addresses := d.protectedGroup("/addresses")
	addresses.Get("/", d.addressBook.GetAddresses)
	addresses.Get("/:id", d.addressBook.GetAddress)
	addresses.Post("/", d.addressBook.CreateAddress)
	addresses.Put("/:id", d.addressBook.UpdateAddress)
	addresses.Delete("/:id", d.addressBook.DeleteAddress)
	addresses.Put("/:id/default", d.addressBook.SetDefaultAddress)
}
