// Package events holds the integration events §11 requires: one concrete type
// per event, inside the envelope a consumer receives.
package events

import (
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
)

// One constant while every event is at its first shape; it becomes a value per
// type when one of them changes.
const version = 1

// Envelope is the published form. EventType and Version are set by the
// constructors and are not parameters, so no caller can mislabel an event (§11).
//
// AggregateID is the wallet for every event, including the transaction ones:
// §6.2 makes the wallet the root of the financial aggregate.
type Envelope struct {
	EventID       uuid.UUID  `json:"eventId"`
	EventType     string     `json:"eventType"`
	AggregateID   uuid.UUID  `json:"aggregateId"`
	CorrelationID uuid.UUID  `json:"correlationId"`
	CausationID   *uuid.UUID `json:"causationId,omitempty"`
	OccurredAt    time.Time  `json:"occurredAt"`
	Version       int        `json:"version"`
	Data          any        `json:"data"`
}

// The external identifiers are absent on an OPENING, which has no provider
// (A.1), so they are omitted rather than published empty.
type wagerTransaction struct {
	TransactionID         uuid.UUID   `json:"transactionId"`
	WalletID              uuid.UUID   `json:"walletId"`
	PlayerID              uuid.UUID   `json:"playerId"`
	Kind                  string      `json:"kind"`
	Money                 money.Money `json:"money"`
	ProviderID            string      `json:"providerId,omitempty"`
	ExternalTransactionID string      `json:"externalTransactionId,omitempty"`
	RoundID               string      `json:"roundId,omitempty"`
	GameID                string      `json:"gameId,omitempty"`
}

type WagerTransactionProcessed struct {
	wagerTransaction
}

type WagerTransactionRejected struct {
	wagerTransaction
	FailureCode string `json:"failureCode"`
}

type WagerTransactionPendingReference struct {
	wagerTransaction
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

type WalletBalanceChanged struct {
	WalletID      uuid.UUID   `json:"walletId"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
}

func NewWagerTransactionProcessed(eventID, correlationID uuid.UUID, t *wagering.WagerTransaction, occurredAt time.Time) Envelope {
	return envelope(eventID, correlationID, TypeWagerTransactionProcessed, t.WalletID(), occurredAt,
		WagerTransactionProcessed{snapshot(t)})
}

func NewWagerTransactionRejected(eventID, correlationID uuid.UUID, t *wagering.WagerTransaction, occurredAt time.Time) Envelope {
	return envelope(eventID, correlationID, TypeWagerTransactionRejected, t.WalletID(), occurredAt,
		WagerTransactionRejected{snapshot(t), t.FailureCode().String()})
}

func NewWagerTransactionPendingReference(eventID, correlationID uuid.UUID, t *wagering.WagerTransaction, occurredAt time.Time) Envelope {
	return envelope(eventID, correlationID, TypeWagerTransactionPendingReference, t.WalletID(), occurredAt,
		WagerTransactionPendingReference{snapshot(t), t.ReferenceExternalTransactionID()})
}

func NewWalletBalanceChanged(eventID, correlationID uuid.UUID, entry *wallet.LedgerEntry, walletVersion int64, occurredAt time.Time) Envelope {
	return envelope(eventID, correlationID, TypeWalletBalanceChanged, entry.WalletID(), occurredAt,
		WalletBalanceChanged{
			WalletID:      entry.WalletID(),
			TransactionID: entry.TransactionID(),
			Direction:     entry.Direction().String(),
			Money:         entry.Amount(),
			BalanceBefore: entry.BalanceBefore(),
			BalanceAfter:  entry.BalanceAfter(),
			WalletVersion: walletVersion,
		})
}

// The domain stores timestamps as given and never converts (03 §9), so UTC is
// applied here, where §11 requires it.
func envelope(eventID, correlationID uuid.UUID, eventType string, aggregateID uuid.UUID, occurredAt time.Time, data any) Envelope {
	return Envelope{
		EventID:       eventID,
		EventType:     eventType,
		AggregateID:   aggregateID,
		CorrelationID: correlationID,
		OccurredAt:    occurredAt.UTC(),
		Version:       version,
		Data:          data,
	}
}

// Copies out of the aggregate, so a later transition cannot alter a payload
// already written (§11).
func snapshot(t *wagering.WagerTransaction) wagerTransaction {
	return wagerTransaction{
		TransactionID:         t.ID(),
		WalletID:              t.WalletID(),
		PlayerID:              t.PlayerID(),
		Kind:                  t.Kind().String(),
		Money:                 t.Amount(),
		ProviderID:            t.ProviderID(),
		ExternalTransactionID: t.ExternalTransactionID(),
		RoundID:               t.RoundID(),
		GameID:                t.GameID(),
	}
}
