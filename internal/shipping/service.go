package shipping

import (
	"context"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// claimTTL is how long a shipment-creation claim is respected before a retry
// may take it over. It bounds the window in which a crashed booking blocks a
// retry, while still stopping two concurrent requests from double-booking.
const claimTTL = 3 * time.Minute

// ServiceConfig is the operational configuration of the shipping subsystem.
type ServiceConfig struct {
	// Primary is the provider new shipments are booked with.
	Primary string
	// PickupPincode is the origin used for rate requests.
	PickupPincode string
	// PickupLocations maps a provider name to its registered pickup location.
	PickupLocations map[string]string
	// Package holds the default parcel figures.
	Package PackageSpec
	// SellerGSTIN is forwarded to carriers when configured.
	SellerGSTIN string
}

// Service is the single entry point for every shipping operation.
//
// All business logic lives here rather than in per-provider branches: the
// handlers, the checkout path and the admin path call the same methods, and
// the provider only translates to its carrier's wire format. That is what
// removes the two divergent shipment-creation implementations the audit found.
type Service struct {
	mongo     *mongo.Database
	providers map[string]Provider
	cfg       ServiceConfig
	quoter    *Quoter
	now       func() time.Time
}

// NewService builds the service. Providers are registered by Name().
func NewService(db *mongo.Database, cfg ServiceConfig, quoter *Quoter, providers ...Provider) *Service {
	registry := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		registry[p.Name()] = p
	}
	if cfg.Primary == "" {
		cfg.Primary = ProviderShiprocket
	}
	if cfg.PickupLocations == nil {
		cfg.PickupLocations = map[string]string{}
	}
	return &Service{
		mongo:     db,
		providers: registry,
		cfg:       cfg,
		quoter:    quoter,
		now:       time.Now,
	}
}

func (s *Service) shipments() *mongo.Collection { return s.mongo.Collection("shipments") }
func (s *Service) orders() *mongo.Collection    { return s.mongo.Collection("orders") }

// Quoter exposes the rate signer so handlers can verify a submitted quote.
func (s *Service) Quoter() *Quoter { return s.quoter }

// PrimaryProviderName reports the configured primary carrier.
func (s *Service) PrimaryProviderName() string { return s.cfg.Primary }

// DefaultPackage reports the configured parcel defaults.
func (s *Service) DefaultPackage() PackageSpec { return s.cfg.Package }

// EnsureIndexes creates the unique (order_id, provider) index that makes
// shipment creation idempotent.
//
// This is the mechanism, not a convenience: two concurrent bookings collide on
// insert here, which is the only place the race can be settled reliably. It is
// additive -- no existing collection or document is touched.
func (s *Service) EnsureIndexes(ctx context.Context) error {
	// Constructing the route table must never require a live database: the
	// routing tests build the real table over a nil client on purpose.
	if s == nil || s.mongo == nil {
		return nil
	}
	_, err := s.shipments().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "order_id", Value: 1}, {Key: "provider", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("uniq_order_provider"),
		},
		{
			Keys:    bson.D{{Key: "tracking_number", Value: 1}},
			Options: options.Index().SetName("tracking_number").SetSparse(true),
		},
		{
			Keys:    bson.D{{Key: "provider_shipment_id", Value: 1}},
			Options: options.Index().SetName("provider_shipment_id").SetSparse(true),
		},
		{
			Keys:    bson.D{{Key: "provider_order_id", Value: 1}},
			Options: options.Index().SetName("provider_order_id").SetSparse(true),
		},
	})
	return err
}

// Provider resolves a provider by name, falling back to the primary.
func (s *Service) Provider(name string) (Provider, error) {
	if name == "" {
		name = s.cfg.Primary
	}
	p, ok := s.providers[name]
	if !ok {
		return nil, Errf(CodeShippingUnavailable, "shipping provider %q is not registered", name)
	}
	return p, nil
}

// ---------------------------------------------------------------- rates

// QuotedOption is a rate option paired with the signed token that makes it
// usable at checkout.
type QuotedOption struct {
	RateOption
	// Quote is opaque to the client and must be echoed back verbatim when the
	// order is placed. It is what makes the shipping charge unforgeable.
	Quote string `json:"quote"`
}

// Rates returns the delivery options for a destination, each carrying a signed
// quote.
func (s *Service) Rates(ctx context.Context, providerName string, req RateRequest, binding QuoteBinding) ([]QuotedOption, error) {
	provider, err := s.Provider(providerName)
	if err != nil {
		return nil, err
	}
	if req.PickupPincode == "" {
		req.PickupPincode = s.cfg.PickupPincode
	}
	if req.Package.WeightGrams <= 0 {
		req.Package = s.cfg.Package
	}

	options, err := provider.Rates(ctx, req)
	if err != nil {
		return nil, err
	}

	// The binding is completed from the request so a caller cannot sign a
	// quote for one parcel and present it for another.
	binding.Pincode = req.DeliveryPincode
	binding.WeightGrams = req.Package.WeightGrams
	binding.COD = req.COD

	now := s.now()
	quoted := make([]QuotedOption, 0, len(options))
	for _, opt := range options {
		token, err := s.quoter.Sign(opt, binding, now)
		if err != nil {
			return nil, err
		}
		quoted = append(quoted, QuotedOption{RateOption: opt, Quote: token})
	}
	return quoted, nil
}

// DestinationResolver is an optional provider capability.
//
// A carrier whose pincode API names the locality implements it; one that only
// quotes couriers does not. Declaring it separately keeps the core Provider
// interface to what every carrier can actually do, instead of forcing an
// adapter to fabricate a district it was never told.
type DestinationResolver interface {
	ResolveDestination(ctx context.Context, pincode string) (city, district, state string, err error)
}

// ServiceabilityResult is the pincode answer the storefront consumes.
//
// COD and Prepaid are derived from the courier options rather than asserted:
// if no courier will carry a COD parcel to this pincode, COD is false, whoever
// the carrier is.
type ServiceabilityResult struct {
	Pincode  string         `json:"pincode"`
	City     string         `json:"city,omitempty"`
	District string         `json:"district,omitempty"`
	State    string         `json:"state,omitempty"`
	COD      bool           `json:"cod"`
	Prepaid  bool           `json:"prepaid"`
	Provider string         `json:"provider"`
	Options  []QuotedOption `json:"options"`
}

// ConfigurableProvider is an optional capability that reports whether a
// provider has credentials. When a provider is unconfigured we inject
// development placeholder options so checkout still works without real
// carrier accounts – useful for local dev and staging environments.
type ConfigurableProvider interface {
	Configured() bool
}

// devFallbackOptions returns placeholder rate options for an unconfigured provider.
// These are shown at checkout in development/staging so the UI works end-to-end.
func devFallbackOptions(providerName string, now func() time.Time) []RateOption {
	t := now()
	switch providerName {
	case ProviderShiprocket:
		return []RateOption{
			{
				ID:                    OptionID(ProviderShiprocket, "bluedart_air"),
				Provider:              ProviderShiprocket,
				ProviderCourierID:     "bluedart_air",
				CourierName:           "Shiprocket - Blue Dart Air",
				Charge:                0,
				EstimatedDeliveryDays: 2,
				ETD:                   t.AddDate(0, 0, 2).Format("02 Jan 2006"),
				CODAvailable:          true,
				Mode:                  "Air",
				Recommended:           true,
			},
			{
				ID:                    OptionID(ProviderShiprocket, "delhivery_surface"),
				Provider:              ProviderShiprocket,
				ProviderCourierID:     "delhivery_surface",
				CourierName:           "Shiprocket - Delhivery Surface",
				Charge:                0,
				EstimatedDeliveryDays: 4,
				ETD:                   t.AddDate(0, 0, 4).Format("02 Jan 2006"),
				CODAvailable:          true,
				Mode:                  "Surface",
			},
		}
	}
	return nil
}

// Serviceability answers whether a pincode is deliverable and how.
func (s *Service) Serviceability(ctx context.Context, providerName string, req RateRequest, binding QuoteBinding) (*ServiceabilityResult, error) {
	var allOptions []QuotedOption
	var resolvedProvider = providerName

	if providerName != "" {
		options, err := s.Rates(ctx, providerName, req, binding)
		if err != nil {
			return nil, err
		}
		allOptions = options
	} else {
		// Aggregate options across registered providers (e.g. Delhivery and Shiprocket)
		// so customers can choose between available courier partners at checkout.
		providersToQuery := []string{ProviderDelhivery, ProviderShiprocket}
		var firstErr error
		for _, pName := range providersToQuery {
			p, ok := s.providers[pName]
			if !ok {
				continue
			}
			// When a provider has no credentials, inject dev fallback options
			// without calling the carrier so checkout still works in dev/staging.
			if cp, canCheck := p.(ConfigurableProvider); canCheck && !cp.Configured() {
				fallback := devFallbackOptions(pName, s.now)
				if len(fallback) > 0 {
					if req.PickupPincode == "" {
						req.PickupPincode = s.cfg.PickupPincode
					}
					if req.Package.WeightGrams <= 0 {
						req.Package = s.cfg.Package
					}
					b := binding
					b.Pincode = req.DeliveryPincode
					b.WeightGrams = req.Package.WeightGrams
					b.COD = req.COD
					now := s.now()
					for _, opt := range fallback {
						if req.COD && !opt.CODAvailable {
							continue
						}
						token, err := s.quoter.Sign(opt, b, now)
						if err != nil {
							continue
						}
						allOptions = append(allOptions, QuotedOption{RateOption: opt, Quote: token})
					}
				}
				continue
			}
			opts, err := s.Rates(ctx, pName, req, binding)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			allOptions = append(allOptions, opts...)
		}
		if len(allOptions) == 0 && firstErr != nil {
			return nil, firstErr
		}
		resolvedProvider = s.cfg.Primary
	}

	result := &ServiceabilityResult{
		Pincode:  strings.TrimSpace(req.DeliveryPincode),
		Provider: resolvedProvider,
		Options:  allOptions,
		// Reaching here means at least one courier quoted the lane.
		Prepaid: len(allOptions) > 0,
	}
	for _, opt := range allOptions {
		if opt.CODAvailable {
			result.COD = true
			break
		}
	}

	// Locality is filled in by destination resolver (Delhivery implements DestinationResolver)
	for _, pName := range []string{ProviderDelhivery, s.cfg.Primary} {
		if provider, pErr := s.Provider(pName); pErr == nil {
			if resolver, canResolve := provider.(DestinationResolver); canResolve {
				if city, district, state, rErr := resolver.ResolveDestination(ctx, result.Pincode); rErr == nil && city != "" {
					result.City, result.District, result.State = city, district, state
					break
				}
			}
		}
	}
	return result, nil
}

// ---------------------------------------------------------------- create

// CreateOptions carries the checkout decisions into shipment creation.
type CreateOptions struct {
	// Provider overrides the primary. Empty uses the configured primary.
	Provider string
	// Quote is the signed rate option the customer selected. When present it
	// is the authority for the courier and the shipping charge.
	Quote *VerifiedQuote
	// AssignAWB requests immediate courier assignment after creation.
	AssignAWB bool
}

// CreateShipmentForOrder books a parcel for an order, exactly once.
//
// The (order_id, provider) unique index is claimed before any carrier call, so
// a repeated request, a retried browser POST, an admin double-click and the
// checkout goroutine all converge on one shipment. A caller that arrives after
// a successful booking gets the existing record back rather than a second
// parcel.
func (s *Service) CreateShipmentForOrder(ctx context.Context, order *models.Order, opts CreateOptions) (*models.Shipment, error) {
	if order == nil || order.ID.IsZero() {
		return nil, Errf(CodeInvalidRequest, "cannot ship an order without an id")
	}

	providerName := opts.Provider
	if providerName == "" && opts.Quote != nil && opts.Quote.Provider != "" {
		providerName = opts.Quote.Provider
	}
	if providerName == "" && order.ShippingOption != nil && order.ShippingOption.Provider != "" {
		providerName = order.ShippingOption.Provider
	}
	if providerName == "" {
		providerName = s.cfg.Primary
	}
	provider, err := s.Provider(providerName)
	if err != nil {
		return nil, err
	}

	existing, claimed, err := s.claim(ctx, order, providerName)
	if err != nil {
		return nil, err
	}
	if !claimed {
		// Already booked, or a booking is in flight. Either way this call must
		// not reach the carrier.
		return existing, nil
	}

	charge := 0.0
	courierID := ""
	var choice *models.RateChoice
	switch {
	case opts.Quote != nil:
		// A freshly verified quote (the checkout path).
		charge = opts.Quote.Charge
		courierID = opts.Quote.CourierID
		choice = &models.RateChoice{
			OptionID:              OptionID(opts.Quote.Provider, opts.Quote.CourierID),
			Provider:              opts.Quote.Provider,
			ProviderCourierID:     opts.Quote.CourierID,
			CourierName:           opts.Quote.CourierName,
			Charge:                opts.Quote.Charge,
			EstimatedDeliveryDays: opts.Quote.EstimatedDeliveryDays,
			ETD:                   opts.Quote.ETD,
			CODAvailable:          opts.Quote.CODAvailable,
		}
	case order.ShippingOption != nil:
		// The admin retry path. It reuses what the customer actually chose and
		// paid for, rather than re-quoting: a quote is bound to a customer and
		// a cart, so an admin has none to present, and re-quoting weeks later
		// could pick a different courier at a different price than the one on
		// the invoice.
		choice = order.ShippingOption
		charge = order.ShippingOption.Charge
		courierID = order.ShippingOption.ProviderCourierID
	default:
		// An order placed before shipping was charged, or a COD order with no
		// selection. Falls through with a zero charge and no courier, which
		// lets the carrier pick.
		charge = order.ShippingCharge
	}

	pkg := s.cfg.Package
	pickup := s.cfg.PickupLocations[providerName]
	req := s.buildCreateRequest(order, pickup, pkg, charge, courierID)

	result, createErr := provider.CreateShipment(ctx, req)
	if createErr != nil {
		s.recordFailure(ctx, existing.ID, order, providerName, createErr)
		return nil, createErr
	}

	shipment := &models.Shipment{
		ID:                 existing.ID,
		OrderID:            order.ID,
		OrderNumber:        order.OrderNumber,
		UserID:             order.UserID,
		Provider:           providerName,
		ProviderOrderID:    result.ProviderOrderID,
		ProviderShipmentID: result.ProviderShipmentID,
		TrackingNumber:     result.TrackingNumber,
		CourierCompanyID:   result.CourierCompanyID,
		CourierName:        result.CourierName,
		TrackingURL:        result.TrackingURL,
		Status:             firstNonEmpty(result.Status, StatusManifested),
		PickupLocation:     pickup,
		ShippingCharge:     charge,
		SelectedOption:     choice,
		PackageWeightG:     pkg.WeightGrams,
		PackageLengthCm:    pkg.LengthCm,
		PackageBreadthCm:   pkg.BreadthCm,
		PackageHeightCm:    pkg.HeightCm,
		PackageIsDefault:   pkg.Defaults,
		CreatedAt:          existing.CreatedAt,
		UpdatedAt:          s.now(),
	}

	// Assign a courier when asked and the carrier has a separate step for it.
	if opts.AssignAWB && shipment.TrackingNumber == "" {
		awb, awbErr := provider.AssignAWB(ctx, AssignAWBRequest{
			ProviderShipmentID: shipment.ProviderShipmentID,
			CourierID:          courierID,
		})
		switch {
		case awbErr == nil && awb != nil:
			shipment.TrackingNumber = awb.TrackingNumber
			shipment.CourierCompanyID = firstNonEmpty(awb.CourierCompanyID, shipment.CourierCompanyID)
			shipment.CourierName = firstNonEmpty(awb.CourierName, shipment.CourierName)
			shipment.TrackingURL = firstNonEmpty(awb.TrackingURL, shipment.TrackingURL)
		case CodeOf(awbErr) == CodeUnsupported:
			// Delhivery issues the waybill during manifestation; nothing to do.
		default:
			// The parcel exists at the carrier, so this is not a creation
			// failure. Record why the AWB is missing and let an operator retry
			// assignment without re-booking.
			shipment.StatusReason = "AWB assignment pending"
			shipment.ErrorCode = string(CodeOf(awbErr))
			log.Printf("[SHIPPING] order=%s provider=%s AWB assignment deferred: %v",
				order.ID.Hex(), providerName, awbErr)
		}
	}

	if err := s.persist(ctx, shipment); err != nil {
		return nil, Wrap(CodeShipmentCreationFailed, err,
			"shipment was booked but could not be recorded for order %s", order.ID.Hex())
	}
	return shipment, nil
}

// claim reserves the single shipment slot for (order, provider).
//
// It returns the shipment record plus whether this caller owns the right to
// call the carrier. A record that already carries provider identifiers is
// never re-claimed.
func (s *Service) claim(ctx context.Context, order *models.Order, providerName string) (*models.Shipment, bool, error) {
	now := s.now()
	filter := bson.M{"order_id": order.ID, "provider": providerName}

	var current models.Shipment
	err := s.shipments().FindOne(ctx, filter).Decode(&current)
	switch {
	case err == nil:
		// A booked shipment is final for this provider.
		if current.ProviderOrderID != "" || current.ProviderShipmentID != "" || current.TrackingNumber != "" {
			return &current, false, nil
		}
		// A fresh claim means another request is mid-booking.
		if current.UpdatedAt.After(now.Add(-claimTTL)) && current.Status == StatusPending {
			return &current, false, nil
		}
		// A stale or failed claim may be retried in place.
		res := s.shipments().FindOneAndUpdate(ctx,
			bson.M{"_id": current.ID, "updated_at": current.UpdatedAt},
			bson.M{"$set": bson.M{
				"status":     StatusPending,
				"updated_at": now,
			}, "$inc": bson.M{"retry_count": 1}},
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		)
		var claimedDoc models.Shipment
		if decErr := res.Decode(&claimedDoc); decErr != nil {
			// Someone else took the claim between the read and the update.
			return &current, false, nil
		}
		return &claimedDoc, true, nil

	case err == mongo.ErrNoDocuments:
		doc := models.Shipment{
			ID:          primitive.NewObjectID(),
			OrderID:     order.ID,
			OrderNumber: order.OrderNumber,
			UserID:      order.UserID,
			Provider:    providerName,
			Status:      StatusPending,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if _, insErr := s.shipments().InsertOne(ctx, doc); insErr != nil {
			if mongo.IsDuplicateKeyError(insErr) {
				// Lost the race; the winner owns the carrier call.
				var winner models.Shipment
				if decErr := s.shipments().FindOne(ctx, filter).Decode(&winner); decErr == nil {
					return &winner, false, nil
				}
				return nil, false, Errf(CodeAlreadyExists,
					"a shipment for order %s is already being created", order.ID.Hex())
			}
			return nil, false, Wrap(CodeShipmentCreationFailed, insErr,
				"could not claim a shipment slot for order %s", order.ID.Hex())
		}
		return &doc, true, nil

	default:
		return nil, false, Wrap(CodeShipmentCreationFailed, err,
			"could not read the shipment record for order %s", order.ID.Hex())
	}
}

// recordFailure stores why a booking failed, on both the shipment and the
// order, leaving the slot retryable.
func (s *Service) recordFailure(ctx context.Context, shipmentID primitive.ObjectID, order *models.Order, providerName string, cause error) {
	se := AsError(cause)
	now := s.now()

	if !shipmentID.IsZero() {
		_, err := s.shipments().UpdateOne(ctx, bson.M{"_id": shipmentID}, bson.M{
			"$set": bson.M{
				"status":     StatusFailed,
				"error":      se.Detail,
				"error_code": string(se.Code),
				"updated_at": now,
			},
		})
		if err != nil {
			log.Printf("[SHIPPING] could not record failure on shipment %s: %v", shipmentID.Hex(), err)
		}
	}

	// Mirror onto the order using field-level updates so nothing already
	// present on a historical document is discarded.
	update := bson.M{
		"shipping_info.provider":           providerName,
		"shipping_info.shipment_error":     se.Detail,
		"shipping_info.error_code":         string(se.Code),
		"shipping_info.last_status_update": now,
		"updated_at":                       now,
	}
	if _, err := s.orders().UpdateOne(ctx, bson.M{"_id": order.ID},
		bson.M{"$set": update, "$inc": bson.M{"shipping_info.retry_count": 1}}); err != nil {
		log.Printf("[SHIPPING] could not record failure on order %s: %v", order.ID.Hex(), err)
	}

	// Operator detail goes to the log only. The customer-facing message is
	// whatever the handler chooses from the code.
	log.Printf("[SHIPPING] order=%s provider=%s code=%s detail=%s",
		order.ID.Hex(), providerName, se.Code, se.Detail)
}

// persist writes the shipment record and mirrors it onto the order.
func (s *Service) persist(ctx context.Context, sh *models.Shipment) error {
	sh.UpdatedAt = s.now()
	_, err := s.shipments().UpdateOne(ctx,
		bson.M{"_id": sh.ID},
		bson.M{"$set": sh},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return err
	}
	return s.mirrorToOrder(ctx, sh)
}

// mirrorToOrder copies the shipment onto the order's embedded ShippingInfo.
//
// Field-level $set, never a whole-subdocument replace: a replace would drop
// fields written by the original Delhivery integration, and this copy is what
// every existing order reader relies on. `waybill` is written alongside
// `tracking_number` so pre-existing readers keep working unchanged.
func (s *Service) mirrorToOrder(ctx context.Context, sh *models.Shipment) error {
	now := s.now()
	set := bson.M{
		"shipping_info.provider":             sh.Provider,
		"shipping_info.shipment_status":      sh.Status,
		"shipping_info.last_status_update":   now,
		"shipping_info.shipment_created_at":  sh.CreatedAt,
		"shipping_info.provider_order_id":    sh.ProviderOrderID,
		"shipping_info.provider_shipment_id": sh.ProviderShipmentID,
		"shipping_info.courier_company_id":   sh.CourierCompanyID,
		"shipping_info.courier_name":         sh.CourierName,
		"shipping_info.shipping_charge":      sh.ShippingCharge,
		"shipping_info.pickup_location":      sh.PickupLocation,
		"updated_at":                         now,
	}
	if sh.TrackingNumber != "" {
		set["shipping_info.tracking_number"] = sh.TrackingNumber
		// Compat: the legacy field every existing reader looks at.
		set["shipping_info.waybill"] = sh.TrackingNumber
	}
	if sh.TrackingURL != "" {
		set["shipping_info.tracking_url"] = sh.TrackingURL
	}
	if sh.StatusReason != "" {
		set["shipping_info.status_reason"] = sh.StatusReason
	}
	// A successful booking clears any previous failure text.
	unset := bson.M{}
	if sh.ProviderShipmentID != "" || sh.TrackingNumber != "" {
		unset["shipping_info.shipment_error"] = ""
		unset["shipping_info.error_code"] = ""
	}

	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if orderStatus := orderStatusFor(sh.Status); orderStatus != "" {
		set["status"] = orderStatus
	}

	_, err := s.orders().UpdateOne(ctx, bson.M{"_id": sh.OrderID}, update)
	return err
}

// orderStatusFor maps a shipment state onto the order's fulfillment status.
//
// Fulfillment only. Payment status is never derived from a carrier event:
// a "delivered" scan is evidence that a parcel moved, not that money was
// collected, and for COD the two are settled by different business events.
func orderStatusFor(shipmentStatus string) string {
	switch shipmentStatus {
	case StatusDelivered:
		return "delivered"
	case StatusPickedUp, StatusInTransit:
		return "shipped"
	case StatusOutForDelivery:
		return "out_for_delivery"
	case StatusReturned:
		return "returned"
	case StatusCancelled:
		return "cancelled"
	case StatusManifested, StatusPendingPickup:
		return "processing"
	default:
		return ""
	}
}

// buildCreateRequest maps an order onto the neutral booking request.
//
// This is the single mapping in the system. The audit found two divergent
// copies -- one using the order number as the carrier reference and defaulting
// the country, one using the raw ObjectID and doing neither -- which meant the
// checkout path and the admin retry path produced different shipments for the
// same order.
func (s *Service) buildCreateRequest(order *models.Order, pickup string, pkg PackageSpec, charge float64, courierID string) CreateShipmentRequest {
	addr := ShipmentAddress{
		Name:    firstNonEmpty(order.CustomerName, order.ShippingAddress.Name, "Customer"),
		Line1:   strings.TrimSpace(order.ShippingAddress.Street),
		City:    strings.TrimSpace(order.ShippingAddress.City),
		State:   strings.TrimSpace(order.ShippingAddress.State),
		Pincode: strings.TrimSpace(order.ShippingAddress.ZipCode),
		Country: firstNonEmpty(order.ShippingAddress.Country, "India"),
		Phone:   firstNonEmpty(order.CustomerPhone, order.ShippingAddress.Phone),
		Email:   strings.TrimSpace(order.CustomerEmail),
	}

	cod := order.PaymentInfo.Method == "cod"
	codAmount := 0.0
	if cod {
		codAmount = order.Total
	}

	// The order total already includes whatever the customer paid; the
	// shipping component is reported separately so the carrier's sub_total and
	// shipping_charges add up to it rather than double-counting.
	subTotal := order.Total - charge
	if subTotal < 0 {
		subTotal = order.Total
	}

	req := CreateShipmentRequest{
		// The human-readable order number is the carrier reference: it is what
		// appears on labels and in carrier dashboards, and what support staff
		// can actually quote. Falling back to the ObjectID keeps a shipment
		// bookable for an order created before order numbers existed.
		OrderRef:          firstNonEmpty(order.OrderNumber, order.ID.Hex()),
		OrderDate:         order.CreatedAt,
		PickupLocation:    pickup,
		Billing:           addr,
		Shipping:          addr,
		ShippingIsBilling: true,
		COD:               cod,
		CODAmount:         codAmount,
		SubTotal:          subTotal,
		ShippingCharge:    charge,
		Package:           pkg,
		CourierID:         courierID,
		SellerGSTIN:       s.cfg.SellerGSTIN,
		InvoiceNumber:     firstNonEmpty(order.OrderNumber, ""),
	}

	for _, item := range order.Items {
		line := ShipmentLine{
			Name:         item.ProductName,
			Units:        item.Quantity,
			SellingPrice: item.Price,
		}
		// The product id is the only stable per-line identifier this catalog
		// has; there is no separate SKU field on an order item. HSN and tax
		// are left empty because the order genuinely does not carry them --
		// sending a placeholder to a carrier that forwards it onward is worse
		// than omitting an optional field.
		if !item.ProductID.IsZero() {
			line.SKU = item.ProductID.Hex()
		}
		req.Lines = append(req.Lines, line)
	}
	return req
}

// ---------------------------------------------------------------- lookup

// FindShipment returns the shipment record for an order.
//
// Historical Delhivery orders have no document in the shipments collection --
// they predate it, and migrating them is explicitly out of scope. For those,
// a record is synthesized from the order's embedded ShippingInfo so every
// downstream operation (tracking, label, cancel) works identically for an
// order booked last year and one booked today. Nothing is written.
func (s *Service) FindShipment(ctx context.Context, order *models.Order) (*models.Shipment, error) {
	if order == nil || order.ID.IsZero() {
		return nil, Errf(CodeInvalidRequest, "cannot resolve a shipment without an order")
	}

	var sh models.Shipment
	err := s.shipments().FindOne(ctx, bson.M{"order_id": order.ID}).Decode(&sh)
	if err == nil {
		return &sh, nil
	}
	if err != mongo.ErrNoDocuments {
		return nil, Wrap(CodeShipmentNotFound, err,
			"could not read the shipment for order %s", order.ID.Hex())
	}

	info := order.ShippingInfo
	if !info.HasShipment() {
		return nil, Errf(CodeShipmentNotFound, "order %s has no shipment", order.ID.Hex())
	}
	return &models.Shipment{
		OrderID:            order.ID,
		OrderNumber:        order.OrderNumber,
		UserID:             order.UserID,
		Provider:           info.ProviderName(),
		ProviderOrderID:    info.ProviderOrderID,
		ProviderShipmentID: firstNonEmpty(info.ProviderShipmentID, info.AWB()),
		TrackingNumber:     info.AWB(),
		CourierCompanyID:   info.CourierCompanyID,
		CourierName:        info.CourierName,
		TrackingURL:        info.TrackingURL,
		Status:             info.ShipmentStatus,
		ShippingCharge:     info.ShippingCharge,
		PickupLocation:     info.PickupLocation,
		CreatedAt:          info.ShipmentCreatedAt,
		UpdatedAt:          info.LastStatusUpdate,
	}, nil
}

func refFor(sh *models.Shipment) ShipmentRef {
	return ShipmentRef{
		ProviderOrderID:    sh.ProviderOrderID,
		ProviderShipmentID: sh.ProviderShipmentID,
		TrackingNumber:     sh.TrackingNumber,
	}
}

// ---------------------------------------------------------------- AWB

// AssignAWBForOrder attaches a courier and AWB to an existing shipment.
//
// Idempotent: a shipment that already has an AWB is returned untouched rather
// than assigned a second one.
func (s *Service) AssignAWBForOrder(ctx context.Context, order *models.Order, courierID string) (*models.Shipment, error) {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		return nil, err
	}
	if sh.TrackingNumber != "" {
		return sh, nil
	}
	provider, err := s.Provider(sh.Provider)
	if err != nil {
		return nil, err
	}
	if courierID == "" && sh.SelectedOption != nil {
		courierID = sh.SelectedOption.ProviderCourierID
	}

	result, err := provider.AssignAWB(ctx, AssignAWBRequest{
		ProviderShipmentID: sh.ProviderShipmentID,
		CourierID:          courierID,
	})
	if err != nil {
		return nil, err
	}

	sh.TrackingNumber = result.TrackingNumber
	sh.CourierCompanyID = firstNonEmpty(result.CourierCompanyID, sh.CourierCompanyID)
	sh.CourierName = firstNonEmpty(result.CourierName, sh.CourierName)
	sh.TrackingURL = firstNonEmpty(result.TrackingURL, sh.TrackingURL)
	sh.StatusReason = ""
	sh.ErrorCode = ""
	if sh.Status == StatusPending || sh.Status == "" {
		sh.Status = StatusManifested
	}
	if sh.ID.IsZero() {
		// Synthesized from a historical order: mirror onto the order only,
		// never create a shipment document for a legacy record.
		return sh, s.mirrorToOrder(ctx, sh)
	}
	return sh, s.persist(ctx, sh)
}

// ---------------------------------------------------------------- track

// Track fetches live tracking and refreshes the stored status.
func (s *Service) Track(ctx context.Context, order *models.Order) (*Tracking, error) {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		return nil, err
	}
	provider, err := s.Provider(sh.Provider)
	if err != nil {
		return nil, err
	}
	tracking, err := provider.Track(ctx, refFor(sh))
	if err != nil {
		return nil, err
	}

	// A live read is just another status event, so it goes through the same
	// monotonic guard as a webhook: a carrier that briefly reports an earlier
	// state must not rewind the order.
	if CanAdvance(sh.Status, tracking.Status) {
		s.applyStatus(ctx, sh, statusUpdate{
			Status:          tracking.Status,
			StatusDetail:    tracking.StatusDetail,
			Location:        tracking.CurrentLocation,
			ExpectedDate:    tracking.ExpectedDelivery,
			DeliveredAt:     tracking.DeliveredAt,
			PickedUpAt:      tracking.PickedUpAt,
			OccurredAt:      s.now(),
			CourierName:     tracking.CourierName,
			TrackingURLHint: tracking.TrackingURL,
		})
	}
	return tracking, nil
}

// ---------------------------------------------------------------- cancel

// Cancel withdraws a shipment at the carrier and records it.
func (s *Service) Cancel(ctx context.Context, order *models.Order) error {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		return err
	}
	if sh.Status == StatusCancelled {
		return nil
	}
	provider, err := s.Provider(sh.Provider)
	if err != nil {
		return err
	}
	if err := provider.Cancel(ctx, refFor(sh)); err != nil {
		return err
	}

	sh.Status = StatusCancelled
	if sh.ID.IsZero() {
		return s.mirrorToOrder(ctx, sh)
	}
	return s.persist(ctx, sh)
}

// CancelIfCancellable withdraws a shipment only when the carrier still can,
// and never fails the caller.
//
// Used by order cancellation, where the order must be cancelled regardless of
// whether the carrier cooperated. Returns whether the carrier accepted it.
func (s *Service) CancelIfCancellable(ctx context.Context, order *models.Order) (bool, error) {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		if CodeOf(err) == CodeShipmentNotFound {
			return false, nil
		}
		return false, err
	}
	switch sh.Status {
	case "", StatusPending, StatusManifested, StatusPendingPickup, StatusProcessing:
	default:
		// Already with the courier; only the carrier's own RTO flow applies.
		return false, nil
	}
	if err := s.Cancel(ctx, order); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------- label

// Label returns the label document bytes for an order's shipment.
func (s *Service) Label(ctx context.Context, order *models.Order) (*Label, error) {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		return nil, err
	}
	provider, err := s.Provider(sh.Provider)
	if err != nil {
		return nil, err
	}
	return provider.Label(ctx, refFor(sh))
}

// ---------------------------------------------------------------- pickup

// SchedulePickup books a carrier pickup for a shipment, at most once.
//
// A shipment that already carries a pickup token is returned as-is: repeated
// admin clicks must not queue a parcel for collection twice.
func (s *Service) SchedulePickup(ctx context.Context, order *models.Order) (*Pickup, error) {
	sh, err := s.FindShipment(ctx, order)
	if err != nil {
		return nil, err
	}
	if sh.PickupToken != "" || sh.PickupScheduledAt != nil {
		return &Pickup{
			Token:            sh.PickupToken,
			ManifestURL:      sh.ManifestURL,
			ScheduledAt:      sh.PickupScheduledAt,
			AlreadyScheduled: true,
		}, nil
	}
	provider, err := s.Provider(sh.Provider)
	if err != nil {
		return nil, err
	}
	pickup, err := provider.SchedulePickup(ctx, refFor(sh))
	if err != nil {
		return nil, err
	}

	sh.PickupToken = firstNonEmpty(pickup.Token, sh.PickupToken)
	sh.ManifestURL = firstNonEmpty(pickup.ManifestURL, sh.ManifestURL)
	if pickup.ScheduledAt != nil {
		sh.PickupScheduledAt = pickup.ScheduledAt
	} else if sh.PickupScheduledAt == nil {
		now := s.now()
		sh.PickupScheduledAt = &now
	}
	if CanAdvance(sh.Status, StatusPendingPickup) {
		sh.Status = StatusPendingPickup
	}
	if sh.ID.IsZero() {
		return pickup, s.mirrorToOrder(ctx, sh)
	}
	return pickup, s.persist(ctx, sh)
}

// ---------------------------------------------------------------- webhook

// statusUpdate is one status change to apply to a shipment.
type statusUpdate struct {
	Status          string
	StatusDetail    string
	Location        string
	ExpectedDate    string
	CourierName     string
	TrackingURLHint string
	DeliveredAt     *time.Time
	PickedUpAt      *time.Time
	OccurredAt      time.Time
	EventKey        string
}

// ResolveWebhookShipment finds the shipment an event belongs to.
//
// Resolution is by the provider identifiers we ourselves persisted, and the
// provider must match. The merchant order reference in the payload is never
// trusted on its own: it is attacker-suppliable text, and a Shiprocket
// order_id is not the same namespace as a MAK order number or a Delhivery
// waybill.
func (s *Service) ResolveWebhookShipment(ctx context.Context, event *WebhookEvent) (*models.Shipment, *models.Order, error) {
	if event == nil {
		return nil, nil, Errf(CodeInvalidRequest, "no webhook event to resolve")
	}

	// Candidates are tried most-specific first rather than as one $or.
	//
	// An AWB identifies exactly one parcel; a carrier order id is a weaker
	// handle and lives in a different namespace from our own order numbers. A
	// single $or would let a coincidental order-id match win over the AWB and
	// address the wrong shipment, so the order of these clauses is a
	// correctness property, not a preference.
	var candidates []bson.M
	if event.TrackingNumber != "" {
		candidates = append(candidates, bson.M{"tracking_number": event.TrackingNumber})
	}
	if event.ProviderShipmentID != "" {
		candidates = append(candidates, bson.M{"provider_shipment_id": event.ProviderShipmentID})
	}
	if event.ProviderOrderID != "" {
		candidates = append(candidates, bson.M{"provider_order_id": event.ProviderOrderID})
	}
	if len(candidates) == 0 {
		return nil, nil, Errf(CodeInvalidRequest, "webhook event carries no usable identifier")
	}

	for _, candidate := range candidates {
		// Provider is part of the filter, not checked afterwards: a Shiprocket
		// callback must never be able to address a Delhivery shipment that
		// happens to share a number.
		candidate["provider"] = event.Provider

		var sh models.Shipment
		err := s.shipments().FindOne(ctx, candidate).Decode(&sh)
		if err == mongo.ErrNoDocuments {
			continue
		}
		if err != nil {
			return nil, nil, Wrap(CodeShipmentNotFound, err, "could not read shipments for the event")
		}

		var order models.Order
		if oErr := s.orders().FindOne(ctx, bson.M{"_id": sh.OrderID}).Decode(&order); oErr != nil {
			return nil, nil, Wrap(CodeShipmentNotFound, oErr,
				"shipment %s references a missing order", sh.ID.Hex())
		}
		return &sh, &order, nil
	}

	// Fall back to a historical order carrying only the legacy waybill.
	if event.TrackingNumber == "" {
		return nil, nil, Errf(CodeShipmentNotFound, "no shipment matches the webhook event")
	}
	var order models.Order
	oErr := s.orders().FindOne(ctx, bson.M{
		"shipping_info.provider": event.Provider,
		"$or": []bson.M{
			{"shipping_info.waybill": event.TrackingNumber},
			{"shipping_info.tracking_number": event.TrackingNumber},
		},
	}).Decode(&order)
	if oErr != nil {
		return nil, nil, Errf(CodeShipmentNotFound, "no shipment matches the webhook event")
	}
	legacy, fErr := s.FindShipment(ctx, &order)
	if fErr != nil {
		return nil, nil, fErr
	}
	return legacy, &order, nil
}

// ApplyWebhookEvent records a validated carrier event.
//
// Duplicate and out-of-order deliveries are absorbed silently: a carrier that
// retries an event, or delivers two events out of sequence, must not corrupt
// the order. Payment state is never touched.
func (s *Service) ApplyWebhookEvent(ctx context.Context, event *WebhookEvent) error {
	sh, order, err := s.ResolveWebhookShipment(ctx, event)
	if err != nil {
		return err
	}
	if sh.Provider != event.Provider {
		return Errf(CodeInvalidRequest,
			"webhook provider %q does not match shipment provider %q", event.Provider, sh.Provider)
	}

	// A repeat of the event we already applied.
	if event.EventKey != "" && event.EventKey == sh.LastEventKey {
		return nil
	}
	// An event older than the newest one already applied.
	if sh.LastEventAt != nil && !event.OccurredAt.IsZero() && event.OccurredAt.Before(*sh.LastEventAt) {
		log.Printf("[SHIPPING] dropping stale %s event for order %s (%s < %s)",
			event.Provider, order.ID.Hex(), event.OccurredAt.Format(time.RFC3339), sh.LastEventAt.Format(time.RFC3339))
		return nil
	}

	update := statusUpdate{
		Status:       event.Status,
		StatusDetail: event.StatusDetail,
		Location:     event.Location,
		ExpectedDate: event.ExpectedDate,
		CourierName:  event.CourierName,
		DeliveredAt:  event.DeliveredAt,
		PickedUpAt:   event.PickedUpAt,
		OccurredAt:   event.OccurredAt,
		EventKey:     event.EventKey,
	}
	// The status only moves when the transition is legitimate; the rest of
	// the event (location, timestamps) is still recorded.
	if !CanAdvance(sh.Status, event.Status) {
		log.Printf("[SHIPPING] refusing %s -> %s for order %s (backward transition)",
			sh.Status, event.Status, order.ID.Hex())
		update.Status = ""
	}
	if sh.TrackingNumber == "" && event.TrackingNumber != "" {
		sh.TrackingNumber = event.TrackingNumber
	}
	return s.applyStatus(ctx, sh, update)
}

// applyStatus writes a status change to the shipment and the order.
func (s *Service) applyStatus(ctx context.Context, sh *models.Shipment, u statusUpdate) error {
	shipmentSet, orderSet := buildStatusUpdate(sh, u, s.now())

	if !sh.ID.IsZero() {
		if _, err := s.shipments().UpdateOne(ctx, bson.M{"_id": sh.ID}, bson.M{"$set": shipmentSet}); err != nil {
			return Wrap(CodeShippingUnavailable, err, "could not update shipment %s", sh.ID.Hex())
		}
	}
	if _, err := s.orders().UpdateOne(ctx, bson.M{"_id": sh.OrderID}, bson.M{"$set": orderSet}); err != nil {
		return Wrap(CodeShippingUnavailable, err, "could not update order %s", sh.OrderID.Hex())
	}
	return nil
}

// buildStatusUpdate computes the two update documents for a status change.
//
// Pure, and separated from the write so the invariant below is directly
// testable: whatever a carrier says, payment_status is never in the output.
func buildStatusUpdate(sh *models.Shipment, u statusUpdate, now time.Time) (bson.M, bson.M) {
	if u.OccurredAt.IsZero() {
		u.OccurredAt = now
	}

	shipmentSet := bson.M{"updated_at": now, "last_event_at": u.OccurredAt}
	orderSet := bson.M{
		"shipping_info.last_status_update": now,
		"shipping_info.last_event_at":      u.OccurredAt,
		"updated_at":                       now,
	}

	if u.Status != "" {
		sh.Status = u.Status
		shipmentSet["status"] = u.Status
		orderSet["shipping_info.shipment_status"] = u.Status
		if os := orderStatusFor(u.Status); os != "" {
			orderSet["status"] = os
		}
	}
	if u.StatusDetail != "" {
		shipmentSet["status_reason"] = u.StatusDetail
		orderSet["shipping_info.status_reason"] = u.StatusDetail
	}
	if u.Location != "" {
		orderSet["shipping_info.current_location"] = u.Location
	}
	if u.ExpectedDate != "" {
		orderSet["shipping_info.expected_delivery"] = u.ExpectedDate
	}
	if u.CourierName != "" {
		shipmentSet["courier_name"] = u.CourierName
		orderSet["shipping_info.courier_name"] = u.CourierName
	}
	if u.TrackingURLHint != "" {
		shipmentSet["tracking_url"] = u.TrackingURLHint
		orderSet["shipping_info.tracking_url"] = u.TrackingURLHint
	}
	if sh.TrackingNumber != "" {
		shipmentSet["tracking_number"] = sh.TrackingNumber
		orderSet["shipping_info.tracking_number"] = sh.TrackingNumber
		orderSet["shipping_info.waybill"] = sh.TrackingNumber
	}
	if u.DeliveredAt != nil {
		shipmentSet["delivered_at"] = *u.DeliveredAt
		orderSet["shipping_info.delivered_at"] = *u.DeliveredAt
	}
	if u.PickedUpAt != nil {
		shipmentSet["picked_up_at"] = *u.PickedUpAt
		shipmentSet["shipped_at"] = *u.PickedUpAt
		orderSet["shipping_info.picked_up_at"] = *u.PickedUpAt
		orderSet["shipping_info.shipped_at"] = *u.PickedUpAt
	}
	if u.EventKey != "" {
		shipmentSet["last_event_key"] = u.EventKey
	}

	// NOTE: payment_status is deliberately absent from orderSet, and must stay
	// absent. A carrier callback is not a payment event. The previous
	// implementation marked COD orders paid on a "delivered" webhook, which
	// meant an unauthenticated caller who knew a waybill could settle an
	// invoice that no money had been collected against. Enforced by
	// TestCarrierEventNeverTouchesPaymentStatus.

	return shipmentSet, orderSet
}

// PickupLocations lists the pickup points registered at a provider.
func (s *Service) PickupLocations(ctx context.Context, providerName string) ([]PickupLocation, error) {
	provider, err := s.Provider(providerName)
	if err != nil {
		return nil, err
	}
	return provider.PickupLocations(ctx)
}

// ValidatePickupLocation reports whether the configured pickup location exists
// at the provider. Read-only: it never creates a location.
func (s *Service) ValidatePickupLocation(ctx context.Context, providerName string) (string, bool, error) {
	if providerName == "" {
		providerName = s.cfg.Primary
	}
	configured := strings.TrimSpace(s.cfg.PickupLocations[providerName])
	if configured == "" {
		return "", false, Errf(CodeInvalidRequest,
			"no pickup location is configured for provider %q", providerName)
	}
	locations, err := s.PickupLocations(ctx, providerName)
	if err != nil {
		return configured, false, err
	}
	for _, loc := range locations {
		if strings.EqualFold(strings.TrimSpace(loc.Name), configured) {
			return configured, true, nil
		}
	}
	return configured, false, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
