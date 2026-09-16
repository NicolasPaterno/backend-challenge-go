// Package money holds the exact-precision monetary value object. Amounts are
// int64 minor units and never touch floating point (§5.1).
package money

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Every amount uses a fixed scale of two decimal places (§6.1), so a currency
// whose real minor unit differs — JPY with none, KWD with three — is still
// stored and rendered with two.
const (
	minorPerMajor = 100
	minorDigits   = 2
)

// amountPattern accepts the equivalent spellings of one amount: an optional
// minus, digits with or without leading zeros, and an optional fraction of one
// or two digits. "25", "25.0", "025", "025.0" and "025.00" all mean 2500.
// Excess scale ("25.000") stays rejected — §6.1 requires it — along with "",
// "NaN", "Infinity", "1e2", "+25.00", "25." and " 25.00".
var amountPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]{1,2})?$`)

var (
	ErrUninitialized    = errors.New("money: value is uninitialised")
	ErrInvalidAmount    = errors.New("money: amount must be a decimal string with at most two decimal places")
	ErrNegativeAmount   = errors.New("money: amount must not be negative")
	ErrInvalidCurrency  = errors.New("money: currency must be one of BRL, EUR, USD")
	ErrOverflow         = errors.New("money: amount is out of range")
	ErrCurrencyMismatch = errors.New("money: currencies differ")
)

// CurrencyMismatchError names both operands so a caller can report which
// currencies clashed. errors.Is matches it against ErrCurrencyMismatch.
type CurrencyMismatchError struct{ Left, Right Currency }

func (e *CurrencyMismatchError) Error() string {
	return fmt.Sprintf("money: currencies differ: %s and %s", e.Left, e.Right)
}

func (e *CurrencyMismatchError) Is(target error) bool { return target == ErrCurrencyMismatch }

// Money is an immutable amount in the minor units of one currency. Its zero
// value carries no currency and every operation rejects it (§6).
type Money struct {
	minor    int64
	currency Currency
}

// Parse reads an amount arriving from outside the system: no negative sign, and
// a scale of at most two decimal places (§6.1). Equivalent spellings such as
// "25" or "25.0" normalise to the same minor units, so the idempotency hash
// must be taken over those units and never over the received text (§9, A.3.1).
func Parse(amount string, currency Currency) (Money, error) {
	if err := checkCurrency(currency); err != nil {
		return Money{}, err
	}
	minor, err := parseMinor(amount)
	if err != nil {
		return Money{}, err
	}
	if minor < 0 {
		return Money{}, fmt.Errorf("%w, got %q", ErrNegativeAmount, amount)
	}
	return Money{minor: minor, currency: currency}, nil
}

// FromMinor builds a value from minor units already known to be exact: a
// database column, or a calculation done elsewhere. Unlike Parse it accepts
// negatives, which differences and reversals need (§6.1).
func FromMinor(minor int64, currency Currency) (Money, error) {
	if err := checkCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

func Zero(currency Currency) (Money, error) { return FromMinor(0, currency) }

func (m Money) Minor() int64 { return m.minor }

func (m Money) Currency() Currency { return m.currency }

func (m Money) IsValid() bool { return m.currency.IsValid() }

func (m Money) IsZero() bool { return m.minor == 0 }

func (m Money) IsNegative() bool { return m.minor < 0 }

func (m Money) String() string {
	if !m.IsValid() {
		return "<uninitialised money>"
	}
	return format(m.minor) + " " + m.currency.String()
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	sum := m.minor + other.minor
	if (other.minor > 0 && sum < m.minor) || (other.minor < 0 && sum > m.minor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, other)
	}
	return Money{minor: sum, currency: m.currency}, nil
}

func (m Money) Sub(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	diff := m.minor - other.minor
	if (other.minor < 0 && diff < m.minor) || (other.minor > 0 && diff > m.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, other)
	}
	return Money{minor: diff, currency: m.currency}, nil
}

func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, ErrUninitialized
	}
	// The two's-complement minimum has no positive counterpart.
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

func (m Money) Cmp(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	return cmp.Compare(m.minor, other.minor), nil
}

func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrUninitialized
	}
	return json.Marshal(wire{Amount: format(m.minor), Currency: m.currency})
}

// UnmarshalJSON is an external boundary, so it applies Parse's rules: a
// negative amount is refused here, and only arithmetic can produce one (§6.1).
func (m *Money) UnmarshalJSON(data []byte) error {
	// encoding/json asks unmarshalers to treat null as a no-op. A monetary
	// field is never optional, and §6 requires invalid domain values to be
	// rejected, so null fails here instead of leaving an invalid value for a
	// caller to notice later. An absent field never reaches this method at all
	// and stays the caller's check.
	if string(data) == "null" {
		return fmt.Errorf("%w: money must not be null", ErrUninitialized)
	}

	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("money: decode: %w", err)
	}
	parsed, err := Parse(w.Amount, w.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

type wire struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

func (m Money) compatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return ErrUninitialized
	}
	if m.currency != other.currency {
		return &CurrencyMismatchError{Left: m.currency, Right: other.currency}
	}
	return nil
}

func parseMinor(amount string) (int64, error) {
	if !amountPattern.MatchString(amount) {
		return 0, fmt.Errorf("%w, got %q", ErrInvalidAmount, amount)
	}

	// Normalisation (§6.1): pad the fraction to two digits and drop the point,
	// so every spelling of an amount collapses to the same minor units before
	// anything hashes it (§9). ParseInt ignores leading zeros.
	integer, fraction, _ := strings.Cut(amount, ".")
	digits := integer + fraction + strings.Repeat("0", minorDigits-len(fraction))

	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w, got %q", ErrOverflow, amount)
	}
	if minor == 0 && strings.HasPrefix(amount, "-") {
		return 0, fmt.Errorf("%w, got %q", ErrInvalidAmount, amount) // "-0.00" is a second spelling of zero
	}
	return minor, nil
}

func format(minor int64) string {
	sign := ""
	magnitude := uint64(minor)
	if minor < 0 {
		sign = "-"
		magnitude = -magnitude // unsigned negation, so MinInt64 does not overflow
	}
	return fmt.Sprintf("%s%d.%02d", sign, magnitude/minorPerMajor, magnitude%minorPerMajor)
}
