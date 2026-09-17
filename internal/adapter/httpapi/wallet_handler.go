package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

type WalletHandler struct {
	wallets *walletapp.Service
	logger  *slog.Logger
}

func NewWalletHandler(wallets *walletapp.Service, logger *slog.Logger) *WalletHandler {
	return &WalletHandler{wallets: wallets, logger: logger}
}

// The whole wallet surface is internal-service only (§2); a provider token is
// verified and then refused with 403.
func NewOpenWalletRoute(h *WalletHandler, g *Guard) httpserver.Route {
	return httpserver.Route{Pattern: "POST /wallets", Handler: g.Require(auth.ScopeWallets, h.open)}
}

func NewGetWalletRoute(h *WalletHandler, g *Guard) httpserver.Route {
	return httpserver.Route{Pattern: "GET /wallets/{walletId}", Handler: g.Require(auth.ScopeWallets, h.get)}
}

type walletResponse struct {
	ID       string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

func newWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{
		ID:       w.ID().String(),
		PlayerID: w.PlayerID().String(),
		Balance:  w.Balance(),
		Version:  w.Version(),
	}
}

// InitialBalance is held raw so a bad amount and a bad player id are both
// reported: unmarshalling the whole body at once stops at the first (A.3.5).
type openWalletRequest struct {
	PlayerID       string          `json:"playerId"`
	InitialBalance json.RawMessage `json:"initialBalance"`
}

func (h *WalletHandler) open(w http.ResponseWriter, r *http.Request) {
	var body openWalletRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, CodeMalformedBody, "request body is not valid JSON")
		return
	}

	var violations []Violation
	playerID, err := uuid.Parse(body.PlayerID)
	switch {
	case body.PlayerID == "":
		violations = append(violations, Violation{"playerId", ViolationRequired, "playerId is required"})
	case err != nil:
		violations = append(violations, Violation{"playerId", ViolationInvalid, "playerId must be a UUID"})
	}

	var initialBalance money.Money
	if len(body.InitialBalance) == 0 {
		violations = append(violations, Violation{"initialBalance", ViolationRequired, "initialBalance is required"})
	} else if err := json.Unmarshal(body.InitialBalance, &initialBalance); err != nil {
		violations = append(violations, moneyViolation("initialBalance", err))
	}

	if len(violations) > 0 {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields", violations...)
		return
	}

	opened, err := h.wallets.Open(r.Context(), walletapp.OpenParams{PlayerID: playerID, InitialBalance: initialBalance})
	switch {
	case errors.Is(err, walletapp.ErrAlreadyExists):
		writeProblem(w, http.StatusConflict, CodeWalletAlreadyExists,
			"a wallet already exists for this player and currency")
		return
	case err != nil:
		h.fail(w, r, "open wallet", err)
		return
	}

	writeJSON(w, http.StatusCreated, newWalletResponse(opened))
}

func (h *WalletHandler) get(w http.ResponseWriter, r *http.Request) {
	walletID, err := uuid.Parse(r.PathValue("walletId"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields",
			Violation{"walletId", ViolationInvalid, "walletId must be a UUID"})
		return
	}

	found, err := h.wallets.ByID(r.Context(), walletID)
	switch {
	case errors.Is(err, walletapp.ErrNotFound):
		writeProblem(w, http.StatusNotFound, CodeWalletNotFound, "wallet not found")
		return
	case err != nil:
		h.fail(w, r, "read wallet", err)
		return
	}

	writeJSON(w, http.StatusOK, newWalletResponse(found))
}

func (h *WalletHandler) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	failInternal(h.logger, w, r, operation, err)
}
