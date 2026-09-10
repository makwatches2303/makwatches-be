// Package shipment creates Delhivery shipments for committed orders. It is
// called from two places: synchronously from OrderHandler.Checkout when no
// SQS queue is configured (cmd/api, local dev), and from the SQS-triggered
// worker (cmd/shipment-worker) when one is. Neither caller depends on
// *fiber.Ctx -- this only ever touches config, the Delhivery client and Mongo.
package shipment

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/debuglog"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
)

// CreateDelhiveryShipment creates a shipment with Delhivery for the order and
// records the result (waybill or error) back onto the order document. It does
// not return an error: a Delhivery failure is logged and recorded on the
// order, matching the pre-Lambda synchronous behavior in Checkout, which
// never failed the checkout response over a Delhivery problem.
func CreateDelhiveryShipment(ctx context.Context, cfg *config.Config, delhiveryService *services.DelhiveryService, mongoDB *mongo.Database, order *models.Order) {
	// Use human-readable order number for logging
	orderDisplay := order.OrderNumber
	if orderDisplay == "" {
		orderDisplay = order.ID.Hex()
	}

	log.Printf("[DELHIVERY] ========== Starting shipment creation for order %s ==========", orderDisplay)

	// Check if Delhivery is configured
	if cfg.DelhiveryAPIToken == "" {
		log.Printf("[DELHIVERY] ERROR: API Token not configured! Skipping shipment for order %s", orderDisplay)
		log.Printf("[DELHIVERY] Please set DELHIVERY_API_TOKEN in your .env file")
		return
	}
	debuglog.Printf("[DELHIVERY] API Token configured: %s... (first 10 chars)\n", cfg.DelhiveryAPIToken[:min(10, len(cfg.DelhiveryAPIToken))])
	debuglog.Printf("[DELHIVERY] Base URL: %s\n", cfg.DelhiveryBaseURL)
	debuglog.Printf("[DELHIVERY] Pickup Location: %s\n", cfg.DelhiveryPickupLocation)

	// Build product description from order items
	var productNames []string
	totalQuantity := 0
	for _, item := range order.Items {
		productNames = append(productNames, item.ProductName)
		totalQuantity += item.Quantity
	}
	productDesc := strings.Join(productNames, ", ")
	if len(productDesc) > 200 {
		productDesc = productDesc[:197] + "..."
	}
	debuglog.Printf("[DELHIVERY] Product description: %s\n", productDesc)
	debuglog.Printf("[DELHIVERY] Total quantity: %d\n", totalQuantity)

	// Determine payment mode
	paymentMode := "Prepaid"
	codAmount := 0.0
	if order.PaymentInfo.Method == "cod" {
		paymentMode = "COD"
		codAmount = order.Total
	}
	debuglog.Printf("[DELHIVERY] Payment mode: %s, COD Amount: %.2f\n", paymentMode, codAmount)

	// Get customer details
	customerName := order.CustomerName
	if customerName == "" {
		customerName = order.ShippingAddress.Name
	}
	if customerName == "" {
		customerName = "Customer"
	}

	customerPhone := order.CustomerPhone
	if customerPhone == "" {
		customerPhone = order.ShippingAddress.Phone
	}

	// Get city and state from shipping address (ensure correct values are used)
	customerCity := strings.TrimSpace(order.ShippingAddress.City)
	customerState := strings.TrimSpace(order.ShippingAddress.State)
	customerPincode := strings.TrimSpace(order.ShippingAddress.ZipCode)
	customerAddress := strings.TrimSpace(order.ShippingAddress.Street)
	customerCountry := strings.TrimSpace(order.ShippingAddress.Country)
	if customerCountry == "" {
		customerCountry = "India"
	}

	debuglog.Printf("[DELHIVERY] Customer Name: %s\n", customerName)
	debuglog.Printf("[DELHIVERY] Customer Phone: %s\n", customerPhone)
	debuglog.Printf("[DELHIVERY] Customer Address: %s\n", customerAddress)
	debuglog.Printf("[DELHIVERY] Customer City: %s, State: %s, Pincode: %s\n", customerCity, customerState, customerPincode)

	// Use human-readable order number for Delhivery
	orderRef := order.OrderNumber
	if orderRef == "" {
		// Fallback to ObjectID if OrderNumber not set
		orderRef = order.ID.Hex()
	}
	debuglog.Printf("[DELHIVERY] Order Reference: %s\n", orderRef)

	// Create shipment request with all details
	req := services.CreateShipmentRequest{
		CustomerName:    customerName,
		CustomerPhone:   customerPhone,
		CustomerEmail:   order.CustomerEmail,
		CustomerAddress: customerAddress,
		CustomerCity:    customerCity,
		CustomerState:   customerState,
		CustomerPincode: customerPincode,
		CustomerCountry: customerCountry,
		OrderID:         orderRef, // Use human-readable order number
		OrderDate:       order.CreatedAt.Format("2006-01-02"),
		TotalAmount:     order.Total,
		PaymentMode:     paymentMode,
		CODAmount:       codAmount,
		ProductQuantity: totalQuantity,
		ProductDesc:     productDesc,
		// Default package dimensions for watches
		Weight:  500, // 500 grams
		Length:  15,  // 15 cm
		Breadth: 10,  // 10 cm
		Height:  8,   // 8 cm
	}

	// Create items list
	for _, item := range order.Items {
		req.Items = append(req.Items, services.ShipmentItem{
			Name:     item.ProductName,
			SKU:      item.ProductID.Hex(),
			Quantity: item.Quantity,
			Price:    item.Price,
		})
	}

	debuglog.Printf("[DELHIVERY] Request prepared with %d items, total amount: %.2f\n", len(req.Items), req.TotalAmount)
	debuglog.Printf("[DELHIVERY] Calling Delhivery API...\n")

	// Call Delhivery API
	shipmentResp, err := delhiveryService.CreateShipment(req)

	orderCollection := mongoDB.Collection("orders")

	if err != nil {
		log.Printf("[DELHIVERY] ERROR: Failed to create shipment for order %s: %v", orderDisplay, err)

		// Update order with error info
		_, updateErr := orderCollection.UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{
			"$set": bson.M{
				"shipping_info": models.ShippingInfo{
					Provider:          "delhivery",
					ShipmentError:     err.Error(),
					RetryCount:        1,
					ShipmentCreatedAt: time.Now(),
				},
				"updated_at": time.Now(),
			},
		})
		if updateErr != nil {
			log.Printf("[DELHIVERY] ERROR: Failed to update order with error info: %v", updateErr)
		} else {
			debuglog.Printf("[DELHIVERY] Order updated with error info\n")
		}
		log.Printf("[DELHIVERY] ========== Shipment creation FAILED for order %s ==========", orderDisplay)
		return
	}

	log.Printf("[DELHIVERY] SUCCESS: Waybill received: %s", shipmentResp.Waybill)

	// Update order with successful shipping info
	trackingURL := fmt.Sprintf("https://www.delhivery.com/track/package/%s", shipmentResp.Waybill)
	_, updateErr := orderCollection.UpdateOne(ctx, bson.M{"_id": order.ID}, bson.M{
		"$set": bson.M{
			"shipping_info": models.ShippingInfo{
				Provider:          "delhivery",
				Waybill:           shipmentResp.Waybill,
				TrackingURL:       trackingURL,
				ShipmentStatus:    "manifested",
				ShipmentCreatedAt: time.Now(),
				LastStatusUpdate:  time.Now(),
			},
			"updated_at": time.Now(),
		},
	})
	if updateErr != nil {
		log.Printf("[DELHIVERY] ERROR: Failed to update order with shipping info: %v", updateErr)
		return
	}

	debuglog.Printf("[DELHIVERY] SUCCESS: Order %s updated with waybill %s\n", orderDisplay, shipmentResp.Waybill)
	debuglog.Printf("[DELHIVERY] Tracking URL: %s\n", trackingURL)
	log.Printf("[DELHIVERY] ========== Shipment creation COMPLETED for order %s ==========", orderDisplay)
}
