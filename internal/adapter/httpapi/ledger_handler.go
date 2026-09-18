package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

func NewGetWalletLedgerRoute(h *WalletHandler, g *Guard) httpserver.Route {
	return httpserver.Route{Pattern: "GET /wallets/{walletId}/ledger", Handler: g.Require(auth.ScopeWallets, h.ledger)}
}

type ledgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerResponse struct {
	Entries []ledgerEntryResponse `json:"entries"`
	// A full last page still carries one, so a ledger whose length is a multiple
	// of limit ends on an empty page.
	NextCursor string `json:"nextCursor,omitempty"`
}

func (h *WalletHandler) ledger(w http.ResponseWriter, r *http.Request) {
	var violations []Violation

	walletID, err := uuid.Parse(r.PathValue("walletId"))
	if err != nil {
		violations = append(violations, Violation{"walletId", ViolationInvalid, "walletId must be a UUID"})
	}

	after, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		violations = append(violations, Violation{"cursor", ViolationInvalid, "cursor is not a cursor this API issued"})
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		violations = append(violations, Violation{"limit", ViolationInvalid, err.Error()})
	}

	if len(violations) > 0 {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields", violations...)
		return
	}

	entries, err := h.wallets.Ledger(r.Context(), walletapp.LedgerParams{WalletID: walletID, After: after, Limit: limit})
	switch {
	case errors.Is(err, walletapp.ErrNotFound):
		writeProblem(w, http.StatusNotFound, CodeWalletNotFound, "wallet not found")
		return
	case err != nil:
		h.fail(w, r, "read wallet ledger", err)
		return
	}

	body := ledgerResponse{Entries: make([]ledgerEntryResponse, 0, len(entries))}
	for _, e := range entries {
		body.Entries = append(body.Entries, ledgerEntryResponse{
			ID:            e.ID().String(),
			TransactionID: e.TransactionID().String(),
			Direction:     e.Direction().String(),
			Money:         e.Amount(),
			BalanceBefore: e.BalanceBefore(),
			BalanceAfter:  e.BalanceAfter(),
			CreatedAt:     e.CreatedAt(),
		})
	}
	if len(entries) == walletapp.LedgerLimit(limit) {
		body.NextCursor = encodeCursor(entries[len(entries)-1])
	}

	writeJSON(w, http.StatusOK, body)
}

// Opaque by construction: the sort key is encoded, never an offset.
func encodeCursor(e *wallet.LedgerEntry) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(e.CreatedAt().UTC().Format(time.RFC3339Nano) + "|" + e.ID().String()))
}

func decodeCursor(raw string) (*walletapp.LedgerCursor, error) {
	if raw == "" {
		return nil, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	createdAt, id, found := strings.Cut(string(decoded), "|")
	if !found {
		return nil, errors.New("cursor has no separator")
	}
	at, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, err
	}
	entryID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return &walletapp.LedgerCursor{CreatedAt: at, ID: entryID}, nil
}

// The service caps the maximum, so only an unreadable value is refused here.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	return limit, nil
}
