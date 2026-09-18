// Package queue wraps the SQS client used to hand work to the worker
// function instead of doing it inside a request: shipment creation, and the
// fetching and storing of imported product images.
package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Message kinds. The worker reads Kind first and dispatches on it, so one
// queue carries every job type -- a second queue would mean a second event
// source mapping, IAM statement and DLQ to keep in step for no gain at this
// volume.
const (
	KindShipment    = "shipment"
	KindImageImport = "image_import"
)

// Envelope is the shape of every message on the queue.
type Envelope struct {
	Kind string `json:"kind"`
	// OrderID for KindShipment.
	OrderID string `json:"order_id,omitempty"`
	// JobID and Index for KindImageImport: which job, and which image in it.
	// The worker re-reads the job from Mongo rather than trusting a payload
	// carried through SQS, which is how a stale or oversized message is
	// avoided.
	JobID string `json:"job_id,omitempty"`
	Index int    `json:"index,omitempty"`
}

// Queue sends messages. A nil *Queue is a valid, expected value -- see New --
// and callers must check for it and do the work inline instead.
type Queue struct {
	client   *sqs.Client
	queueURL string
}

// New returns (nil, nil) when queueURL is empty -- the expected state for the
// non-Lambda entrypoint (cmd/api) and local development, where there is no
// worker to hand anything to.
func New(queueURL string) (*Queue, error) {
	if queueURL == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, err
	}
	return &Queue{client: sqs.NewFromConfig(cfg), queueURL: queueURL}, nil
}

// ShipmentQueue is the older name for Queue, kept so existing call sites read
// as they did.
type ShipmentQueue = Queue

// NewShipmentQueue is the older name for New.
func NewShipmentQueue(queueURL string) (*Queue, error) { return New(queueURL) }

// EnqueueShipment sends just the order ID; the consumer re-fetches the full
// order from Mongo.
func (q *Queue) EnqueueShipment(ctx context.Context, orderID string) error {
	return q.Send(ctx, Envelope{Kind: KindShipment, OrderID: orderID})
}

// Send puts one envelope on the queue.
func (q *Queue) Send(ctx context.Context, envelope Envelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.queueURL),
		MessageBody: aws.String(string(body)),
	})
	return err
}

// SendAll puts many envelopes on the queue, ten per request -- SQS's batch
// limit -- so a job of forty images is four round trips rather than forty.
//
// Returns the first failure and the number successfully queued before it, so
// a caller can record exactly which images will never be processed.
func (q *Queue) SendAll(ctx context.Context, envelopes []Envelope) (int, error) {
	sent := 0
	for start := 0; start < len(envelopes); start += 10 {
		end := start + 10
		if end > len(envelopes) {
			end = len(envelopes)
		}

		entries := make([]types.SendMessageBatchRequestEntry, 0, end-start)
		for i, envelope := range envelopes[start:end] {
			body, err := json.Marshal(envelope)
			if err != nil {
				return sent, err
			}
			entries = append(entries, types.SendMessageBatchRequestEntry{
				Id:          aws.String(fmt.Sprintf("m%d", start+i)),
				MessageBody: aws.String(string(body)),
			})
		}

		out, err := q.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
			QueueUrl: aws.String(q.queueURL),
			Entries:  entries,
		})
		if err != nil {
			return sent, err
		}
		sent += len(out.Successful)
		if len(out.Failed) > 0 {
			first := out.Failed[0]
			return sent, fmt.Errorf("queue refused %d message(s): %s", len(out.Failed), aws.ToString(first.Message))
		}
	}
	return sent, nil
}
