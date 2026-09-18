package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/correlation"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
)

// The queue's own maximum, so a poll costs one request whether it returns ten
// messages or none.
const (
	receiveBatch    = 10
	receiveWaitTime = 20
)

// Submitter is the one use case both transports share, so an operation
// that arrives on the queue gets the guarantees the HTTP one gets.
type Submitter interface {
	Submit(ctx context.Context, p wageringapp.SubmitParams) (wageringapp.Result, error)
}

// envelope is the message shape. Money is decoded by the domain type, so a
// malformed amount is refused at the same boundary as over HTTP (A.3.5).
type envelope struct {
	MessageID string `json:"messageId"`
	Type      string `json:"type"`
	// Optional: a producer that traces its own call can hand us its id, and
	// otherwise one is generated.
	CorrelationID string `json:"correlationId"`
	Data          struct {
		ProviderID                     string      `json:"providerId"`
		ExternalTransactionID          string      `json:"externalTransactionId"`
		IdempotencyKey                 string      `json:"idempotencyKey"`
		PlayerID                       string      `json:"playerId"`
		WalletID                       string      `json:"walletId"`
		RoundID                        string      `json:"roundId"`
		GameID                         string      `json:"gameId"`
		Kind                           string      `json:"kind"`
		Money                          money.Money `json:"money"`
		ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
	} `json:"data"`
}

type Consumer struct {
	client   *awssqs.Client
	queueURL string
	dlqURL   string
	submit   Submitter
	logger   *slog.Logger

	stop chan struct{}
	done chan struct{}
}

func NewConsumer(lc fx.Lifecycle, client *awssqs.Client, submit Submitter, cfg config.Config, logger *slog.Logger) *Consumer {
	c := &Consumer{
		client:   client,
		queueURL: cfg.WagerQueueURL,
		dlqURL:   cfg.WagerDLQURL,
		submit:   submit,
		logger:   logger.With(slog.String("component", "wager-consumer")),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	// Two contexts, because the brief asks for two things on SIGTERM. Cancelling
	// fetchCtx stops the long poll at once; workCtx keeps the handling in flight
	// alive until the shutdown deadline, and cancelling it releases the
	// message's visibility for redelivery instead of half-finishing it.
	fetchCtx, stopFetching := context.WithCancel(context.Background())
	workCtx, abandonWork := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go c.run(fetchCtx, workCtx)
			c.logger.Info("wager consumer started", slog.String("queue", cfg.WagerQueueURL))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			close(c.stop)
			stopFetching()

			// Bounded by the worker's own share, not by the whole shutdown: the
			// hooks after this one — the HTTP server, the pool — have their own
			// in-flight work to finish.
			drain, giveUp := context.WithTimeout(ctx, cfg.WorkerDrainTimeout)
			defer giveUp()

			select {
			case <-c.done:
				c.logger.Info("wager consumer stopped")
			case <-drain.Done():
				abandonWork()
				<-c.done
				c.logger.Warn("wager consumer stopped past its deadline, in-flight messages released")
			}
			abandonWork()
			return nil
		},
	})

	return c
}

func (c *Consumer) run(fetchCtx, workCtx context.Context) {
	defer close(c.done)

	for {
		select {
		case <-c.stop:
			return
		default:
		}

		messages, err := c.receive(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				return
			}
			c.logger.Error("receive failed", slog.Any("error", err))
			continue
		}
		for _, m := range messages {
			c.handle(workCtx, m)
		}
	}
}

func (c *Consumer) receive(ctx context.Context) ([]types.Message, error) {
	out, err := c.client.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:            &c.queueURL,
		MaxNumberOfMessages: receiveBatch,
		WaitTimeSeconds:     receiveWaitTime,
		// Needed to re-group a message on the dead-letter queue, which is FIFO
		// and therefore demands one.
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameMessageGroupId,
		},
	})
	if err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// handle applies the three outcomes. The message is deleted only after its
// handling has committed; a permanent error goes to the dead-letter queue at
// once; a transient one is left for the visibility timeout to redeliver, and
// the redrive policy catches it if the attempts run out.
func (c *Consumer) handle(ctx context.Context, m types.Message) {
	received := time.Now().UTC()
	sqsID := derefString(m.MessageId)

	params, correlationID, err := decode(m)
	ctx, _ = correlation.Ensure(ctx, correlationID)
	if err != nil {
		// Nothing about this body will parse on the fourth attempt either, and
		// there is no transaction to reject: it is an invalid message.
		c.deadLetter(ctx, m, sqsID, "message is not a valid operation", err)
		return
	}
	params.Inbox.ReceivedAt = received

	logger := c.logger.With(slog.String("messageId", params.Inbox.MessageID))

	result, err := c.submit.Submit(ctx, params)
	switch {
	case err == nil:
	case permanent(err):
		c.deadLetter(ctx, m, params.Inbox.MessageID, "message can never be handled", err)
		return
	default:
		logger.WarnContext(ctx, "handling failed, leaving the message for redelivery", slog.Any("error", err))
		return
	}

	// A rejection is a confirmed outcome and a pending reference is durably
	// recorded, so both are done with the queue: 13's worker owns what is left.
	logger.InfoContext(ctx, "message handled",
		slog.String("transactionId", result.Transaction.ID().String()),
		slog.String("walletId", result.Transaction.WalletID().String()),
		slog.String("status", result.Transaction.Status().String()),
		slog.Bool("idempotentReplay", result.Replay))

	c.delete(ctx, m, logger)
}

// permanent reports the errors no redelivery can change: a request the domain
// refuses outright, and a message whose identity collides with content that was
// already handled.
func permanent(err error) bool {
	return wagering.IsRefusal(err) ||
		errors.Is(err, wageringapp.ErrUnsupportedKind) ||
		errors.Is(err, wageringapp.ErrMessageConflict) ||
		errors.Is(err, wageringapp.ErrPayloadConflict) ||
		errors.Is(err, wageringapp.ErrExternalIDConflict)
}

// deadLetter moves a message the consumer can never handle off the queue
// itself, rather than letting it hold up its message group for
// maxReceiveCount × VisibilityTimeout on retries that cannot succeed.
// The redrive policy stays as the backstop for everything else.
func (c *Consumer) deadLetter(ctx context.Context, m types.Message, id, reason string, cause error) {
	logger := c.logger.With(slog.String("messageId", id), slog.Any("error", cause))

	body := derefString(m.Body)
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = id
	}
	if _, err := c.client.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               &c.dlqURL,
		MessageBody:            &body,
		MessageGroupId:         &group,
		MessageDeduplicationId: &id,
	}); err != nil {
		// Left on the queue: the redrive policy will take it after its attempts.
		logger.ErrorContext(ctx, "could not dead-letter the message, leaving it to the redrive policy",
			slog.Any("send_error", err))
		return
	}

	metrics.DeadLettered.Add(1)
	logger.ErrorContext(ctx, reason+", moved to the dead-letter queue")
	c.delete(ctx, m, logger)
}

func (c *Consumer) delete(ctx context.Context, m types.Message, logger *slog.Logger) {
	if _, err := c.client.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl: &c.queueURL, ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		// The handling committed; the inbox makes the redelivery a no-op.
		logger.ErrorContext(ctx, "delete failed, the message will be redelivered", slog.Any("error", err))
	}
}

// decode refuses what NewExternal could not have accepted anyway, so a message
// missing a field never becomes a transaction there is no way to reject.
func decode(m types.Message) (wageringapp.SubmitParams, string, error) {
	var e envelope
	if err := json.Unmarshal([]byte(derefString(m.Body)), &e); err != nil {
		return wageringapp.SubmitParams{}, "", fmt.Errorf("decode body: %w", err)
	}
	if e.MessageID == "" {
		return wageringapp.SubmitParams{}, e.CorrelationID, errors.New("messageId is required")
	}

	playerID, err := uuid.Parse(e.Data.PlayerID)
	if err != nil {
		return wageringapp.SubmitParams{}, e.CorrelationID, fmt.Errorf("playerId: %w", err)
	}
	walletID, err := uuid.Parse(e.Data.WalletID)
	if err != nil {
		return wageringapp.SubmitParams{}, e.CorrelationID, fmt.Errorf("walletId: %w", err)
	}
	if e.Data.IdempotencyKey == "" {
		return wageringapp.SubmitParams{}, e.CorrelationID, errors.New("idempotencyKey is required")
	}

	return wageringapp.SubmitParams{
		ProviderID:                     e.Data.ProviderID,
		ExternalTransactionID:          e.Data.ExternalTransactionID,
		IdempotencyKey:                 e.Data.IdempotencyKey,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        e.Data.RoundID,
		GameID:                         e.Data.GameID,
		Kind:                           wagering.Kind(e.Data.Kind),
		Money:                          e.Data.Money,
		ReferenceExternalTransactionID: e.Data.ReferenceExternalTransactionID,
		Inbox:                          wageringapp.Inbox{MessageID: e.MessageID},
	}, e.CorrelationID, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
