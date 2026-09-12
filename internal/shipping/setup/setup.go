// Package setup assembles the shipping subsystem from application config.
//
// It lives outside internal/shipping because the adapters import the neutral
// contract; a factory inside that package would create an import cycle.
package setup

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
	delhiveryadapter "github.com/shivam-mishra-20/mak-watches-be/internal/shipping/delhivery"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping/shiprocket"
)

// DelhiveryServiceFromConfig builds the legacy Delhivery client.
//
// Kept as its own constructor because two call sites already build it this way
// and the wiring must stay identical -- historical behaviour depends on it.
func DelhiveryServiceFromConfig(cfg *config.Config) *services.DelhiveryService {
	return services.NewDelhiveryService(services.DelhiveryConfig{
		APIToken:       cfg.DelhiveryAPIToken,
		BaseURL:        cfg.DelhiveryBaseURL,
		PickupLocation: cfg.DelhiveryPickupLocation,
		SellerName:     cfg.DelhiverySellerName,
		SellerPhone:    cfg.DelhiverySellerPhone,
		SellerAddress:  cfg.DelhiverySellerAddress,
		SellerCity:     cfg.DelhiverySellerCity,
		SellerState:    cfg.DelhiverySellerState,
		SellerPincode:  cfg.DelhiverySellerPincode,
		ReturnAddress:  cfg.DelhiveryReturnAddress,
		ReturnCity:     cfg.DelhiveryReturnCity,
		ReturnState:    cfg.DelhiveryReturnState,
		ReturnPincode:  cfg.DelhiveryReturnPincode,
		ReturnPhone:    cfg.DelhiveryReturnPhone,
	})
}

// Build constructs the shipping service with both providers registered.
//
// Both carriers are always registered regardless of which is primary, so an
// order booked with Delhivery stays fully operable -- trackable, cancellable,
// labellable -- after the primary switches to Shiprocket.
func Build(db *mongo.Database, cfg *config.Config, delhiverySvc *services.DelhiveryService) *shipping.Service {
	if delhiverySvc == nil {
		delhiverySvc = DelhiveryServiceFromConfig(cfg)
	}

	shiprocketProvider := shiprocket.New(shiprocket.Config{
		Email:          cfg.ShiprocketEmail,
		Password:       cfg.ShiprocketPassword,
		BaseURL:        cfg.ShiprocketBaseURL,
		PickupLocation: cfg.ShiprocketPickupLocation,
		ChannelID:      cfg.ShiprocketChannelID,
		WebhookSecret:  cfg.ShiprocketWebhookSecret,
		SellerGSTIN:    cfg.SellerGSTIN,
	})

	delhiveryProvider := delhiveryadapter.New(delhiverySvc, delhiveryadapter.Config{
		FlatShippingCharge: cfg.DelhiveryFlatShippingCharge,
		WebhookSecret:      cfg.DelhiveryWebhookSecret,
		PickupLocation:     cfg.DelhiveryPickupLocation,
		SellerGSTIN:        cfg.SellerGSTIN,
	})

	primary := cfg.ShippingProvider
	switch primary {
	case shipping.ProviderShiprocket, shipping.ProviderDelhivery:
	default:
		log.Printf("[SHIPPING] SHIPPING_PROVIDER=%q is not a known provider; using %q",
			primary, shipping.ProviderShiprocket)
		primary = shipping.ProviderShiprocket
	}

	svcCfg := shipping.ServiceConfig{
		Primary:       primary,
		PickupPincode: cfg.DelhiverySellerPincode,
		PickupLocations: map[string]string{
			shipping.ProviderShiprocket: cfg.ShiprocketPickupLocation,
			shipping.ProviderDelhivery:  cfg.DelhiveryPickupLocation,
		},
		Package: shipping.PackageSpec{
			WeightGrams: cfg.PackageDefaultWeightGrams,
			LengthCm:    cfg.PackageDefaultLengthCm,
			BreadthCm:   cfg.PackageDefaultBreadthCm,
			HeightCm:    cfg.PackageDefaultHeightCm,
			Defaults:    true,
		},
		SellerGSTIN: cfg.SellerGSTIN,
	}

	svc := shipping.NewService(db, svcCfg, shipping.NewQuoter(cfg.JWTSecret),
		shiprocketProvider, delhiveryProvider)

	logReadiness(cfg, primary, shiprocketProvider)
	return svc
}

// logReadiness reports configuration state without printing any secret.
//
// Only presence is logged -- never a credential, a token, or any fragment of
// one. The audit found the previous integration logging a masked API token and
// its length; even that is more than an operator needs.
func logReadiness(cfg *config.Config, primary string, sr *shiprocket.Provider) {
	log.Printf("[SHIPPING] primary provider: %s", primary)
	log.Printf("[SHIPPING] shiprocket credentials configured: %t", sr.Configured())
	log.Printf("[SHIPPING] shiprocket pickup location configured: %t", cfg.ShiprocketPickupLocation != "")
	log.Printf("[SHIPPING] shiprocket webhook secret configured: %t", cfg.ShiprocketWebhookSecret != "")
	log.Printf("[SHIPPING] delhivery token configured: %t", cfg.DelhiveryAPIToken != "")
	log.Printf("[SHIPPING] delhivery webhook secret configured: %t", cfg.DelhiveryWebhookSecret != "")
	log.Printf("[SHIPPING] package defaults: %.0fg %.0fx%.0fx%.0fcm",
		cfg.PackageDefaultWeightGrams, cfg.PackageDefaultLengthCm,
		cfg.PackageDefaultBreadthCm, cfg.PackageDefaultHeightCm)

	if primary == shipping.ProviderShiprocket && !sr.Configured() {
		log.Printf("[SHIPPING] WARNING: shiprocket is primary but has no credentials; " +
			"shipment creation will fail until SHIPROCKET_EMAIL and SHIPROCKET_PASSWORD are set")
	}
	if cfg.ShiprocketWebhookSecret == "" {
		log.Printf("[SHIPPING] WARNING: SHIPROCKET_WEBHOOK_SECRET is unset; " +
			"shiprocket callbacks will be rejected")
	}
	if cfg.DelhiveryWebhookSecret == "" {
		log.Printf("[SHIPPING] WARNING: DELHIVERY_WEBHOOK_SECRET is unset; " +
			"delhivery callbacks will be rejected")
	}
	warnWeakSecret("SHIPROCKET_WEBHOOK_SECRET", cfg.ShiprocketWebhookSecret)
	warnWeakSecret("DELHIVERY_WEBHOOK_SECRET", cfg.DelhiveryWebhookSecret)
}

// minWebhookSecretLen is the shortest shared secret worth calling one.
//
// These secrets are the *only* thing standing between an anonymous caller and
// an order's fulfillment state, and the endpoints are public by necessity. A
// short value is brute-forceable in seconds, which makes the constant-time
// comparison protecting it beside the point.
const minWebhookSecretLen = 24

// warnWeakSecret reports a configured-but-too-short secret.
//
// The length is logged, never the value. This warns rather than refuses: an
// operator mid-rollout should not have the API fail to boot, but the state
// must be impossible to miss.
func warnWeakSecret(name, value string) {
	if value == "" {
		return
	}
	if len(value) < minWebhookSecretLen {
		log.Printf("[SHIPPING] WARNING: %s is only %d characters. A webhook secret this short "+
			"is guessable, and it is the only authentication on a public endpoint that can move "+
			"an order to delivered. Use at least %d random characters.",
			name, len(value), minWebhookSecretLen)
	}
}

// EnsureIndexes creates the shipping indexes with a bounded timeout.
//
// Failure is logged and not fatal: the API must still boot when Mongo is
// briefly unavailable, and the index is created on the next start.
func EnsureIndexes(svc *shipping.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := svc.EnsureIndexes(ctx); err != nil {
		log.Printf("[SHIPPING] WARNING: could not create shipment indexes: %v", err)
		return
	}
	log.Printf("[SHIPPING] shipment indexes ready")
}
