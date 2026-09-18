// Command shipment-worker consumes the project's SQS queue. It began as the
// Delhivery shipment consumer and keeps that name; it now also copies
// imported product images into storage, one message per image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/internal/queue"
	"github.com/shivam-mishra-20/mak-watches-be/internal/scrape"
	"github.com/shivam-mishra-20/mak-watches-be/internal/services"
	"github.com/shivam-mishra-20/mak-watches-be/internal/shipment"
)

// Cold-start init: Mongo, Delhivery, and -- lazily -- Firebase Storage for
// image jobs. No Fiber, no Redis.
var (
	mongoDB          *mongo.Database
	delhiveryService *services.DelhiveryService
	cfg              *config.Config
	storageProvider  *firebase.Provider
	fetcher          *scrape.Fetcher
)

func init() {
	ctx := context.Background()

	// Same as the API function: tell the collector the function's real size,
	// or it sizes the heap from the host and is killed instead of collecting.
	// Image decoding is the one thing here that allocates by the ten megabytes.
	if size := os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE"); size != "" {
		if mb, err := strconv.Atoi(size); err == nil && mb > 0 {
			debug.SetMemoryLimit(int64(float64(mb) * 0.8 * 1024 * 1024))
		}
	}

	// Firebase credentials come from Secrets Manager, exactly as in the API
	// function (see cmd/lambda/main.go for why they are not a plain env var).
	if secretARN := os.Getenv("FIREBASE_SECRET_ARN"); secretARN != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			log.Fatal("Failed to load AWS SDK config for Secrets Manager: ", err)
		}
		out, err := secretsmanager.NewFromConfig(awsCfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: &secretARN,
		})
		if err != nil {
			log.Fatal("Failed to fetch Firebase credentials from Secrets Manager: ", err)
		}
		if out.SecretString != nil {
			os.Setenv("FIREBASE_CREDENTIALS_JSON", *out.SecretString)
		}
	}

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

	storageProvider = firebase.NewProvider(cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
	fetcher = scrape.NewFetcher()
}

// handleRequest is invoked with a batch of SQS records. The event source
// mapping (see deploy/aws/deploy.sh) uses batch size 1, so len(Records) is
// always 1 here in practice -- deliberately, so a failure is one message's and
// the queue's own retry and dead-letter handling apply to it alone. It also
// means an import of forty images fans out to forty concurrent invocations,
// which is where the speed comes from.
func handleRequest(ctx context.Context, sqsEvent events.SQSEvent) error {
	for _, record := range sqsEvent.Records {
		var envelope queue.Envelope
		if err := json.Unmarshal([]byte(record.Body), &envelope); err != nil {
			log.Printf("[WORKER] malformed message body, dropping: %v", err)
			continue // will never parse on retry either -- ack it, not a transient failure
		}

		// Messages written before Kind existed carry only an order id.
		if envelope.Kind == "" && envelope.OrderID != "" {
			envelope.Kind = queue.KindShipment
		}

		var err error
		switch envelope.Kind {
		case queue.KindShipment:
			err = handleShipment(ctx, envelope)
		case queue.KindImageImport:
			err = handleImageImport(ctx, envelope)
		default:
			log.Printf("[WORKER] unknown message kind %q, dropping", envelope.Kind)
			continue
		}
		if err != nil {
			return err // transient -- SQS retries, then dead-letters after 3 receives
		}
	}
	return nil
}

func handleShipment(ctx context.Context, envelope queue.Envelope) error {
	orderID, err := primitive.ObjectIDFromHex(envelope.OrderID)
	if err != nil {
		log.Printf("[SHIPMENT_WORKER] invalid order_id %q, dropping: %v", envelope.OrderID, err)
		return nil
	}

	var order models.Order
	if err := mongoDB.Collection("orders").FindOne(ctx, bson.M{"_id": orderID}).Decode(&order); err != nil {
		log.Printf("[SHIPMENT_WORKER] order %s not found (yet?): %v", envelope.OrderID, err)
		return err
	}

	// Best-effort, single attempt -- matches the pre-existing synchronous
	// behavior: a Delhivery-side failure is recorded on the order and
	// logged, not retried automatically.
	shipment.CreateDelhiveryShipment(ctx, cfg, delhiveryService, mongoDB, &order)
	return nil
}

// handleImageImport fetches, checks and stores one image of a job.
//
// Idempotent on purpose: SQS delivers at least once, so a message can arrive
// twice, and an image already marked stored is left alone. A download or
// decode failure is recorded on the image and the message acknowledged -- the
// far end will answer the same way on a retry. Only Mongo being unreachable
// is returned as an error, since that is the one failure a retry fixes.
func handleImageImport(ctx context.Context, envelope queue.Envelope) error {
	jobID, err := primitive.ObjectIDFromHex(envelope.JobID)
	if err != nil {
		log.Printf("[IMPORT_WORKER] invalid job id %q, dropping", envelope.JobID)
		return nil
	}

	jobs := mongoDB.Collection("image_import_jobs")
	var job models.ImageImportJob
	if err := jobs.FindOne(ctx, bson.M{"_id": jobID}).Decode(&job); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			log.Printf("[IMPORT_WORKER] job %s not found, dropping", envelope.JobID)
			return nil
		}
		return fmt.Errorf("reading job %s: %w", envelope.JobID, err)
	}
	if envelope.Index < 0 || envelope.Index >= len(job.Images) {
		log.Printf("[IMPORT_WORKER] job %s has no image %d, dropping", envelope.JobID, envelope.Index)
		return nil
	}
	image := job.Images[envelope.Index]
	if image.Status != models.ImportImagePending {
		return nil
	}

	mark := func(fields bson.M) error {
		set := bson.M{"updated_at": time.Now()}
		for key, value := range fields {
			set[fmt.Sprintf("images.%d.%s", envelope.Index, key)] = value
		}
		_, err := jobs.UpdateByID(ctx, jobID, bson.M{"$set": set})
		return err
	}

	client, err := storageProvider.Client(ctx)
	if err != nil {
		log.Printf("[IMPORT_WORKER] storage unavailable: %v", err)
		return mark(bson.M{"status": models.ImportImageFailed, "reason": "image storage is not configured"})
	}

	started := time.Now()
	stored, err := scrape.StoreImage(ctx, fetcher, client, image.SourceURL, job.SourceURL, image.ObjectName)
	if err != nil {
		log.Printf("[IMPORT_WORKER] job %s image %d: %v", envelope.JobID, envelope.Index, err)
		return mark(bson.M{"status": models.ImportImageFailed, "reason": err.Error()})
	}

	log.Printf("[IMPORT_WORKER] job %s image %d stored in %v (%dx%d)",
		envelope.JobID, envelope.Index, time.Since(started).Round(time.Millisecond), stored.Width, stored.Height)

	fields := bson.M{
		"status": models.ImportImageStored,
		"url":    stored.URL,
		"width":  stored.Width,
		"height": stored.Height,
	}
	if stored.Warning != "" {
		fields["warning"] = stored.Warning
	}
	return mark(fields)
}

func main() {
	lambda.Start(handleRequest)
}
