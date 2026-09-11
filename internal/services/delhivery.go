package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DelhiveryConfig holds configuration for Delhivery API
type DelhiveryConfig struct {
	APIToken       string
	BaseURL        string // "https://track.delhivery.com" for production, "https://staging-express.delhivery.com" for staging
	PickupLocation string // Registered pickup location name
	SellerName     string
	SellerPhone    string
	SellerAddress  string
	SellerCity     string
	SellerState    string
	SellerPincode  string
	ReturnAddress  string
	ReturnCity     string
	ReturnState    string
	ReturnPincode  string
	ReturnPhone    string
}

// DelhiveryService handles all Delhivery API interactions
type DelhiveryService struct {
	config     DelhiveryConfig
	httpClient *http.Client
}

// NewDelhiveryService creates a new Delhivery service instance
func NewDelhiveryService(config DelhiveryConfig) *DelhiveryService {
	return &DelhiveryService{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ShipmentItem represents an item in the shipment
type ShipmentItem struct {
	Name     string  `json:"name"`
	SKU      string  `json:"sku"`
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
}

// CreateShipmentRequest represents the data needed to create a shipment
type CreateShipmentRequest struct {
	// Customer details
	CustomerName    string `json:"customer_name"`
	CustomerPhone   string `json:"customer_phone"`
	CustomerEmail   string `json:"customer_email,omitempty"`
	CustomerAddress string `json:"customer_address"`
	CustomerCity    string `json:"customer_city"`
	CustomerState   string `json:"customer_state"`
	CustomerPincode string `json:"customer_pincode"`
	CustomerCountry string `json:"customer_country"`

	// Order details
	OrderID         string         `json:"order_id"`
	OrderDate       string         `json:"order_date"`
	TotalAmount     float64        `json:"total_amount"`
	PaymentMode     string         `json:"payment_mode"` // "Prepaid" or "COD"
	CODAmount       float64        `json:"cod_amount"`   // Amount to collect on delivery (0 for prepaid)
	Items           []ShipmentItem `json:"items"`
	ProductQuantity int            `json:"product_quantity"`

	// Package details
	Weight       float64 `json:"weight"`       // in grams
	Length       float64 `json:"length"`       // in cm
	Breadth      float64 `json:"breadth"`      // in cm
	Height       float64 `json:"height"`       // in cm
	ProductDesc  string  `json:"product_desc"` // Description of contents
	HSNCode      string  `json:"hsn_code,omitempty"`
	SellerGSTIN  string  `json:"seller_gstin,omitempty"`
	TaxableValue float64 `json:"taxable_value,omitempty"`
}

// CreateShipmentResponse represents the response from creating a shipment
type CreateShipmentResponse struct {
	Success    bool   `json:"success"`
	Waybill    string `json:"waybill"`
	OrderID    string `json:"order_id"`
	RefNum     string `json:"refnum,omitempty"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status,omitempty"`
	ShipmentID string `json:"shipment_id,omitempty"`
}

// DelhiveryOrderFormat represents the format expected by Delhivery API
type DelhiveryOrderFormat struct {
	PickupLocation PickupLocation      `json:"pickup_location"`
	Shipments      []DelhiveryShipment `json:"shipments"`
}

// PickupLocation represents the pickup location for the shipment
type PickupLocation struct {
	Name string `json:"name"`
}

// DelhiveryShipment represents a single shipment in Delhivery format
type DelhiveryShipment struct {
	Name         string `json:"name"`
	Add          string `json:"add"`
	City         string `json:"city"`
	State        string `json:"state"`
	Country      string `json:"country"`
	Pin          string `json:"pin"`
	Phone        string `json:"phone"`
	Order        string `json:"order"`
	OrderDate    string `json:"order_date,omitempty"`
	PaymentMode  string `json:"payment_mode"` // "Prepaid" or "COD"
	ProductsDesc string `json:"products_desc"`
	Quantity     string `json:"quantity"`
	Weight       string `json:"weight"` // in kg
	ShippingMode string `json:"shipping_mode,omitempty"`
	AddressType  string `json:"address_type,omitempty"`
	SellerName   string `json:"seller_name"`
	SellerAdd    string `json:"seller_add"`
	SellerInv    string `json:"seller_inv,omitempty"`
	TotalAmount  string `json:"total_amount"`
	CODAmount    string `json:"cod_amount"`
	SellerGSTIN  string `json:"seller_gst_tin,omitempty"`
	HSNCode      string `json:"hsn_code,omitempty"`
}

// CreateShipment creates a new shipment with Delhivery
func (s *DelhiveryService) CreateShipment(req CreateShipmentRequest) (*CreateShipmentResponse, error) {
	log.Printf("[DELHIVERY-API] Starting CreateShipment for order: %s", req.OrderID)
	log.Printf("[DELHIVERY-API] Config - BaseURL: %s, PickupLocation: %s", s.config.BaseURL, s.config.PickupLocation)

	// Prepare pickup location name
	pickupLocationName := s.config.PickupLocation
	if pickupLocationName == "" {
		pickupLocationName = "Shree ganesh watch"
		log.Printf("[DELHIVERY-API] WARNING: pickup_location env var is empty, using default: %s", pickupLocationName)
	}

	// Convert weight from grams to kg for Delhivery API
	weightInKg := req.Weight / 1000
	if weightInKg < 0.5 {
		weightInKg = 0.5 // Minimum weight requirement
	}

	// Get order date in correct format (YYYY-MM-DD)
	orderDate := req.OrderDate
	if orderDate == "" {
		orderDate = time.Now().Format("2006-01-02")
	}

	// Build the shipment object with minimal required fields
	shipment := DelhiveryShipment{
		// Customer/Delivery details
		Name:    req.CustomerName,
		Add:     req.CustomerAddress,
		City:    req.CustomerCity,
		State:   req.CustomerState,
		Country: req.CustomerCountry,
		Pin:     req.CustomerPincode,
		Phone:   req.CustomerPhone,

		// Order details
		Order:        req.OrderID,
		OrderDate:    orderDate,
		PaymentMode:  req.PaymentMode,
		ProductsDesc: req.ProductDesc,
		Quantity:     fmt.Sprintf("%d", req.ProductQuantity),
		Weight:       fmt.Sprintf("%.2f", weightInKg),
		ShippingMode: "Surface",
		AddressType:  "Home",

		// Seller details
		SellerName: s.config.SellerName,
		SellerAdd:  fmt.Sprintf("%s, %s", s.config.SellerCity, s.config.SellerState),
		SellerInv:  fmt.Sprintf("INV-%s", req.OrderID),

		// Amount details
		TotalAmount: fmt.Sprintf("%.2f", req.TotalAmount),
		CODAmount:   fmt.Sprintf("%.2f", req.CODAmount),
	}

	// Add optional fields if provided
	if req.SellerGSTIN != "" {
		shipment.SellerGSTIN = req.SellerGSTIN
	}
	if req.HSNCode != "" {
		shipment.HSNCode = req.HSNCode
	}

	log.Printf("[DELHIVERY-API] Shipment Details:")
	log.Printf("  - Customer: %s, %s, %s, %s - %s", shipment.Name, shipment.Add, shipment.City, shipment.State, shipment.Pin)
	log.Printf("  - Order: %s, Payment: %s, Weight: %s kg", shipment.Order, shipment.PaymentMode, shipment.Weight)
	log.Printf("  - Seller: %s, %s", shipment.SellerName, shipment.SellerAdd)

	// Create the order format with pickup_location inside the data object
	orderData := DelhiveryOrderFormat{
		PickupLocation: PickupLocation{
			Name: pickupLocationName,
		},
		Shipments: []DelhiveryShipment{shipment},
	}

	// Convert to JSON
	jsonData, err := json.Marshal(orderData)
	if err != nil {
		log.Printf("[DELHIVERY-API] ERROR: Failed to marshal shipment data: %v", err)
		return nil, fmt.Errorf("failed to marshal shipment data: %w", err)
	}
	log.Printf("[DELHIVERY-API] Shipment JSON: %s", string(jsonData))

	// Create form data as expected by Delhivery API
	formData := url.Values{}
	formData.Set("format", "json")
	formData.Set("data", string(jsonData))

	log.Printf("[DELHIVERY-API] ✓ Form data prepared with pickup_location inside JSON")

	// Create the request
	apiURL := fmt.Sprintf("%s/api/cmu/create.json", s.config.BaseURL)
	log.Printf("[DELHIVERY-API] API URL: %s", apiURL)

	httpReq, err := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	if err != nil {
		log.Printf("[DELHIVERY-API] ERROR: Failed to create HTTP request: %v", err)
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))
	httpReq.Header.Set("Accept", "application/json")

	log.Printf("[DELHIVERY-API] Headers set, making HTTP request...")

	// Make the request
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		log.Printf("[DELHIVERY-API] ERROR: HTTP request failed: %v", err)
		return nil, fmt.Errorf("failed to send request to Delhivery: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("[DELHIVERY-API] HTTP Response Status: %s (%d)", resp.Status, resp.StatusCode)

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[DELHIVERY-API] ERROR: Failed to read response body: %v", err)
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	log.Printf("[DELHIVERY-API] Response Body: %s", string(body))

	// Parse response
	var apiResp struct {
		CashPickups      float64       `json:"cash_pickups"`
		CashPickupsCount float64       `json:"cash_pickups_count"`
		CODAmount        float64       `json:"cod_amount"`
		CODCount         float64       `json:"cod_count"`
		PackageCount     float64       `json:"package_count"`
		PrepaidCount     float64       `json:"prepaid_count"`
		ReplacementCount float64       `json:"replacement_count"`
		RmaCount         float64       `json:"rma_count"`
		PickupsCount     float64       `json:"pickups_count"`
		Success          bool          `json:"success"`
		UploadWBN        string        `json:"upload_wbn"`
		Packages         []interface{} `json:"packages"`
		NotEnoughInfo    []interface{} `json:"not_enough_info"`
		OverweightError  []interface{} `json:"overweight_error"`
		PincodeError     []interface{} `json:"pincode_error"`
		AddressError     []interface{} `json:"address_error"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse Delhivery response: %w, body: %s", err, string(body))
	}

	// Check for errors
	if len(apiResp.NotEnoughInfo) > 0 || len(apiResp.PincodeError) > 0 || len(apiResp.AddressError) > 0 {
		errMsg := "Shipment creation failed: "
		if len(apiResp.NotEnoughInfo) > 0 {
			errMsg += fmt.Sprintf("Not enough info: %v; ", apiResp.NotEnoughInfo)
		}
		if len(apiResp.PincodeError) > 0 {
			errMsg += fmt.Sprintf("Pincode error: %v; ", apiResp.PincodeError)
		}
		if len(apiResp.AddressError) > 0 {
			errMsg += fmt.Sprintf("Address error: %v; ", apiResp.AddressError)
		}
		log.Printf("[DELHIVERY] ❌ Shipment creation failed: %s", errMsg)
		return nil, fmt.Errorf("%s", errMsg)
	}

	// Extract waybill from packages
	result := &CreateShipmentResponse{
		Success: apiResp.Success,
		OrderID: req.OrderID,
	}

	if len(apiResp.Packages) > 0 {
		if pkg, ok := apiResp.Packages[0].(map[string]interface{}); ok {
			if waybill, ok := pkg["waybill"].(string); ok {
				result.Waybill = waybill
			}
			if refnum, ok := pkg["refnum"].(string); ok {
				result.RefNum = refnum
			}
			if status, ok := pkg["status"].(string); ok {
				result.Status = status
			}
		}
	}

	if result.Waybill == "" && apiResp.UploadWBN != "" {
		result.Waybill = apiResp.UploadWBN
	}

	log.Printf("[DELHIVERY-API] ✓ Shipment created successfully. Waybill: %s", result.Waybill)

	return result, nil
}

// TrackingStatus represents tracking information for a shipment
type TrackingStatus struct {
	Waybill            string         `json:"waybill"`
	Status             string         `json:"status"`
	StatusCode         string         `json:"status_code"`
	StatusType         string         `json:"status_type"`
	StatusLocation     string         `json:"status_location"`
	StatusDateTime     string         `json:"status_datetime"`
	ExpectedDelivery   string         `json:"expected_delivery,omitempty"`
	CurrentLocation    string         `json:"current_location,omitempty"`
	Scans              []TrackingScan `json:"scans,omitempty"`
	ShipmentStatus     string         `json:"shipment_status"`
	ReferenceNo        string         `json:"reference_no,omitempty"`
	DestinationCity    string         `json:"destination_city,omitempty"`
	DestinationPincode string         `json:"destination_pincode,omitempty"`
	FlowCountry        string         `json:"flow_country,omitempty"`
}

// TrackingScan represents a single tracking event
type TrackingScan struct {
	ScanDateTime string `json:"scan_datetime"`
	ScanType     string `json:"scan_type"`
	ScannedAt    string `json:"scanned_location"`
	Instructions string `json:"instructions,omitempty"`
	Remarks      string `json:"remarks,omitempty"`
	StatusDetail string `json:"status_detail,omitempty"`
}

// TrackShipment gets the tracking status of a shipment
func (s *DelhiveryService) TrackShipment(waybill string) (*TrackingStatus, error) {
	apiURL := fmt.Sprintf("%s/api/v1/packages/json/?waybill=%s", s.config.BaseURL, waybill)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create tracking request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to track shipment: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read tracking response: %w", err)
	}

	var trackResp struct {
		ShipmentData []struct {
			Shipment struct {
				Status struct {
					Status         string `json:"Status"`
					StatusCode     string `json:"StatusCode"`
					StatusType     string `json:"StatusType"`
					StatusLocation string `json:"StatusLocation"`
					StatusDateTime string `json:"StatusDateTime"`
					Instructions   string `json:"Instructions"`
					Remarks        string `json:"Remarks"`
				} `json:"Status"`
				Scans []struct {
					ScanDetail struct {
						ScanDateTime    string `json:"ScanDateTime"`
						ScanType        string `json:"ScanType"`
						Scan            string `json:"Scan"`
						ScannedLocation string `json:"ScannedLocation"`
						Instructions    string `json:"Instructions"`
						StatusDateTime  string `json:"StatusDateTime"`
					} `json:"ScanDetail"`
				} `json:"Scans"`
				Destination          string `json:"Destination"`
				DestinationRecieved  bool   `json:"DestinationRecieved"`
				ExpectedDeliveryDate string `json:"ExpectedDeliveryDate"`
				PickUpDate           string `json:"PickUpDate"`
				OriginRecievedDate   string `json:"OriginRecievedDate"`
				ReferenceNo          string `json:"ReferenceNo"`
				Consignee            struct {
					City    string `json:"City"`
					Address string `json:"Address"`
					PinCode int    `json:"PinCode"`
				} `json:"Consignee"`
			} `json:"Shipment"`
		} `json:"ShipmentData"`
	}

	if err := json.Unmarshal(body, &trackResp); err != nil {
		return nil, fmt.Errorf("failed to parse tracking response: %w", err)
	}

	if len(trackResp.ShipmentData) == 0 {
		return nil, fmt.Errorf("no tracking data found for waybill: %s", waybill)
	}

	shipData := trackResp.ShipmentData[0].Shipment
	tracking := &TrackingStatus{
		Waybill:            waybill,
		Status:             shipData.Status.Status,
		StatusCode:         shipData.Status.StatusCode,
		StatusType:         shipData.Status.StatusType,
		StatusLocation:     shipData.Status.StatusLocation,
		StatusDateTime:     shipData.Status.StatusDateTime,
		ExpectedDelivery:   shipData.ExpectedDeliveryDate,
		ReferenceNo:        shipData.ReferenceNo,
		DestinationCity:    shipData.Consignee.City,
		DestinationPincode: fmt.Sprintf("%d", shipData.Consignee.PinCode),
		ShipmentStatus:     mapDelhiveryStatusToInternal(shipData.Status.StatusType),
	}

	// Add scans
	for _, scan := range shipData.Scans {
		tracking.Scans = append(tracking.Scans, TrackingScan{
			ScanDateTime: scan.ScanDetail.ScanDateTime,
			ScanType:     scan.ScanDetail.ScanType,
			ScannedAt:    scan.ScanDetail.ScannedLocation,
			Instructions: scan.ScanDetail.Instructions,
			StatusDetail: scan.ScanDetail.Scan,
		})
	}

	return tracking, nil
}

// PincodeServiceability represents pincode check response
// Distinguishing "the carrier says no" from "we could not ask" matters at
// checkout: telling a customer we do not deliver to them, when the truth is
// that our carrier credentials failed, loses the order for no reason.
var (
	// ErrCarrierUnavailable: the carrier could not be reached, rejected our
	// credentials, or answered with something we could not read.
	ErrCarrierUnavailable = errors.New("carrier unavailable")
	// ErrPincodeNotServiceable: the carrier answered, and the answer is no.
	ErrPincodeNotServiceable = errors.New("pincode not serviceable")
)

type PincodeServiceability struct {
	Pincode        string `json:"pincode"`
	District       string `json:"district"`
	State          string `json:"state"`
	City           string `json:"city"`
	COD            bool   `json:"cod"`           // COD available
	Prepaid        bool   `json:"prepaid"`       // Prepaid available
	Pickup         bool   `json:"pickup"`        // Pickup available
	ReachableODA   bool   `json:"reachable_oda"` // Out of Delivery Area but reachable
	SortCode       string `json:"sort_code"`
	MaxWeight      int    `json:"max_weight"` // Maximum weight allowed in grams
	MaxAmount      int    `json:"max_amount"` // Maximum COD amount allowed
	PrePaidAmount  int    `json:"pre_paid_amount"`
	CODAmount      int    `json:"cod_amount"`
	StateCode      string `json:"state_code"`
	Remarks        string `json:"remarks"`
	IncomingCenter string `json:"incoming_center"`
}

// CheckPincodeServiceability checks if a pincode is serviceable
func (s *DelhiveryService) CheckPincodeServiceability(pincode string) (*PincodeServiceability, error) {
	apiURL := fmt.Sprintf("%s/c/api/pin-codes/json/?filter_codes=%s", s.config.BaseURL, pincode)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pincode check request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCarrierUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCarrierUnavailable, err)
	}

	// A non-2xx answer means we failed to *ask*, not that the pincode is
	// unserviceable. The two were indistinguishable before: an expired token
	// returns an error body that parses into zero delivery codes, so every
	// address in the country was reported as undeliverable.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: carrier returned %d: %s",
			ErrCarrierUnavailable, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Mirrors the carrier's actual shape: each delivery_codes entry wraps a
	// `postal_code` *object*. The previous struct expected those fields flat on
	// the entry, so unmarshalling always failed -- and because the failure was
	// reported as "not serviceable", every address in the country looked
	// undeliverable. max_amount/max_weight are decimals, not integers.
	var pincodeResp struct {
		DeliveryCode []struct {
			PostalCode struct {
				Pin         int     `json:"pin"`
				District    string  `json:"district"`
				StateCode   string  `json:"state_code"`
				CountryCode string  `json:"country_code"`
				PrePaid     string  `json:"pre_paid"` // "Y" or "N"
				Cash        string  `json:"cash"`
				Pickup      string  `json:"pickup"`
				COD         string  `json:"cod"`
				IsODA       string  `json:"is_oda"`
				SortCode    string  `json:"sort_code"`
				MaxWeight   float64 `json:"max_weight"`
				MaxAmount   float64 `json:"max_amount"`
				Remarks     string  `json:"remarks"`
				Inc         string  `json:"inc"`
			} `json:"postal_code"`
		} `json:"delivery_codes"`
	}

	if err := json.Unmarshal(body, &pincodeResp); err != nil {
		return nil, fmt.Errorf("%w: could not parse the carrier response: %v", ErrCarrierUnavailable, err)
	}

	// A 2xx with no delivery codes is the carrier's definitive negative.
	if len(pincodeResp.DeliveryCode) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrPincodeNotServiceable, pincode)
	}

	pc := pincodeResp.DeliveryCode[0].PostalCode
	return &PincodeServiceability{
		Pincode:  strconv.Itoa(pc.Pin),
		District: pc.District,
		// The carrier reports a district and a state code; there is no separate
		// city field. Reporting the district as the city is what it means here.
		City:           pc.District,
		State:          pc.StateCode,
		COD:            pc.COD == "Y",
		Prepaid:        pc.PrePaid == "Y",
		Pickup:         pc.Pickup == "Y",
		ReachableODA:   pc.IsODA == "Y",
		SortCode:       pc.SortCode,
		MaxWeight:      int(pc.MaxWeight),
		MaxAmount:      int(pc.MaxAmount),
		StateCode:      pc.StateCode,
		Remarks:        pc.Remarks,
		IncomingCenter: pc.Inc,
	}, nil
}

// CancelShipment cancels a shipment (only if not picked up yet)
func (s *DelhiveryService) CancelShipment(waybill string) error {
	log.Printf("[DELHIVERY-CANCEL] 🚫 Cancelling shipment with waybill: %s", waybill)
	apiURL := fmt.Sprintf("%s/api/p/edit", s.config.BaseURL)

	formData := url.Values{}
	formData.Set("waybill", waybill)
	formData.Set("cancellation", "true")

	log.Printf("[DELHIVERY-CANCEL] 📡 API URL: %s", apiURL)

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create cancel request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("[DELHIVERY-CANCEL] ❌ HTTP request failed: %v", err)
		return fmt.Errorf("failed to cancel shipment: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("[DELHIVERY-CANCEL] 📥 Response status: %s", resp.Status)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read cancel response: %w", err)
	}

	log.Printf("[DELHIVERY-CANCEL] 📄 Response body: %s", string(body))

	var cancelResp struct {
		Status  bool   `json:"status"`
		Error   string `json:"error,omitempty"`
		Message string `json:"message,omitempty"`
	}

	if err := json.Unmarshal(body, &cancelResp); err != nil {
		return fmt.Errorf("failed to parse cancel response: %w, body: %s", err, string(body))
	}

	if !cancelResp.Status {
		errMsg := cancelResp.Error
		if errMsg == "" {
			errMsg = cancelResp.Message
		}
		if errMsg == "" {
			errMsg = "unknown error"
		}
		log.Printf("[DELHIVERY-CANCEL] ❌ Cancellation failed: %s", errMsg)
		return fmt.Errorf("cancellation failed: %s", errMsg)
	}

	log.Printf("[DELHIVERY-CANCEL] ✅ Shipment %s cancelled successfully", waybill)
	return nil
}

// FetchPackingSlip downloads the label/packing slip for a waybill.
//
// This exists because the packing-slip endpoint requires our API token in a
// header: the URL alone is useless to a browser, so handing it to the client
// (as the earlier implementation did) produced a 401 for the operator and
// would have leaked the token had it been attached. The document is fetched
// server-side instead and streamed from an authenticated MAK endpoint.
func (s *DelhiveryService) FetchPackingSlip(ctx context.Context, waybill string) ([]byte, string, error) {
	if strings.TrimSpace(waybill) == "" {
		return nil, "", fmt.Errorf("waybill is required to fetch a packing slip")
	}

	apiURL := fmt.Sprintf("%s/api/p/packing_slip?wbns=%s&pdf=true",
		s.config.BaseURL, url.QueryEscape(waybill))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create packing slip request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))
	req.Header.Set("Accept", "application/pdf, application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch packing slip: %w", err)
	}
	defer resp.Body.Close()

	// Bounded read: a proxy error page must not be able to exhaust memory.
	const maxSlipBytes = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSlipBytes))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read packing slip: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("packing slip request returned %d", resp.StatusCode)
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("packing slip was empty")
	}

	contentType := resp.Header.Get("Content-Type")
	// Delhivery answers with JSON (carrying a nested PDF link or an error)
	// when it cannot render the slip directly. Reporting that as a PDF would
	// hand the operator a corrupt download.
	if strings.Contains(strings.ToLower(contentType), "json") {
		return nil, "", fmt.Errorf("delhivery did not return a printable slip: %s",
			strings.TrimSpace(string(body[:min(256, len(body))])))
	}
	if contentType == "" {
		contentType = "application/pdf"
	}
	return body, contentType, nil
}

// GenerateWaybill generates a waybill for future use (optional)
func (s *DelhiveryService) GenerateWaybill(count int) ([]string, error) {
	if count <= 0 {
		count = 1
	}
	if count > 50 {
		count = 50 // Max limit
	}

	apiURL := fmt.Sprintf("%s/waybill/api/bulk/json/?count=%d", s.config.BaseURL, count)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create waybill request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to generate waybill: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read waybill response: %w", err)
	}

	var waybillResp []string
	if err := json.Unmarshal(body, &waybillResp); err != nil {
		return nil, fmt.Errorf("failed to parse waybill response: %w, body: %s", err, string(body))
	}

	return waybillResp, nil
}

// RequestPickup schedules a pickup for shipments
func (s *DelhiveryService) RequestPickup(pickupDate string, pickupTime string, expectedPackages int) error {
	apiURL := fmt.Sprintf("%s/fm/request/new/", s.config.BaseURL)

	// Ensure pickup_time has a default value if not provided
	if pickupTime == "" {
		pickupTime = "10:00:00" // Default pickup time
	}

	// Ensure pickup_location is set
	pickupLocation := s.config.PickupLocation
	if pickupLocation == "" {
		log.Printf("[DELHIVERY-API] WARNING: PickupLocation is empty, this will cause pickup request to fail")
	}

	log.Printf("[DELHIVERY-API] RequestPickup - pickup_location: '%s', pickup_date: '%s', pickup_time: '%s', expected_package_count: %d",
		pickupLocation, pickupDate, pickupTime, expectedPackages)

	payload := map[string]interface{}{
		"pickup_location":        pickupLocation,
		"pickup_date":            pickupDate, // Format: YYYY-MM-DD
		"pickup_time":            pickupTime, // Format: HH:MM:SS (e.g., "10:00:00")
		"expected_package_count": expectedPackages,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal pickup request: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create pickup request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.config.APIToken))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request pickup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pickup request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// mapDelhiveryStatusToInternal maps Delhivery status types to internal order status
func mapDelhiveryStatusToInternal(statusType string) string {
	switch statusType {
	case "UD": // Undelivered
		return "undelivered"
	case "DL": // Delivered
		return "delivered"
	case "OT": // In Transit
		return "in_transit"
	case "RT": // RTO (Return to Origin)
		return "returned"
	case "PU": // Picked Up
		return "picked_up"
	case "OP": // Out for Pickup
		return "out_for_pickup"
	case "OD": // Out for Delivery
		return "out_for_delivery"
	case "IT": // In Transit
		return "in_transit"
	case "MD": // Manifested
		return "manifested"
	case "PP": // Pending Pickup
		return "pending_pickup"
	default:
		return "processing"
	}
}

// WebhookPayload represents the webhook data sent by Delhivery
type WebhookPayload struct {
	Waybill        string `json:"Waybill"`
	Status         string `json:"Status"`
	StatusType     string `json:"StatusType"`
	StatusDateTime string `json:"StatusDateTime"`
	StatusLocation string `json:"StatusLocation"`
	Instructions   string `json:"Instructions"`
	Remarks        string `json:"Remarks"`
	ClientName     string `json:"ClientName"`
	ReferenceNo    string `json:"ReferenceNo"`
	ExpectedDate   string `json:"ExpectedDate"`
	OTPPin         string `json:"OTPPin,omitempty"`
	SDD            string `json:"SDD,omitempty"`
	EDDUpdate      string `json:"EDD_Update,omitempty"`
}

// ParseWebhook parses the Delhivery webhook payload
func ParseWebhook(body []byte) (*WebhookPayload, error) {
	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse webhook payload: %w", err)
	}
	return &payload, nil
}

// GetOrderStatusFromWebhook maps webhook status to order status
func GetOrderStatusFromWebhook(statusType string) string {
	return mapDelhiveryStatusToInternal(statusType)
}
