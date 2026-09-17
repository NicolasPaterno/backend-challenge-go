package events_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()

	parsed, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return parsed
}

func bet(t *testing.T, amount string) *wagering.WagerTransaction {
	t.Helper()

	built, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                    uuid.NewV7(),
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PayloadHash:           "hash",
		WalletID:              uuid.NewV7(),
		PlayerID:              uuid.NewV7(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  wagering.KindBet,
		Amount:                brl(t, amount),
		Now:                   time.Now(),
	})
	if err != nil {
		t.Fatalf("NewExternal() error = %v", err)
	}
	return built
}

func decode(t *testing.T, e events.Envelope) map[string]any {
	t.Helper()

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return object(t, raw)
}

// UseNumber, so a decoded number never becomes a float64 — which
// TestInternalContainsNoFloat refuses, rightly (§5.1, A.3.4).
func object(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	return decoded
}

func TestEnvelopeCarriesEveryRequiredField(t *testing.T) {
	transaction := bet(t, "25.00")
	eventID, correlationID := uuid.NewV7(), uuid.NewV7()
	// A zone other than UTC, which §11 requires the envelope to normalise.
	occurredAt := time.Date(2026, 9, 8, 9, 0, 0, 0, time.FixedZone("BRT", -3*60*60))

	envelope := events.NewWagerTransactionProcessed(eventID, correlationID, transaction, occurredAt)
	if envelope.EventType != events.TypeWagerTransactionProcessed {
		t.Errorf("eventType = %q, want %q", envelope.EventType, events.TypeWagerTransactionProcessed)
	}
	if envelope.Version != 1 {
		t.Errorf("version = %d, want 1", envelope.Version)
	}
	// §6.2: the wallet is the aggregate root, not the transaction.
	if envelope.AggregateID != transaction.WalletID() {
		t.Errorf("aggregateId = %s, want the wallet %s", envelope.AggregateID, transaction.WalletID())
	}
	if envelope.OccurredAt.Location() != time.UTC {
		t.Errorf("occurredAt is in %s, want UTC", envelope.OccurredAt.Location())
	}

	decoded := decode(t, envelope)
	for _, field := range []string{"eventId", "eventType", "aggregateId", "correlationId", "occurredAt", "version", "data"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("the envelope has no %q", field)
		}
	}
	if _, ok := decoded["causationId"]; ok {
		t.Error("causationId is optional and nothing set one, so it must be absent")
	}
	if got := decoded["occurredAt"]; got != "2026-09-08T12:00:00Z" {
		t.Errorf("occurredAt = %v, want the RFC 3339 UTC 2026-09-08T12:00:00Z", got)
	}

	data, _ := decoded["data"].(map[string]any)
	for _, field := range []string{"transactionId", "walletId", "playerId", "kind", "money", "providerId", "externalTransactionId", "roundId", "gameId"} {
		if _, ok := data[field]; !ok {
			t.Errorf("data has no %q", field)
		}
	}
}

// §11 names the payload's fields, so a rename breaks a consumer.
func TestWalletBalanceChangedCarriesTheMovement(t *testing.T) {
	w, err := wallet.New(uuid.NewV7(), uuid.NewV7(), brl(t, "100.00"), time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	entry, err := w.Debit(uuid.NewV7(), uuid.NewV7(), brl(t, "25.00"), time.Now())
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}

	decoded := decode(t, events.NewWalletBalanceChanged(uuid.NewV7(), uuid.NewV7(), entry, w.Version(), time.Now()))
	data, _ := decoded["data"].(map[string]any)
	for _, field := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, ok := data[field]; !ok {
			t.Errorf("data has no %q", field)
		}
	}
	if got := data["direction"]; got != "DEBIT" {
		t.Errorf("direction = %v, want DEBIT", got)
	}
	if got := data["walletVersion"]; got != json.Number("2") {
		t.Errorf("walletVersion = %v, want 2", got)
	}
	if got := data["balanceAfter"]; !sameMoney(got, "75.00") {
		t.Errorf("balanceAfter = %v, want 75.00 BRL", got)
	}
}

func sameMoney(value any, amount string) bool {
	m, ok := value.(map[string]any)
	return ok && m["amount"] == amount && m["currency"] == "BRL"
}

// The payload is a snapshot consumers compare, so the same amount spelled two
// ways must serialise to the same bytes (A.3.1).
func TestEqualAmountsSerialiseIdentically(t *testing.T) {
	eventID, correlationID := uuid.NewV7(), uuid.NewV7()
	occurredAt := time.Now()

	canonical, spelled := bet(t, "25.00"), bet(t, "25")
	first, err := json.Marshal(events.NewWagerTransactionProcessed(eventID, correlationID, canonical, occurredAt).Data)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	second, err := json.Marshal(events.NewWagerTransactionProcessed(eventID, correlationID, spelled, occurredAt).Data)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// The ids differ per transaction, so compare the rendering of the amount.
	if !sameMoney(field(t, first, "money"), "25.00") || !sameMoney(field(t, second, "money"), "25.00") {
		t.Errorf(`money rendered as %s and %s, want both "25.00"`, first, second)
	}
}

func field(t *testing.T, raw []byte, name string) any {
	t.Helper()

	return object(t, raw)[name]
}

// §11: the outbox payload is an immutable snapshot.
func TestPayloadDoesNotFollowLaterTransitions(t *testing.T) {
	transaction := bet(t, "25.00")
	envelope := events.NewWagerTransactionProcessed(uuid.NewV7(), uuid.NewV7(), transaction, time.Now())
	before, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if err := transaction.Reject(wagering.FailureInsufficientFunds, brl(t, "10.00"), time.Now()); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	after, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the payload changed with the aggregate:\n%s\n%s", before, after)
	}
}

func TestRejectedAndPendingReferenceCarryTheirReason(t *testing.T) {
	transaction := bet(t, "25.00")
	if err := transaction.Reject(wagering.FailureInsufficientFunds, brl(t, "10.00"), time.Now()); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	rejected := decode(t, events.NewWagerTransactionRejected(uuid.NewV7(), uuid.NewV7(), transaction, time.Now()))
	if rejected["eventType"] != events.TypeWagerTransactionRejected {
		t.Errorf("eventType = %v", rejected["eventType"])
	}
	data, _ := rejected["data"].(map[string]any)
	if got := data["failureCode"]; got != "INSUFFICIENT_FUNDS" {
		t.Errorf("failureCode = %v, want INSUFFICIENT_FUNDS", got)
	}

	pending := decode(t, events.NewWagerTransactionPendingReference(uuid.NewV7(), uuid.NewV7(), bet(t, "25.00"), time.Now()))
	if pending["eventType"] != events.TypeWagerTransactionPendingReference {
		t.Errorf("eventType = %v", pending["eventType"])
	}
	if _, ok := pending["data"].(map[string]any)["referenceExternalTransactionId"]; !ok {
		t.Error("data has no referenceExternalTransactionId")
	}
}
