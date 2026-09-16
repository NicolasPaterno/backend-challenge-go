package wagering_test

import (
	"errors"
	"testing"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

var (
	txID      = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	walletID  = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID  = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	createdAt = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	changedAt = createdAt.Add(time.Hour)
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return m
}

func externalParams(kind wagering.Kind, amount money.Money) wagering.NewExternalParams {
	p := wagering.NewExternalParams{
		ID:                    txID,
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PayloadHash:           "9f2c",
		WalletID:              walletID,
		PlayerID:              playerID,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Amount:                amount,
		Now:                   createdAt,
	}
	if kind.IsReversal() {
		p.ReferenceExternalTransactionID = "transaction-100"
	}
	return p
}

func newExternal(t *testing.T, kind wagering.Kind, amount string) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.NewExternal(externalParams(kind, brl(t, amount)))
	if err != nil {
		t.Fatalf("NewExternal(%s, %s) error = %v", kind, amount, err)
	}
	return tx
}

func TestNewExternalStartsPending(t *testing.T) {
	tx := newExternal(t, wagering.KindBet, "25.00")

	if tx.Status() != wagering.StatusPending {
		t.Errorf("Status() = %s, want PENDING", tx.Status())
	}
	if tx.Origin() != wagering.OriginExternal {
		t.Errorf("Origin() = %s, want EXTERNAL", tx.Origin())
	}
	if tx.ReferenceTransactionID() != uuid.Nil() || tx.FailureCode() != "" {
		t.Errorf("new transaction carries a reference or failure code: %s / %s", tx.ReferenceTransactionID(), tx.FailureCode())
	}
	if !tx.CreatedAt().Equal(createdAt) || !tx.UpdatedAt().Equal(createdAt) {
		t.Errorf("timestamps = %s/%s, want %s", tx.CreatedAt(), tx.UpdatedAt(), createdAt)
	}
}

func TestNewExternalRejectsOpening(t *testing.T) {
	_, err := wagering.NewExternal(externalParams(wagering.KindOpening, brl(t, "10.00")))

	if !errors.Is(err, wagering.ErrOpeningIsInternal) {
		t.Fatalf("NewExternal(OPENING) error = %v, want ErrOpeningIsInternal", err)
	}
}

func TestNewExternalRequiresEveryExternalField(t *testing.T) {
	blank := map[string]func(*wagering.NewExternalParams){
		"providerId":            func(p *wagering.NewExternalParams) { p.ProviderID = "" },
		"externalTransactionId": func(p *wagering.NewExternalParams) { p.ExternalTransactionID = "" },
		"idempotencyKey":        func(p *wagering.NewExternalParams) { p.IdempotencyKey = "" },
		"payloadHash":           func(p *wagering.NewExternalParams) { p.PayloadHash = "" },
		"roundId":               func(p *wagering.NewExternalParams) { p.RoundID = "" },
		"gameId":                func(p *wagering.NewExternalParams) { p.GameID = "" },
		"id":                    func(p *wagering.NewExternalParams) { p.ID = uuid.Nil() },
		"walletId":              func(p *wagering.NewExternalParams) { p.WalletID = uuid.Nil() },
		"playerId":              func(p *wagering.NewExternalParams) { p.PlayerID = uuid.Nil() },
		"now":                   func(p *wagering.NewExternalParams) { p.Now = time.Time{} },
		"money":                 func(p *wagering.NewExternalParams) { p.Amount = money.Money{} },
	}

	for name, blankOut := range blank {
		t.Run(name, func(t *testing.T) {
			p := externalParams(wagering.KindBet, brl(t, "25.00"))
			blankOut(&p)
			if _, err := wagering.NewExternal(p); !errors.Is(err, wagering.ErrUninitialized) {
				t.Errorf("NewExternal without %s error = %v, want ErrUninitialized", name, err)
			}
		})
	}
}

func TestNewExternalRejectsUnknownKind(t *testing.T) {
	_, err := wagering.NewExternal(externalParams(wagering.Kind("CASHOUT"), brl(t, "25.00")))

	if !errors.Is(err, wagering.ErrInvalidKind) {
		t.Fatalf("NewExternal(CASHOUT) error = %v, want ErrInvalidKind", err)
	}
}

// §7's amount policy per kind. The LOSS cases cover "0", "0.0" and "0.00":
// A.3.1 normalises all three to zero minor units, so the rule is IsZero and not
// a comparison against the literal "0.00".
func TestAmountPolicyPerKind(t *testing.T) {
	cases := []struct {
		kind    wagering.Kind
		amount  string
		wantErr error
	}{
		{wagering.KindBet, "25.00", nil},
		{wagering.KindBet, "0.00", wagering.ErrAmountNotPositive},
		{wagering.KindWin, "0.01", nil},
		{wagering.KindWin, "0", wagering.ErrAmountNotPositive},
		{wagering.KindRefund, "25.00", nil},
		{wagering.KindRefund, "0.00", wagering.ErrAmountNotPositive},
		{wagering.KindRollback, "25.00", nil},
		{wagering.KindRollback, "0.00", wagering.ErrAmountNotPositive},
		{wagering.KindLoss, "0.00", nil},
		{wagering.KindLoss, "0.0", nil},
		{wagering.KindLoss, "0", nil},
		{wagering.KindLoss, "0.01", wagering.ErrAmountNotZero},
		{wagering.KindLoss, "25.00", wagering.ErrAmountNotZero},
	}

	for _, c := range cases {
		t.Run(c.kind.String()+"/"+c.amount, func(t *testing.T) {
			_, err := wagering.NewExternal(externalParams(c.kind, brl(t, c.amount)))
			if !errors.Is(err, c.wantErr) {
				t.Errorf("NewExternal(%s, %s) error = %v, want %v", c.kind, c.amount, err, c.wantErr)
			}
		})
	}
}

func TestNegativeAmountIsRejected(t *testing.T) {
	negative, err := money.FromMinor(-100, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}

	for _, kind := range []wagering.Kind{wagering.KindBet, wagering.KindWin, wagering.KindLoss} {
		if _, err := wagering.NewExternal(externalParams(kind, negative)); err == nil {
			t.Errorf("NewExternal(%s, -1.00) error = nil, want a rejection", kind)
		}
	}
	if _, err := opening(negative, createdAt); !errors.Is(err, money.ErrNegativeAmount) {
		t.Errorf("NewInternalOpening(-1.00) error = %v, want ErrNegativeAmount", err)
	}
}

func TestNewInternalOpeningCarriesNoExternalMetadata(t *testing.T) {
	tx, err := opening(brl(t, "1000.00"), createdAt)
	if err != nil {
		t.Fatalf("NewInternalOpening error = %v", err)
	}

	if tx.Origin() != wagering.OriginInternal || tx.Kind() != wagering.KindOpening || tx.Status() != wagering.StatusPending {
		t.Errorf("got %s/%s/%s, want INTERNAL/OPENING/PENDING", tx.Origin(), tx.Kind(), tx.Status())
	}
	empty := map[string]string{
		"providerId":            tx.ProviderID(),
		"externalTransactionId": tx.ExternalTransactionID(),
		"idempotencyKey":        tx.IdempotencyKey(),
		"payloadHash":           tx.PayloadHash(),
		"roundId":               tx.RoundID(),
		"gameId":                tx.GameID(),
		"reference":             tx.ReferenceExternalTransactionID(),
	}
	for name, value := range empty {
		if value != "" {
			t.Errorf("OPENING carries %s = %q, want empty", name, value)
		}
	}
}

// §7 accepts zero as an initial balance; the use case (05) is what skips the
// OPENING in that case, so the domain must not refuse it.
func TestNewInternalOpeningAcceptsZero(t *testing.T) {
	zero, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatalf("Zero error = %v", err)
	}
	if _, err := opening(zero, createdAt); err != nil {
		t.Errorf("NewInternalOpening(0.00) error = %v, want nil", err)
	}
}

func TestNewInternalOpeningRejectsUninitialisedValues(t *testing.T) {
	amount := brl(t, "10.00")
	blank := map[string]func(*wagering.NewInternalOpeningParams){
		"id":       func(p *wagering.NewInternalOpeningParams) { p.ID = uuid.Nil() },
		"walletId": func(p *wagering.NewInternalOpeningParams) { p.WalletID = uuid.Nil() },
		"playerId": func(p *wagering.NewInternalOpeningParams) { p.PlayerID = uuid.Nil() },
		"money":    func(p *wagering.NewInternalOpeningParams) { p.Amount = money.Money{} },
		"now":      func(p *wagering.NewInternalOpeningParams) { p.Now = time.Time{} },
	}

	for name, blankOut := range blank {
		p := wagering.NewInternalOpeningParams{ID: txID, WalletID: walletID, PlayerID: playerID, Amount: amount, Now: createdAt}
		blankOut(&p)
		if _, err := wagering.NewInternalOpening(p); !errors.Is(err, wagering.ErrUninitialized) {
			t.Errorf("NewInternalOpening without %s error = %v, want ErrUninitialized", name, err)
		}
	}
}

// The full matrix: every start state against every target, so a forbidden edge
// cannot be added by accident (§6.3).
func TestTransitionMatrix(t *testing.T) {
	// PENDING is only a start state: no method targets it, so it is not in
	// the target list. TestPendingIsUnreachable covers that separately.
	from := []wagering.Status{
		wagering.StatusPending,
		wagering.StatusPendingReference,
		wagering.StatusProcessed,
		wagering.StatusRejected,
		wagering.StatusFailed,
	}
	to := from[1:]
	permitted := map[wagering.Status]map[wagering.Status]bool{
		wagering.StatusPending: {
			wagering.StatusPendingReference: true,
			wagering.StatusProcessed:        true,
			wagering.StatusRejected:         true,
			wagering.StatusFailed:           true,
		},
		wagering.StatusPendingReference: {
			wagering.StatusProcessed: true,
			wagering.StatusRejected:  true,
			wagering.StatusFailed:    true,
		},
		wagering.StatusProcessed: {},
		wagering.StatusRejected:  {},
		wagering.StatusFailed:    {},
	}

	for _, from := range from {
		for _, to := range to {
			t.Run(from.String()+"->"+to.String(), func(t *testing.T) {
				tx := rehydrated(t, from)
				err := move(t, tx, to)

				if want := permitted[from][to]; want != (err == nil) {
					t.Fatalf("%s -> %s error = %v, permitted = %v", from, to, err, want)
				}
				if err != nil {
					if !errors.Is(err, wagering.ErrInvalidTransition) {
						t.Fatalf("error = %v, want ErrInvalidTransition", err)
					}
					if tx.Status() != from {
						t.Errorf("refused transition changed status to %s", tx.Status())
					}
					return
				}
				if tx.Status() != to {
					t.Errorf("Status() = %s, want %s", tx.Status(), to)
				}
				if !tx.UpdatedAt().Equal(changedAt) {
					t.Errorf("UpdatedAt() = %s, want %s", tx.UpdatedAt(), changedAt)
				}
			})
		}
	}
}

// Nothing returns a transaction to PENDING: the constructors are the only code
// that sets it, so a resumed PENDING row is the one the interrupted instance
// committed (§6.3).
func TestPendingIsUnreachable(t *testing.T) {
	for _, status := range []wagering.Status{wagering.StatusPendingReference, wagering.StatusProcessed, wagering.StatusRejected, wagering.StatusFailed} {
		tx := rehydrated(t, status)
		for _, attempt := range []func() error{
			func() error { return tx.MarkPendingReference(changedAt) },
			func() error { return tx.MarkProcessed(brl(t, "1.00"), changedAt) },
			func() error { return tx.Reject(wagering.FailureInsufficientFunds, money.Money{}, changedAt) },
			func() error { return tx.Fail(wagering.FailureInternalError, changedAt) },
		} {
			_ = attempt()
			if tx.Status() == wagering.StatusPending {
				t.Fatalf("a transition from %s landed back in PENDING", status)
			}
		}
	}
}

func TestTerminalTransactionStaysTerminal(t *testing.T) {
	for _, status := range []wagering.Status{wagering.StatusProcessed, wagering.StatusRejected, wagering.StatusFailed} {
		if !status.IsTerminal() {
			t.Errorf("%s.IsTerminal() = false, want true", status)
		}
	}
	for _, status := range []wagering.Status{wagering.StatusPending, wagering.StatusPendingReference} {
		if status.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false", status)
		}
	}

	tx := rehydrated(t, wagering.StatusProcessed)
	var transitionErr *wagering.TransitionError
	err := tx.MarkProcessed(brl(t, "1.00"), changedAt)
	if !errors.As(err, &transitionErr) || transitionErr.From != wagering.StatusProcessed {
		t.Fatalf("MarkProcessed on PROCESSED error = %v, want a *TransitionError from PROCESSED", err)
	}
}

func TestMarkProcessedRecordsTheObservedBalance(t *testing.T) {
	tx := newExternal(t, wagering.KindBet, "25.00")

	if err := tx.MarkProcessed(brl(t, "975.00"), changedAt); err != nil {
		t.Fatalf("MarkProcessed error = %v", err)
	}
	difference, err := tx.ResultBalance().Cmp(brl(t, "975.00"))
	if err != nil || difference != 0 {
		t.Errorf("ResultBalance() = %s, want 975.00 (cmp error %v)", tx.ResultBalance(), err)
	}
}

func TestMarkProcessedRejectsAnImpossibleBalance(t *testing.T) {
	negative, err := money.FromMinor(-1, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}

	tx := newExternal(t, wagering.KindBet, "25.00")
	if err := tx.MarkProcessed(negative, changedAt); !errors.Is(err, money.ErrNegativeAmount) {
		t.Errorf("MarkProcessed(-0.01) error = %v, want ErrNegativeAmount", err)
	}
	if err := tx.MarkProcessed(money.Money{}, changedAt); !errors.Is(err, wagering.ErrUninitialized) {
		t.Errorf("MarkProcessed(zero value) error = %v, want ErrUninitialized", err)
	}
	if tx.Status() != wagering.StatusPending {
		t.Errorf("Status() = %s, want PENDING after a refused MarkProcessed", tx.Status())
	}
}

func TestRejectAndFailRecordTheirCode(t *testing.T) {
	rejected := newExternal(t, wagering.KindBet, "25.00")
	if err := rejected.Reject(wagering.FailureInsufficientFunds, brl(t, "100.00"), changedAt); err != nil {
		t.Fatalf("Reject error = %v", err)
	}
	if rejected.FailureCode() != wagering.FailureInsufficientFunds {
		t.Errorf("FailureCode() = %s, want INSUFFICIENT_FUNDS", rejected.FailureCode())
	}

	failed := newExternal(t, wagering.KindBet, "25.00")
	if err := failed.Fail(wagering.FailureInternalError, changedAt); err != nil {
		t.Fatalf("Fail error = %v", err)
	}
	if failed.Status() != wagering.StatusFailed || failed.FailureCode() != wagering.FailureInternalError {
		t.Errorf("got %s/%s, want FAILED/INTERNAL_ERROR", failed.Status(), failed.FailureCode())
	}
}

func TestRejectRefusesACodeOutsideTheCatalogue(t *testing.T) {
	tx := newExternal(t, wagering.KindBet, "25.00")

	if err := tx.Reject(wagering.FailureCode("NO_IDEA"), money.Money{}, changedAt); !errors.Is(err, wagering.ErrInvalidFailureCode) {
		t.Fatalf("Reject(NO_IDEA) error = %v, want ErrInvalidFailureCode", err)
	}
	if tx.Status() != wagering.StatusPending {
		t.Errorf("Status() = %s, want PENDING after a refused Reject", tx.Status())
	}
}

// §7 requires a reversal that overdraws to be told apart from a bet that
// overdraws, and both to be distinguishable from correctable input (03 §7).
func TestFailureCodeClassification(t *testing.T) {
	if wagering.FailureInsufficientFunds == wagering.FailureReversalExceedsBalance {
		t.Fatal("the bet and reversal overdraw codes are the same string")
	}
	definitive := []wagering.FailureCode{
		wagering.FailureInsufficientFunds,
		wagering.FailureReversalExceedsBalance,
		wagering.FailureReferenceNotFound,
		wagering.FailureReferenceNotProcessed,
		wagering.FailureReferenceAlreadyReversed,
		wagering.FailureInternalError,
	}
	for _, code := range definitive {
		if !code.IsValid() || code.Correctable() {
			t.Errorf("%s: valid = %v, correctable = %v, want true/false", code, code.IsValid(), code.Correctable())
		}
	}
	correctable := []wagering.FailureCode{
		wagering.FailureWalletNotFound,
		wagering.FailureCurrencyMismatch,
		wagering.FailureReferenceMismatch,
		wagering.FailureReferenceAmount,
	}
	for _, code := range correctable {
		if !code.IsValid() || !code.Correctable() {
			t.Errorf("%s: valid = %v, correctable = %v, want true/true", code, code.IsValid(), code.Correctable())
		}
	}
	if wagering.FailureCode("").IsValid() || wagering.FailureCode("NO_IDEA").IsValid() {
		t.Error("an unknown code reports itself valid")
	}
}

// §7: a reversal must cite what it undoes, a WIN "may cite a bet from the same
// round", and a BET or LOSS carrying one is a malformed request.
func TestReferencePolicyPerKind(t *testing.T) {
	cases := []struct {
		kind      wagering.Kind
		reference string
		wantErr   error
	}{
		{wagering.KindRefund, "transaction-100", nil},
		{wagering.KindRollback, "transaction-100", nil},
		{wagering.KindRefund, "", wagering.ErrMissingReference},
		{wagering.KindRollback, "", wagering.ErrMissingReference},
		{wagering.KindWin, "transaction-100", nil},
		{wagering.KindWin, "", nil},
		{wagering.KindBet, "", nil},
		{wagering.KindBet, "transaction-100", wagering.ErrNoReference},
		{wagering.KindLoss, "transaction-100", wagering.ErrNoReference},
	}

	for _, c := range cases {
		name := c.kind.String() + "/reference=" + c.reference
		t.Run(name, func(t *testing.T) {
			amount := "25.00"
			if c.kind == wagering.KindLoss {
				amount = "0.00"
			}
			p := externalParams(c.kind, brl(t, amount))
			p.ReferenceExternalTransactionID = c.reference

			if _, err := wagering.NewExternal(p); !errors.Is(err, c.wantErr) {
				t.Errorf("NewExternal(%s, reference %q) error = %v, want %v", c.kind, c.reference, err, c.wantErr)
			}
		})
	}
}

// §9: a rejection still reports a balance, and a replay must return that one.
// It is optional because a rejection for an unknown wallet has none to observe.
func TestRejectRecordsTheReportedBalance(t *testing.T) {
	tx := newExternal(t, wagering.KindBet, "25.00")
	if err := tx.Reject(wagering.FailureInsufficientFunds, brl(t, "10.00"), changedAt); err != nil {
		t.Fatalf("Reject error = %v", err)
	}
	difference, err := tx.ResultBalance().Cmp(brl(t, "10.00"))
	if err != nil || difference != 0 {
		t.Errorf("ResultBalance() = %s, want 10.00 (cmp error %v)", tx.ResultBalance(), err)
	}

	none := newExternal(t, wagering.KindBet, "25.00")
	if err := none.Reject(wagering.FailureWalletNotFound, money.Money{}, changedAt); err != nil {
		t.Fatalf("Reject without a balance error = %v", err)
	}
	if none.ResultBalance().IsValid() {
		t.Errorf("ResultBalance() = %s, want the invalid zero value", none.ResultBalance())
	}

	negative, err := money.FromMinor(-1, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}
	bad := newExternal(t, wagering.KindBet, "25.00")
	if err := bad.Reject(wagering.FailureInsufficientFunds, negative, changedAt); !errors.Is(err, money.ErrNegativeAmount) {
		t.Errorf("Reject(-0.01) error = %v, want ErrNegativeAmount", err)
	}
	if bad.Status() != wagering.StatusPending {
		t.Errorf("Status() = %s, want PENDING after a refused Reject", bad.Status())
	}
}

// Rehydration restores the stored outcome without re-running the kind rules: a
// row whose amount would now be refused still loads (§6).
func TestRehydrateReplaysNothing(t *testing.T) {
	stored := rehydrateParams(t, wagering.StatusProcessed)
	stored.Kind = wagering.KindLoss
	stored.Amount = brl(t, "25.00")

	tx, err := wagering.Rehydrate(stored)
	if err != nil {
		t.Fatalf("Rehydrate error = %v", err)
	}
	if tx.Status() != wagering.StatusProcessed || tx.Kind() != wagering.KindLoss {
		t.Errorf("got %s/%s, want PROCESSED/LOSS", tx.Status(), tx.Kind())
	}
}

func TestRehydrateRejectsCorruptRows(t *testing.T) {
	corrupt := map[string]func(*wagering.RehydrateParams){
		"unknown status":       func(p *wagering.RehydrateParams) { p.Status = wagering.Status("STARTED") },
		"unknown kind":         func(p *wagering.RehydrateParams) { p.Kind = wagering.Kind("CASHOUT") },
		"unknown origin":       func(p *wagering.RehydrateParams) { p.Origin = wagering.Origin("PARTNER") },
		"unknown failure code": func(p *wagering.RehydrateParams) { p.FailureCode = wagering.FailureCode("NO_IDEA") },
		"internal non-opening": func(p *wagering.RehydrateParams) { p.Origin = wagering.OriginInternal },
		"external opening":     func(p *wagering.RehydrateParams) { p.Kind = wagering.KindOpening },
		"missing id":           func(p *wagering.RehydrateParams) { p.ID = uuid.Nil() },
		"missing walletId":     func(p *wagering.RehydrateParams) { p.WalletID = uuid.Nil() },
		"missing playerId":     func(p *wagering.RehydrateParams) { p.PlayerID = uuid.Nil() },
		"missing money":        func(p *wagering.RehydrateParams) { p.Amount = money.Money{} },
		"missing createdAt":    func(p *wagering.RehydrateParams) { p.CreatedAt = time.Time{} },
		"missing updatedAt":    func(p *wagering.RehydrateParams) { p.UpdatedAt = time.Time{} },
	}

	for name, corruptIt := range corrupt {
		t.Run(name, func(t *testing.T) {
			stored := rehydrateParams(t, wagering.StatusPending)
			corruptIt(&stored)
			if _, err := wagering.Rehydrate(stored); err == nil {
				t.Errorf("Rehydrate with %s error = nil, want a rejection", name)
			}
		})
	}
}

func opening(amount money.Money, now time.Time) (*wagering.WagerTransaction, error) {
	return wagering.NewInternalOpening(wagering.NewInternalOpeningParams{
		ID: txID, WalletID: walletID, PlayerID: playerID, Amount: amount, Now: now,
	})
}

func rehydrateParams(t *testing.T, status wagering.Status) wagering.RehydrateParams {
	t.Helper()
	p := externalParams(wagering.KindBet, brl(t, "25.00"))
	return wagering.RehydrateParams{
		ID:                             p.ID,
		Origin:                         wagering.OriginExternal,
		Kind:                           p.Kind,
		Status:                         status,
		WalletID:                       p.WalletID,
		PlayerID:                       p.PlayerID,
		Amount:                         p.Amount,
		ProviderID:                     p.ProviderID,
		ExternalTransactionID:          p.ExternalTransactionID,
		IdempotencyKey:                 p.IdempotencyKey,
		PayloadHash:                    p.PayloadHash,
		RoundID:                        p.RoundID,
		GameID:                         p.GameID,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		CreatedAt:                      createdAt,
		UpdatedAt:                      createdAt,
	}
}

func rehydrated(t *testing.T, status wagering.Status) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.Rehydrate(rehydrateParams(t, status))
	if err != nil {
		t.Fatalf("Rehydrate(%s) error = %v", status, err)
	}
	return tx
}

func move(t *testing.T, tx *wagering.WagerTransaction, to wagering.Status) error {
	t.Helper()
	switch to {
	case wagering.StatusPendingReference:
		return tx.MarkPendingReference(changedAt)
	case wagering.StatusProcessed:
		return tx.MarkProcessed(brl(t, "975.00"), changedAt)
	case wagering.StatusRejected:
		return tx.Reject(wagering.FailureInsufficientFunds, brl(t, "100.00"), changedAt)
	case wagering.StatusFailed:
		return tx.Fail(wagering.FailureInternalError, changedAt)
	default:
		t.Fatalf("no method transitions to %s", to)
		return nil
	}
}
