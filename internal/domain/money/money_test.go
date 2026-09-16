package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

const (
	maxAmount = "92233720368547758.07"  // math.MaxInt64 minor units
	minAmount = "-92233720368547758.08" // math.MinInt64 minor units
)

func mustParse(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q, %q) error = %v, want nil", amount, currency, err)
	}
	return m
}

func mustFromMinor(t *testing.T, minor int64, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, currency)
	if err != nil {
		t.Fatalf("FromMinor(%d, %q) error = %v, want nil", minor, currency, err)
	}
	return m
}

func TestParseAcceptsEquivalentSpellings(t *testing.T) {
	tests := map[string]int64{
		"25.00":   2500,
		"25.0":    2500,
		"25":      2500,
		"025.00":  2500,
		"025.0":   2500,
		"025":     2500,
		"0.00":    0, // LOSS requires exactly this (§7)
		"0":       0,
		"0.01":    1,
		"0.1":     10,
		"1000.00": 100000,
		maxAmount: math.MaxInt64,
	}

	for amount, want := range tests {
		t.Run(amount, func(t *testing.T) {
			m := mustParse(t, amount, money.BRL)
			if m.Minor() != want {
				t.Errorf("Minor() = %d, want %d", m.Minor(), want)
			}
			if m.Currency() != money.BRL {
				t.Errorf("Currency() = %q, want %q", m.Currency(), money.BRL)
			}
		})
	}
}

func TestParseRejectsMalformedAmounts(t *testing.T) {
	for _, amount := range []string{
		"", " ", "NaN", "Infinity", "-Infinity", "1e2", "1E2", "2.5e1",
		"25.000", "25.", ".00", "+25.00",
		" 25.00", "25.00 ", "2 5.00", "25,00", "1.00.00", "--1.00", "-0.00",
	} {
		t.Run(amount, func(t *testing.T) {
			_, err := money.Parse(amount, money.BRL)
			if !errors.Is(err, money.ErrInvalidAmount) {
				t.Errorf("Parse(%q) error = %v, want ErrInvalidAmount", amount, err)
			}
		})
	}
}

// An invalid amount must never be rounded into a valid one (§6.1).
// §6.1 allows equivalent forms only if the normalisation is documented, and §9
// requires HTTP and SQS to agree: every spelling must collapse to one value and
// one canonical rendering before anything hashes it.
func TestEquivalentSpellingsAreIndistinguishable(t *testing.T) {
	canonical := mustParse(t, "25.00", money.BRL)

	for _, spelling := range []string{"25", "25.0", "025", "025.0", "025.00"} {
		t.Run(spelling, func(t *testing.T) {
			parsed := mustParse(t, spelling, money.BRL)
			if parsed != canonical {
				t.Errorf("Parse(%q) = %s, want it equal to Parse(\"25.00\") = %s", spelling, parsed, canonical)
			}
			encoded, err := json.Marshal(parsed)
			if err != nil {
				t.Fatalf("Marshal error = %v", err)
			}
			if want := `{"amount":"25.00","currency":"BRL"}`; string(encoded) != want {
				t.Errorf("Marshal = %s, want %s", encoded, want)
			}
		})
	}
}

func TestParseDoesNotRoundExcessScale(t *testing.T) {
	if _, err := money.Parse("25.005", money.BRL); !errors.Is(err, money.ErrInvalidAmount) {
		t.Fatalf("Parse(\"25.005\") error = %v, want ErrInvalidAmount", err)
	}
}

func TestParseRejectsNegativeExternalInput(t *testing.T) {
	_, err := money.Parse("-25.00", money.BRL)
	if !errors.Is(err, money.ErrNegativeAmount) {
		t.Errorf("Parse(\"-25.00\") error = %v, want ErrNegativeAmount", err)
	}
}

func TestParseRejectsOutOfRangeAmounts(t *testing.T) {
	for _, amount := range []string{"92233720368547758.08", "999999999999999999999.99"} {
		t.Run(amount, func(t *testing.T) {
			if _, err := money.Parse(amount, money.BRL); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("Parse(%q) error = %v, want ErrOverflow", amount, err)
			}
		})
	}
}

func TestParseCurrencyRejectsUnsupportedCodes(t *testing.T) {
	// GBP and JPY are real ISO 4217 codes this service deliberately does not
	// settle in, so they must fail exactly like malformed input (A.3.2).
	for _, raw := range []string{"", "brl", "BR", "BRLL", "B R", "XXX", "XAU", "USN", "ZZZ", "GBP", "JPY"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := money.ParseCurrency(raw); !errors.Is(err, money.ErrInvalidCurrency) {
				t.Errorf("ParseCurrency(%q) error = %v, want ErrInvalidCurrency", raw, err)
			}
			// A Currency conversion bypasses ParseCurrency, so the constructors
			// must reject it too rather than trust the type alone.
			if _, err := money.Parse("25.00", money.Currency(raw)); !errors.Is(err, money.ErrInvalidCurrency) {
				t.Errorf("Parse(_, %q) error = %v, want ErrInvalidCurrency", raw, err)
			}
		})
	}
}

func TestEverySupportedCurrencyRoundTrips(t *testing.T) {
	for _, currency := range []money.Currency{money.BRL, money.EUR, money.USD} {
		t.Run(currency.String(), func(t *testing.T) {
			parsed, err := money.ParseCurrency(currency.String())
			if err != nil || parsed != currency {
				t.Fatalf("ParseCurrency(%q) = %v, %v", currency, parsed, err)
			}
			if _, err := money.Parse("25.00", currency); err != nil {
				t.Errorf("Parse(_, %q) error = %v, want nil", currency, err)
			}
		})
	}
}

func TestZeroCarriesCurrency(t *testing.T) {
	m, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatalf("Zero(\"BRL\") error = %v, want nil", err)
	}
	if !m.IsZero() || !m.IsValid() || m.Currency() != money.BRL {
		t.Errorf("Zero(\"BRL\") = %v, want a valid zero in BRL", m)
	}
	if _, err := money.Zero("ZZZ"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Zero(\"ZZZ\") error = %v, want ErrInvalidCurrency", err)
	}
}

func TestArithmetic(t *testing.T) {
	tests := map[string]struct {
		op   func(a, b money.Money) (money.Money, error)
		a, b string
		want string
	}{
		"add":              {op: money.Money.Add, a: "10.00", b: "15.50", want: "25.50"},
		"add zero":         {op: money.Money.Add, a: "10.00", b: "0.00", want: "10.00"},
		"sub":              {op: money.Money.Sub, a: "100.00", b: "80.00", want: "20.00"},
		"sub below zero":   {op: money.Money.Sub, a: "10.00", b: "15.00", want: "-5.00"},
		"sub to zero":      {op: money.Money.Sub, a: "10.00", b: "10.00", want: "0.00"},
		"add carries cent": {op: money.Money.Add, a: "0.99", b: "0.01", want: "1.00"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tc.op(mustParse(t, tc.a, money.BRL), mustParse(t, tc.b, money.BRL))
			if err != nil {
				t.Fatalf("op error = %v, want nil", err)
			}
			if want := tc.want + " BRL"; got.String() != want {
				t.Errorf("result = %s, want %s", got, want)
			}
		})
	}
}

func TestNeg(t *testing.T) {
	negated, err := mustParse(t, "25.00", money.BRL).Neg()
	if err != nil {
		t.Fatalf("Neg() error = %v, want nil", err)
	}
	if !negated.IsNegative() || negated.Minor() != -2500 {
		t.Errorf("Neg() = %s, want -25.00 BRL", negated)
	}

	back, err := negated.Neg()
	if err != nil {
		t.Fatalf("Neg() twice error = %v, want nil", err)
	}
	if back.Minor() != 2500 {
		t.Errorf("Neg() twice = %s, want 25.00 BRL", back)
	}
}

func TestOverflowIsReportedNotWrapped(t *testing.T) {
	maxed := mustFromMinor(t, math.MaxInt64, money.BRL)
	floored := mustFromMinor(t, math.MinInt64, money.BRL)
	oneCent := mustParse(t, "0.01", money.BRL)

	if _, err := maxed.Add(oneCent); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("max.Add(0.01) error = %v, want ErrOverflow", err)
	}
	if _, err := floored.Sub(oneCent); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("min.Sub(0.01) error = %v, want ErrOverflow", err)
	}
	if _, err := floored.Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("min.Neg() error = %v, want ErrOverflow", err)
	}
	if got, err := maxed.Add(mustParse(t, "0.00", money.BRL)); err != nil || got.Minor() != math.MaxInt64 {
		t.Errorf("max.Add(0.00) = %v, %v, want the boundary itself", got, err)
	}
}

func TestCmp(t *testing.T) {
	ten := mustParse(t, "10.00", money.BRL)
	twenty := mustParse(t, "20.00", money.BRL)

	for _, tc := range []struct {
		a, b money.Money
		want int
	}{{ten, twenty, -1}, {twenty, ten, 1}, {ten, ten, 0}} {
		got, err := tc.a.Cmp(tc.b)
		if err != nil {
			t.Fatalf("Cmp() error = %v, want nil", err)
		}
		if got != tc.want {
			t.Errorf("%s.Cmp(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCurrencyMismatchIsTypedAndClassifiable(t *testing.T) {
	brl := mustParse(t, "10.00", money.BRL)
	usd := mustParse(t, "10.00", money.USD)

	operations := map[string]func() error{
		"Add": func() error { _, err := brl.Add(usd); return err },
		"Sub": func() error { _, err := brl.Sub(usd); return err },
		"Cmp": func() error { _, err := brl.Cmp(usd); return err },
	}

	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			err := operation()
			if !errors.Is(err, money.ErrCurrencyMismatch) {
				t.Fatalf("%s error = %v, want ErrCurrencyMismatch", name, err)
			}
			var mismatch *money.CurrencyMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("%s error = %v, want a *CurrencyMismatchError", name, err)
			}
			if mismatch.Left != money.BRL || mismatch.Right != money.USD {
				t.Errorf("mismatch = %+v, want BRL and USD", mismatch)
			}
			if want := "money: currencies differ: BRL and USD"; mismatch.Error() != want {
				t.Errorf("Error() = %q, want %q", mismatch.Error(), want)
			}
		})
	}
}

func TestUninitialisedValueIsRejected(t *testing.T) {
	var zero money.Money
	valid := mustParse(t, "10.00", money.BRL)

	if zero.IsValid() {
		t.Error("IsValid() = true for the zero value, want false")
	}
	for name, operation := range map[string]func() error{
		"Add":         func() error { _, err := valid.Add(zero); return err },
		"Sub":         func() error { _, err := zero.Sub(valid); return err },
		"Cmp":         func() error { _, err := valid.Cmp(zero); return err },
		"Neg":         func() error { _, err := zero.Neg(); return err },
		"MarshalJSON": func() error { _, err := zero.MarshalJSON(); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, money.ErrUninitialized) {
				t.Errorf("%s error = %v, want ErrUninitialized", name, err)
			}
		})
	}

	// The realistic path: a response struct marshalled by encoding/json, which
	// wraps the failure in *json.MarshalerError.
	t.Run("marshalled inside a struct", func(t *testing.T) {
		_, err := json.Marshal(struct {
			Balance money.Money `json:"balance"`
		}{})
		if !errors.Is(err, money.ErrUninitialized) {
			t.Errorf("Marshal error = %v, want it to wrap ErrUninitialized", err)
		}
	})
}

func TestJSONRoundTrip(t *testing.T) {
	const payload = `{"amount":"25.00","currency":"BRL"}`

	var decoded money.Money
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if decoded.Minor() != 2500 || decoded.Currency() != money.BRL {
		t.Fatalf("decoded = %s, want 25.00 BRL", decoded)
	}

	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	if string(encoded) != payload {
		t.Errorf("Marshal = %s, want %s", encoded, payload)
	}
}

func TestUnmarshalAppliesParseRules(t *testing.T) {
	tests := map[string]struct {
		payload string
		want    error
	}{
		"negative":           {`{"amount":"-5.00","currency":"BRL"}`, money.ErrNegativeAmount},
		"excess scale":       {`{"amount":"25.000","currency":"BRL"}`, money.ErrInvalidAmount},
		"scientific":         {`{"amount":"2.5e1","currency":"BRL"}`, money.ErrInvalidAmount},
		"excess scale short": {`{"amount":"25.0000","currency":"BRL"}`, money.ErrInvalidAmount},
		"missing amount":     {`{"currency":"BRL"}`, money.ErrInvalidAmount},
		"unknown currency":   {`{"amount":"25.00","currency":"ZZZ"}`, money.ErrInvalidCurrency},
		"null":               {`null`, money.ErrUninitialized},
		"number not string":  {`{"amount":25.00,"currency":"BRL"}`, nil},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var m money.Money
			err := json.Unmarshal([]byte(tc.payload), &m)
			if err == nil {
				t.Fatalf("Unmarshal(%s) error = nil, want an error", tc.payload)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Unmarshal(%s) error = %v, want %v", tc.payload, err, tc.want)
			}
		})
	}
}

// Reconciliation reports a difference that may be negative (§9), so marshalling
// must render a sign even though unmarshalling refuses to read one.
func TestMarshalRendersNegativeDifference(t *testing.T) {
	difference, err := mustParse(t, "10.00", money.BRL).Sub(mustParse(t, "15.00", money.BRL))
	if err != nil {
		t.Fatalf("Sub error = %v, want nil", err)
	}

	encoded, err := json.Marshal(difference)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	if want := `{"amount":"-5.00","currency":"BRL"}`; string(encoded) != want {
		t.Errorf("Marshal = %s, want %s", encoded, want)
	}
}

func TestStringRendersBoundaries(t *testing.T) {
	if got := mustFromMinor(t, math.MinInt64, money.BRL).String(); got != minAmount+" BRL" {
		t.Errorf("String() = %s, want %s BRL", got, minAmount)
	}
	if got := mustFromMinor(t, 5, money.BRL).String(); got != "0.05 BRL" {
		t.Errorf("String() = %s, want 0.05 BRL", got)
	}
	var zero money.Money
	if got := zero.String(); !strings.Contains(got, "uninitialised") {
		t.Errorf("String() = %s, want it to flag the uninitialised value", got)
	}
}
