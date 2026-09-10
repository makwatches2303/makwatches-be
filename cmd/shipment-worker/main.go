package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipment"
)

// Cold-start init: Mongo + Delhivery only. No Fiber, no Redis, no Firebase --
// this path never needs them, and dropping them keeps this function's IAM
// role and env-var footprint smaller than the API function's.
var (
	mongoDB          *mongo.Database
	delhiveryService *services.DelhiveryService
	cfg              *config.Config
)

func init() {
	var err error
	cfg, err = config.LoadConfig()
	if err != nil {
		log.Fatal("Failed to load configuration: ", err)
	}

	_, db, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatal("Cannot continue without database connection: ", err)
	}
	mongoDB = db

	delhiveryService = services.NewDelhiveryService(services.DelhiveryConfig{
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

type shipmentMessage struct {
	OrderID string `json:"order_id"`
}

// handleRequest is invoked with a batch of SQS records. The event source
// mapping (see deploy/aws/deploy.sh) uses batch size 1, so len(Records) is
// always 1 here in practice -- deliberately, to avoid needing SQS's
// partial-batch-failure reporting (ReportBatchItemFailures) for what is a
// low-volume queue.
func handleRequest(ctx context.Context, sqsEvent events.SQSEvent) error {
	for _, record := range sqsEvent.Records {
		var msg shipmentMessage
		if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
			log.Printf("[SHIPMENT_WORKER] malformed message body, dropping: %v", err)
			continue // will never parse on retry either -- ack it, not a transient failure
		}

		orderID, err := primitive.ObjectIDFromHex(msg.OrderID)
		if err != nil {
			log.Printf("[SHIPMENT_WORKER] invalid order_id %q, dropping: %v", msg.OrderID, err)
			continue
		}

		var order models.Order
		if err := mongoDB.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
			log.Printf("[SHIPMENT_WORKER] order %s not found (yet?): %v", msg.OrderID, err)
			return err // transient -- return the error so SQS retries, then DLQs after 3 receives
		}

		// Best-effort, single attempt -- matches the pre-existing synchronous
		// behavior: a Delhivery-side failure is recorded on the order and
		// logged, not retried automatically.
		shipment.CreateDelhiveryShipment(ctx, cfg, delhiveryService, mongoDB, &order)
	}
	return nil
}

func main() {
	lambda.Start(handleRequest)
}
