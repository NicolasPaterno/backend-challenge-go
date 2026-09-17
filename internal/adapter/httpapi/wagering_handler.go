package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

// Mandatory, and stored as received: {providerId}:{externalTransactionId} is a
// convention of the client, never a value this API recomputes (§9).
const idempotencyKeyHeader = "Idempotency-Key"

type WageringHandler struct {
	wagering *wageringapp.Service
	logger   *slog.Logger
}

func NewWageringHandler(wagering *wageringapp.Service, logger *slog.Logger) *WageringHandler {
	return &WageringHandler{wagering: wagering, logger: logger}
}

func NewSubmitTransactionRoute(h *WageringHandler, g *Guard) httpserver.Route {
	return httpserver.Route{Pattern: "POST /wagering/transactions", Handler: g.Require(auth.ScopeWagering, h.submit)}
}

func NewGetTransactionRoute(h *WageringHandler, g *Guard) httpserver.Route {
	return httpserver.Route{Pattern: "GET /wagering/transactions/{transactionId}", Handler: g.Require(auth.ScopeWagering, h.get)}
}

func NewGetProviderTransactionRoute(h *WageringHandler, g *Guard) httpserver.Route {
	return httpserver.Route{
		Pattern: "GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		Handler: g.Require(auth.ScopeWagering, h.getByExternalID),
	}
}

// Money is raw so an invalid amount and a missing gameId are both reported,
// as in openWalletRequest (A.3.5).
type submitRequest struct {
	ProviderID                     string          `json:"providerId"`
	ExternalTransactionID          string          `json:"externalTransactionId"`
	PlayerID                       string          `json:"playerId"`
	WalletID                       string          `json:"walletId"`
	RoundID                        string          `json:"roundId"`
	GameID                         string          `json:"gameId"`
	Kind                           string          `json:"kind"`
	Money                          json.RawMessage `json:"money"`
	ReferenceExternalTransactionID string          `json:"referenceExternalTransactionId"`
}

type submitResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

func (h *WageringHandler) submit(w http.ResponseWriter, r *http.Request) {
	var body submitRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, CodeMalformedBody, "request body is not valid JSON")
		return
	}

	// Before any validation is reported, so a caller cannot probe another
	// provider's contract (§2).
	identity, _ := auth.FromContext(r.Context())
	if body.ProviderID != "" && body.ProviderID != identity.ProviderID {
		writeProblem(w, http.StatusForbidden, CodeForbidden,
			"the access token does not authorize this providerId")
		return
	}

	params, violations := decodeSubmit(r, body)
	if len(violations) > 0 {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields", violations...)
		return
	}

	result, err := h.wagering.Submit(r.Context(), params)
	switch {
	case err == nil:
		// The identifiers §12 asks for; the amounts stay out of the log.
		h.logger.InfoContext(r.Context(), "wager transaction handled",
			slog.String("providerId", params.ProviderID),
			slog.String("walletId", params.WalletID.String()),
			slog.String("transactionId", result.Transaction.ID().String()),
			slog.String("status", result.Transaction.Status().String()),
			slog.Bool("idempotentReplay", result.Replay))
		h.writeOutcome(w, result)
	case errors.Is(err, wageringapp.ErrPayloadConflict):
		writeProblem(w, http.StatusConflict, CodeIdempotencyKeyConflict,
			"this Idempotency-Key was already used for a different payload")
	case errors.Is(err, wageringapp.ErrExternalIDConflict):
		writeProblem(w, http.StatusConflict, CodeExternalTransactionConflict,
			"this externalTransactionId was already submitted under another Idempotency-Key")
	case errors.Is(err, wageringapp.ErrWalletBusy):
		writeUnavailable(w, "the wallet is busy; the operation was not applied")
	case errors.Is(err, wageringapp.ErrUnsupportedKind), wagering.IsRefusal(err):
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields",
			kindViolation(err))
	default:
		h.fail(w, r, "submit wager transaction", err)
	}
}

// §9's outcome matrix, tabulated in docs/wagering.md. Only 13's pending
// references reach 202.
func (h *WageringHandler) writeOutcome(w http.ResponseWriter, result wageringapp.Result) {
	t := result.Transaction
	switch t.Status() {
	case wagering.StatusRejected:
		writeRejection(w, http.StatusUnprocessableEntity, t, result.Replay)
	case wagering.StatusFailed:
		writeRejection(w, http.StatusInternalServerError, t, result.Replay)
	case wagering.StatusProcessed:
		writeJSON(w, http.StatusOK, newSubmitResponse(result))
	default:
		writeJSON(w, http.StatusAccepted, newSubmitResponse(result))
	}
}

func newSubmitResponse(result wageringapp.Result) submitResponse {
	t := result.Transaction
	body := submitResponse{
		TransactionID:    t.ID().String(),
		Status:           t.Status().String(),
		IdempotentReplay: result.Replay,
	}
	if balance := t.ResultBalance(); balance.IsValid() {
		body.Balance = &balance
	}
	return body
}

func decodeSubmit(r *http.Request, body submitRequest) (wageringapp.SubmitParams, []Violation) {
	var violations []Violation

	require := func(field, value string) {
		if value == "" {
			violations = append(violations, Violation{field, ViolationRequired, field + " is required"})
		}
	}
	require("providerId", body.ProviderID)
	require("externalTransactionId", body.ExternalTransactionID)
	require("roundId", body.RoundID)
	require("gameId", body.GameID)

	key := r.Header.Get(idempotencyKeyHeader)
	if key == "" {
		violations = append(violations, Violation{idempotencyKeyHeader, ViolationRequired,
			idempotencyKeyHeader + " is a required header"})
	}

	playerID := parseUUIDField("playerId", body.PlayerID, &violations)
	walletID := parseUUIDField("walletId", body.WalletID, &violations)

	kind := wagering.Kind(body.Kind)
	switch {
	case body.Kind == "":
		violations = append(violations, Violation{"kind", ViolationRequired, "kind is required"})
	case !kind.IsValid():
		violations = append(violations, Violation{"kind", ViolationInvalid, "kind is not a known operation"})
	}

	var amount money.Money
	if len(body.Money) == 0 {
		violations = append(violations, Violation{"money", ViolationRequired, "money is required"})
	} else if err := json.Unmarshal(body.Money, &amount); err != nil {
		violations = append(violations, moneyViolation("money", err))
	}

	return wageringapp.SubmitParams{
		ProviderID:                     body.ProviderID,
		ExternalTransactionID:          body.ExternalTransactionID,
		IdempotencyKey:                 key,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        body.RoundID,
		GameID:                         body.GameID,
		Kind:                           kind,
		Money:                          amount,
		ReferenceExternalTransactionID: body.ReferenceExternalTransactionID,
	}, violations
}

func parseUUIDField(field, value string, violations *[]Violation) uuid.UUID {
	if value == "" {
		*violations = append(*violations, Violation{field, ViolationRequired, field + " is required"})
		return uuid.Nil()
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		*violations = append(*violations, Violation{field, ViolationInvalid, field + " must be a UUID"})
	}
	return parsed
}

type transactionResponse struct {
	TransactionID         string       `json:"transactionId"`
	ProviderID            string       `json:"providerId"`
	ExternalTransactionID string       `json:"externalTransactionId"`
	WalletID              string       `json:"walletId"`
	RoundID               string       `json:"roundId"`
	GameID                string       `json:"gameId"`
	Kind                  string       `json:"kind"`
	Status                string       `json:"status"`
	Money                 money.Money  `json:"money"`
	FailureCode           string       `json:"failureCode,omitempty"`
	Balance               *money.Money `json:"balance,omitempty"`
	CreatedAt             time.Time    `json:"createdAt"`
	UpdatedAt             time.Time    `json:"updatedAt"`
}

func newTransactionResponse(t *wagering.WagerTransaction) transactionResponse {
	body := transactionResponse{
		TransactionID:         t.ID().String(),
		ProviderID:            t.ProviderID(),
		ExternalTransactionID: t.ExternalTransactionID(),
		WalletID:              t.WalletID().String(),
		RoundID:               t.RoundID(),
		GameID:                t.GameID(),
		Kind:                  t.Kind().String(),
		Status:                t.Status().String(),
		Money:                 t.Amount(),
		FailureCode:           t.FailureCode().String(),
		CreatedAt:             t.CreatedAt(),
		UpdatedAt:             t.UpdatedAt(),
	}
	if balance := t.ResultBalance(); balance.IsValid() {
		body.Balance = &balance
	}
	return body
}

func (h *WageringHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("transactionId"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, CodeValidationFailed, "the request has invalid fields",
			Violation{"transactionId", ViolationInvalid, "transactionId must be a UUID"})
		return
	}

	found, err := h.wagering.ByID(r.Context(), id)
	if h.failedRead(w, r, err) {
		return
	}

	// Another provider's transaction is reported as missing, not as forbidden:
	// a 403 here would confirm the id exists (§2, §13).
	identity, _ := auth.FromContext(r.Context())
	if found.ProviderID() != identity.ProviderID {
		writeProblem(w, http.StatusNotFound, CodeTransactionNotFound, "transaction not found")
		return
	}

	writeJSON(w, http.StatusOK, newTransactionResponse(found))
}

func (h *WageringHandler) getByExternalID(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerId")
	identity, _ := auth.FromContext(r.Context())
	if providerID != identity.ProviderID {
		writeProblem(w, http.StatusForbidden, CodeForbidden,
			"the access token does not authorize this providerId")
		return
	}

	found, err := h.wagering.ByExternalID(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if h.failedRead(w, r, err) {
		return
	}

	writeJSON(w, http.StatusOK, newTransactionResponse(found))
}

func (h *WageringHandler) failedRead(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, wageringapp.ErrNotFound):
		writeProblem(w, http.StatusNotFound, CodeTransactionNotFound, "transaction not found")
	default:
		h.fail(w, r, "read wager transaction", err)
	}
	return true
}

func (h *WageringHandler) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	failInternal(h.logger, w, r, operation, err)
}
