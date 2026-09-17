package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
)

func NewReconcileWalletRoute(h *WalletHandler, g *Guard) httpserver.Route {
	return httpserver.Route{
		Pattern: "POST /wallets/{walletId}/reconciliation",
		Handler: g.Require(auth.ScopeWallets, h.reconcile),
	}
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int         `json:"checkedEntries"`
}

// POST, as §9 names it, though it changes nothing: the wallet and its ledger
// are only read.
func (h *WalletHandler) reconcile(w http.ResponseWriter, r *http.Request) {
	walletID, err := uuid.Parse(r.PathValue("walletId"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields",
			Violation{"walletId", ViolationInvalid, "walletId must be a UUID"})
		return
	}

	report, err := h.wallets.Reconcile(r.Context(), walletID)
	switch {
	case errors.Is(err, walletapp.ErrNotFound):
		writeProblem(w, http.StatusNotFound, CodeWalletNotFound, "wallet not found")
		return
	case err != nil:
		h.fail(w, r, "reconcile wallet", err)
		return
	}

	// §12 wants a divergence visible without reading the response that found it.
	if !report.Consistent {
		metrics.ReconciliationDivergences.Add(1)
		h.logger.ErrorContext(r.Context(), "wallet balance diverges from its ledger",
			slog.String("walletId", walletID.String()),
			slog.String("storedBalance", report.Stored.String()),
			slog.String("calculatedBalance", report.Calculated.String()),
			slog.String("difference", report.Difference.String()))
	}

	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID:          walletID.String(),
		StoredBalance:     report.Stored,
		CalculatedBalance: report.Calculated,
		Difference:        report.Difference,
		Consistent:        report.Consistent,
		CheckedEntries:    report.CheckedEntries,
	})
}
