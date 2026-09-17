package wageringapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PayloadHash is SHA-256 over canonical JSON; a map is used because
// encoding/json sorts its keys, which is what makes HTTP and SQS agree (§9,
// §10). The algorithm, fields and normalisations are documented in
// docs/wagering.md.
//
// Money is hashed as minor units, never as the received text: "25" and "25.00"
// are one amount after A.3.1, and hashing the text would turn an equivalent
// resubmission into a payload conflict.
func PayloadHash(p SubmitParams) string {
	// Every value is a string or an int64, so Marshal cannot fail.
	canonical, _ := json.Marshal(map[string]any{
		"providerId":                     p.ProviderID,
		"externalTransactionId":          p.ExternalTransactionID,
		"playerId":                       p.PlayerID.String(),
		"walletId":                       p.WalletID.String(),
		"roundId":                        p.RoundID,
		"gameId":                         p.GameID,
		"kind":                           p.Kind.String(),
		"amountMinor":                    p.Money.Minor(),
		"currency":                       p.Money.Currency().String(),
		"referenceExternalTransactionId": p.ReferenceExternalTransactionID,
	})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
