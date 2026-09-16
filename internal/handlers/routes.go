package handlers

import (
	"context"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/mediaindex"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/whatsapp"
	shippingsetup "github.com/shivam-mishra-20/mak-watches-be/internal/shipping/setup"
)

// routeDeps carries everything the domain registrars need.
//
// Handlers and the Firebase provider are constructed once here and shared, so
// no registrar builds its own client. Adding a route means adding it to the
// matching registrar rather than growing one monolithic function.
type routeDeps struct {
	app *fiber.App
	db  *database.DBClient
	cfg *config.Config

	// firebase is the single shared Storage provider for the process. It is
	// lazily initialized, so the API still boots without credentials.
	firebase *firebase.Provider

	// media is the cached bucket inventory used to drop image references whose
	// object no longer exists. Built on the same provider -- no second Firebase
	// initialization -- and fails open.
	media *mediaindex.Index

	// admin is behind JWT auth *and* the admin role check, in that order.
	// Safe to build up front: its "/admin" prefix scopes the middleware to
	// admin routes only.
	admin fiber.Router

	auth        *AuthHandler
	product     *ProductHandler
	cart        *CartHandler
	order       *OrderHandler
	payment     *PaymentHandler
	rec         *RecommendationHandler
	userProfile *UserProfileHandler
	wishlist    *WishlistHandler
	addressBook *AddressBookHandler
	adminAcct   *AdminAccountHandler
	category    *CategoryHandler
	homeContent *HomeContentHandler
	review      *ReviewHandler
	shipping    *ShippingHandler
	shippingV1  *ShippingV1Handler
	// shipHooks serves the authenticated carrier callbacks for every provider.
	shipHooks   *ShippingWebhookHandler
	account     *AccountHandler
	upload      *UploadHandler
	settings    *SettingsHandler
	catalogV1   *CatalogV1Handler
	storefront  *StorefrontHandler
	subscriber  *SubscriberHandler
	cartTracker *CartTrackerHandler
	analytics   *AnalyticsHandler
	coupon      *CouponHandler
	whatsappSettings *WhatsAppSettingsHandler
}

// SetupRoutes configures all application routes.
//
// Every route that existed before the Phase 1 refactor is still registered,
// on the same path and with the same middleware; the registrars below only
// group them by domain. The /api/v1 surface is added alongside, never in place
// of, the existing flat routes.
func SetupRoutes(app *fiber.App, db *database.DBClient, cfg *config.Config) {
	app.Use(logger.New())
	app.Use(recover.New())

	fb := firebase.NewProvider(cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
	media := mediaindex.New(fb, mediaindex.DefaultTTL)

	// One shipping service for the process, with both carriers registered.
	// Every shipping caller shares it, so there is a single Shiprocket token
	// cache and a single Delhivery client rather than one per handler.
	shippingSvc := shippingsetup.Build(db.MongoDB, cfg, nil)
	shippingsetup.EnsureIndexes(shippingSvc)

	// Payment uniqueness guard. Best-effort and non-fatal, like the shipping
	// indexes: the API must still boot if Mongo is briefly unavailable, and
	// checkout's own pre-insert check still applies.
	func() {
		idxCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := db.EnsureOrderIndexes(idxCtx); err != nil {
			log.Printf("[ORDERS] WARNING: could not create the razorpay payment uniqueness index: %v", err)
			log.Printf("[ORDERS] If this reports a duplicate key, existing orders already share a " +
				"razorpay_payment_id and must be reconciled before the index can be built.")
		}
	}()

	d := &routeDeps{
		app:      app,
		db:       db,
		cfg:      cfg,
		firebase: fb,
		media:    media,

		auth:        NewAuthHandler(db, cfg),
		product:     NewProductHandler(db, cfg, fb, media),
		cart:        NewCartHandler(db, cfg),
		order:       NewOrderHandler(db, cfg, shippingSvc),
		payment:     NewPaymentHandler(db, cfg, shippingSvc),
		rec:         NewRecommendationHandler(db, cfg),
		userProfile: NewUserProfileHandler(db, cfg),
		wishlist:    NewWishlistHandler(db, cfg),
		addressBook: NewAddressBookHandler(db, cfg),
		adminAcct:   &AdminAccountHandler{DB: db},
		category:    NewCategoryHandler(db, cfg),
		homeContent: NewHomeContentHandler(db, cfg),
		review:      NewReviewHandler(db, cfg),
		shipping:    NewShippingHandler(db, cfg, shippingSvc),
		shippingV1:  NewShippingV1Handler(db, cfg, shippingSvc),
		shipHooks:   NewShippingWebhookHandler(shippingSvc),
		account:     NewAccountHandler(db, cfg),
		upload:      NewUploadHandler(cfg, fb, media),
		settings:    NewSettingsHandler(db.MongoDB, fb),
		catalogV1:   NewCatalogV1Handler(db, cfg, media),
		storefront:  NewStorefrontHandler(db, cfg),
		analytics:   NewAnalyticsHandler(db, cfg),
		coupon:      NewCouponHandler(db, cfg),
	}

	wa := whatsapp.NewClient(cfg)
	d.subscriber = NewSubscriberHandler(db, cfg, wa)
	d.cartTracker = NewCartTrackerHandler(db, cfg, wa)
	d.whatsappSettings = NewWhatsAppSettingsHandler(db, cfg, wa)

	// Start background worker for abandoned cart recovery
	d.cartTracker.StartAbandonedCartWorker(context.Background(), 10*time.Minute)

	d.admin = app.Group("/admin", middleware.Auth(cfg.JWTSecret), middleware.Role("admin"))

	registerSystemRoutes(d)
	registerAuthRoutes(d)
	registerCatalogRoutes(d)
	registerMediaRoutes(d)
	registerReviewRoutes(d)
	registerCartRoutes(d)
	registerWishlistRoutes(d)
	registerOrderRoutes(d)
	registerPaymentRoutes(d)
	registerShippingRoutes(d)
	registerAccountRoutes(d)
	registerRecommendationRoutes(d)
	registerWebhookRoutes(d)
	registerAdminRoutes(d)
	registerMarketingRoutes(d)
	registerCouponRoutes(d)

	// New surface, additive: everything above keeps working unchanged.
	registerV1Routes(d)
}

func registerMarketingRoutes(d *routeDeps) {
	d.app.Post("/subscribers/whatsapp", d.subscriber.SubscribeWhatsApp)
	d.app.Post("/subscribers", d.subscriber.SubscribeWhatsApp)
	d.app.Post("/cart/track", d.cartTracker.TrackCart)
}

// protectedGroup returns a route group behind JWT authentication.
//
// Auth is attached per group at the point of use, never as a root-level group.
// Fiber's Group(prefix, handlers...) registers those handlers as Use middleware
// on that prefix, and a "" prefix matches every path -- so a root-level
// `app.Group("", middleware.Auth(...))` silently puts every route registered
// after it behind authentication, including /health and the whole public
// catalog. Scoping each group to its own prefix makes the wiring independent of
// registration order. See TestPublicRoutesAreNotAuthenticated.
func (d *routeDeps) protectedGroup(prefix string) fiber.Router {
	return d.app.Group(prefix, middleware.Auth(d.cfg.JWTSecret))
}

// protectedRoute wraps a single root-level handler in JWT authentication, for
// endpoints that do not belong to a group (for example /me and /checkout).
func (d *routeDeps) protectedRoute(handler fiber.Handler) []fiber.Handler {
	return []fiber.Handler{middleware.Auth(d.cfg.JWTSecret), handler}
}

// registerSystemRoutes wires health and welcome probes.
func registerSystemRoutes(d *routeDeps) {
	d.app.Get("/health", HealthHandler)
	d.app.Get("/welcome", WelcomeHandler)
}

// registerAuthRoutes wires registration, login and the Google OAuth dance.
func registerAuthRoutes(d *routeDeps) {
	auth := d.app.Group("/auth")
	auth.Post("/register", rateLimit(5, time.Minute), d.auth.Register)
	auth.Post("/login", rateLimit(10, time.Minute), d.auth.Login)
	auth.Get("/google", d.auth.GoogleLogin)
	auth.Get("/google/callback", d.auth.GoogleCallback)
	auth.Post("/exchange", d.auth.ExchangeOAuthCode)

	d.app.Get("/me", d.protectedRoute(d.auth.Me)...)
}

// HealthHandler handles the health check endpoint
func HealthHandler(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Server is healthy! Welcome to Makwatches API",
	})
}

// WelcomeHandler handles the welcome endpoint
func WelcomeHandler(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"success": true,
		"message": "Welcome to Makwatches API's",
	})
}
