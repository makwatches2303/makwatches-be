// Package queue wraps the SQS client used to enqueue Delhivery
// shipment-creation work instead of the caller doing it synchronously.
package queue

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// ShipmentQueue enqueues shipment-creation messages. A nil *ShipmentQueue is
// a valid, expected value -- see NewShipmentQueue -- and callers must check
// for it and fall back to the synchronous shipment.CreateDelhiveryShipment
// call, exactly as OrderHandler.Checkout does.
type ShipmentQueue struct {
	client   *sqs.Client
	queueURL string
}

// NewShipmentQueue returns (nil, nil) when queueURL is empty -- the expected
// state for the non-Lambda entrypoint (cmd/api) and local development.
func NewShipmentQueue(queueURL string) (*ShipmentQueue, error) {
	if queueURL == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, err
	}
	return &ShipmentQueue{client: sqs.NewFromConfig(cfg), queueURL: queueURL}, nil
}

type shipmentMessage struct {
	OrderID string `json:"order_id"`
}

// EnqueueShipment sends just the order ID; the consumer re-fetches the full
// order from Mongo rather than trusting a serialized payload passed through
// SQS (avoids staleness and SQS's message-size limit).
func (q *ShipmentQueue) EnqueueShipment(ctx context.Context, orderID string) error {
	body, err := json.Marshal(shipmentMessage{OrderID: orderID})
	if err != nil {
		return err
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.queueURL),
		MessageBody: aws.String(string(body)),
	})
	return err
}
