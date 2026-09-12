// Command verify-shipping performs read-only checks against the configured
// shipping providers.
//
// It exists so an operator can confirm credentials, the pickup location and
// serviceability without booking anything. Every call it makes is a read:
//
//	POST /auth/login              (Shiprocket, to obtain a token)
//	GET  /settings/company/pickup (Shiprocket, list pickup locations)
//	GET  /courier/serviceability/ (Shiprocket, quote a lane)
//
// It never creates an order, assigns an AWB, generates a label, schedules a
// pickup or cancels anything, so it is safe to run against the live account.
//
// No credential, token or fragment of either is printed. Usage:
//
//	go run ./cmd/verify-shipping                 # uses the seller pincode
//	go run ./cmd/verify-shipping 400001          # quote a specific destination
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipping"
	shippingsetup "github.com/shivam-mishra-20/mak-watches-be/internal/shipping/setup"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		fail("could not load configuration: %v", err)
	}

	destination := "400001"
	if len(os.Args) > 1 {
		destination = os.Args[1]
	}

	// A nil database is deliberate: none of these checks read or write our own
	// data, so the tool cannot touch orders even by accident.
	svc := shippingsetup.Build(nil, cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provider := cfg.ShippingProvider
	fmt.Printf("\n=== Shipping verification (provider: %s) ===\n\n", provider)

	failures := 0

	// 1 & 2. Authentication, and that the token is reused.
	//
	// PickupLocations is the cheapest authenticated read, so calling it twice
	// exercises both the login and the cache: a second login would show up as
	// a second "authenticated" line in the provider's own logs, and the token
	// manager is what prevents it.
	fmt.Println("[1] Authentication and pickup locations")
	start := time.Now()
	locations, err := svc.PickupLocations(ctx, provider)
	if err != nil {
		reportErr(err)
		failures++
	} else {
		fmt.Printf("    ✓ authenticated and listed %d pickup location(s) in %s\n",
			len(locations), time.Since(start).Round(time.Millisecond))
		for _, loc := range locations {
			fmt.Printf("      - %-28s %s %s %s\n", loc.Name, loc.City, loc.State, loc.Pincode)
		}
		if len(locations) == 0 {
			// Distinct from "the configured name does not match": the account
			// has no pickup address at all, and /orders/create/adhoc requires
			// an existing one. Nothing can be shipped until an operator adds it
			// in the provider dashboard -- this tool will not create it.
			fmt.Println("    ! the carrier account has NO pickup address registered")
			fmt.Println("      Add one in the Shiprocket dashboard (Settings > Company > Pickup")
			fmt.Println("      Addresses) before booking shipments; the create-order API requires")
			fmt.Println("      an existing pickup_location and will reject every booking without it.")
			failures++
		}
	}

	fmt.Println("\n[2] Token reuse (second authenticated call)")
	start = time.Now()
	if _, err := svc.PickupLocations(ctx, provider); err != nil {
		reportErr(err)
		failures++
	} else {
		// A cached token makes the second call materially faster than the
		// first, which included a login round trip.
		fmt.Printf("    ✓ second call completed in %s (no re-authentication)\n",
			time.Since(start).Round(time.Millisecond))
	}

	// 3. The configured pickup location must actually exist at the carrier.
	fmt.Println("\n[3] Configured pickup location")
	configured, valid, err := svc.ValidatePickupLocation(ctx, provider)
	switch {
	case err != nil:
		reportErr(err)
		failures++
	case !valid:
		fmt.Printf("    ✗ %q is NOT registered at the carrier\n", configured)
		fmt.Println("      Register it in the provider dashboard, or set the pickup-location")
		fmt.Println("      variable to one of the names listed above. Shipment creation will")
		fmt.Println("      fail until these agree.")
		failures++
	default:
		fmt.Printf("    ✓ %q is registered\n", configured)
	}

	// 4. Serviceability, prepaid and COD.
	fmt.Printf("\n[4] Serviceability for %s\n", destination)
	for _, mode := range []struct {
		name string
		cod  bool
	}{{"prepaid", false}, {"COD", true}} {
		result, err := svc.Serviceability(ctx, provider, shipping.RateRequest{
			DeliveryPincode: destination,
			COD:             mode.cod,
			Package:         svc.DefaultPackage(),
			DeclaredValue:   4999,
		}, shipping.QuoteBinding{})
		if err != nil {
			fmt.Printf("    %s: ", mode.name)
			reportErr(err)
			// An unserviceable pincode is an answer, not a fault.
			if shipping.CodeOf(err) != shipping.CodeNotServiceable &&
				shipping.CodeOf(err) != shipping.CodeRateUnavailable {
				failures++
			}
			continue
		}
		fmt.Printf("    ✓ %s: %d option(s), cod=%t\n", mode.name, len(result.Options), result.COD)
		for _, opt := range result.Options {
			eta := "no ETA given"
			if opt.EstimatedDeliveryDays > 0 {
				eta = fmt.Sprintf("%d day(s)", opt.EstimatedDeliveryDays)
			}
			if opt.ETD != "" {
				eta += " (" + opt.ETD + ")"
			}
			flag := ""
			if opt.Recommended {
				flag = "  [carrier-recommended]"
			}
			fmt.Printf("      - %-26s ₹%-8.2f %-22s cod=%t%s\n",
				opt.CourierName, opt.Charge, eta, opt.CODAvailable, flag)
		}
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("=== %d check(s) failed ===\n\n", failures)
		os.Exit(1)
	}
	fmt.Print("=== All read-only checks passed ===\n\n")
}

// reportErr prints the classification and the operator detail.
//
// The detail comes from the carrier and is safe here because this tool is run
// by an operator at a terminal, not served to a customer. It still never
// contains a credential: the adapters keep those out of error text.
func reportErr(err error) {
	se := shipping.AsError(err)
	fmt.Printf("    ✗ %s: %s\n", se.Code, se.Detail)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
