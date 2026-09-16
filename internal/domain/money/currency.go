package money

import "fmt"

// Currency is an ISO 4217 alphabetic code this service settles in. The zero
// value is invalid; only the constants below and ParseCurrency produce a usable
// one, so a currency cannot reach the domain as an unchecked string.
type Currency string

const (
	BRL Currency = "BRL"
	EUR Currency = "EUR"
	USD Currency = "USD"
)

// supported is the one place a currency is added or removed.
//
// ponytail: a set, not a metadata table, because every entry here has two
// decimal places and §6.1 fixes the external contract at that scale. A currency
// with a different exponent — JPY with none, KWD with three — is not a row in
// this map: it needs a scale per currency in parseMinor and format. At that
// point this becomes map[Currency]int holding exponents and the callers read
// from it. See A.3.1 and A.3.2.
var supported = map[Currency]struct{}{
	BRL: {},
	EUR: {},
	USD: {},
}

// ParseCurrency is the boundary for a currency arriving as text: an HTTP body,
// an SQS payload, a database column.
func ParseCurrency(raw string) (Currency, error) {
	currency := Currency(raw)
	if !currency.IsValid() {
		return "", fmt.Errorf("%w, got %q", ErrInvalidCurrency, raw)
	}
	return currency, nil
}

func (c Currency) IsValid() bool {
	_, ok := supported[c]
	return ok
}

func (c Currency) String() string { return string(c) }

func checkCurrency(currency Currency) error {
	if !currency.IsValid() {
		return fmt.Errorf("%w, got %q", ErrInvalidCurrency, string(currency))
	}
	return nil
}
