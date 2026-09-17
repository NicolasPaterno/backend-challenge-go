// Package sqs carries both directions of §10 and §11: the publisher that sends
// outbox events out, and the consumer that brings wager operations in.
package sqs

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/worker/outbox"
)

func NewClient(cfg config.Config) (*awssqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}
	return awssqs.NewFromConfig(awsCfg, func(o *awssqs.Options) {
		// Empty outside Compose and the tests, where the real endpoint applies.
		if cfg.SQSEndpoint != "" {
			o.BaseEndpoint = &cfg.SQSEndpoint
		}
	}), nil
}

type Publisher struct {
	client   *awssqs.Client
	queueURL string
}

func NewPublisher(client *awssqs.Client, cfg config.Config) *Publisher {
	return &Publisher{client: client, queueURL: cfg.EventsQueueURL}
}

// The group is the aggregate, so one wallet's events stay ordered while
// different wallets are delivered in parallel (§5.6). The deduplication id is
// the eventId, which makes a republication after a crash between send and
// confirmation a no-op inside the queue's dedup window (§11).
func (p *Publisher) Publish(ctx context.Context, e outbox.Event) error {
	body := string(e.Payload)
	group := e.AggregateID.String()
	dedup := e.EventID.String()

	_, err := p.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               &p.queueURL,
		MessageBody:            &body,
		MessageGroupId:         &group,
		MessageDeduplicationId: &dedup,
	})
	if err != nil {
		return fmt.Errorf("send event %s: %w", e.EventID, err)
	}
	return nil
}

var Module = fx.Module("sqs-adapter",
	fx.Provide(
		NewClient,
		fx.Annotate(NewPublisher, fx.As(new(outbox.Publisher))),
		NewConsumer,
	),
	fx.Invoke(func(*Consumer) {}),
)
