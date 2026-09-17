// Package money represents currency amounts as integer cents, because float64
// cannot hold money exactly: 0.1 + 0.2 != 0.3, and a SUM over REAL rows drifts.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cents is a currency amount in the smallest unit (US cents).
type Cents int64

// Zero is the additive identity, provided for readability.
const Zero Cents = 0

// ErrInvalidAmount is returned when a string cannot be read as an amount.
var ErrInvalidAmount = errors.New("not a valid amount")

// maxAmount caps one transaction at $1 billion.
const maxAmount = 100_000_000_000 // $1,000,000,000.00 in cents

// FromFloat converts float dollars to Cents, rounding half away from zero.
func FromFloat(f float64) Cents {
	if f >= 0 {
		return Cents(int64(f*100 + 0.5))
	}
	return Cents(int64(f*100 - 0.5))
}

// Float returns the amount as dollars. Use only at the very edge of the
// program (JSON for charts), never for arithmetic.
func (c Cents) Float() float64 {
	return float64(c) / 100
}

// Parse reads a user-supplied amount such as "12", "12.5", "12.50",
// "$1,234.56" or "-3.00" into Cents without ever touching a float.
func Parse(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidAmount
	}

	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, ErrInvalidAmount
	}

	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}

	// "5." and ".5" are both amounts somebody might type. "." on its own is not
	// one, and must not quietly become zero.
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if whole == "" {
		whole = "0"
	}

	// Pad or reject the fractional part: ".5" is 50 cents, ".456" is not a
	// representable amount and is rejected rather than silently rounded.
	switch len(frac) {
	case 0:
		frac = "00"
	case 1:
		frac += "0"
	case 2:
		// exact
	default:
		return 0, fmt.Errorf("%w: more than two decimal places", ErrInvalidAmount)
	}

	// Both halves must be digits and nothing else, checked before ParseInt
	// rather than relying on it.
	//
	// ParseInt accepts a sign of its own, and that is the whole bug this guard
	// exists to close: "--5" reaches here as whole == "-5", parses to -5, sails
	// under the "too large" ceiling because the ceiling is one-sided, and is
	// then re-negated into +$5. The same trick with a 17-digit number produced
	// $92,233,720,368,547,758 -- ninety-two thousand times the documented cap,
	// large enough that SUM(amount_cents) overflows in SQLite and every page
	// that totals money returns 500 from then on, including the page you would
	// need to delete the row.
	if !allDigits(whole) || !allDigits(frac) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if w > maxAmount/100 {
		return 0, fmt.Errorf("%w: amount too large", ErrInvalidAmount)
	}

	total := w*100 + f
	if total > maxAmount {
		return 0, fmt.Errorf("%w: amount too large", ErrInvalidAmount)
	}
	if neg {
		total = -total
	}
	return Cents(total), nil
}

// allDigits reports whether s is one or more ASCII digits and nothing else.
//
// Deliberately ASCII-only: strconv would reject a Devanagari digit anyway, and
// an amount field that quietly accepted one would be a surprise, not a feature.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParsePositive is Parse plus the requirement that the amount is above zero, which
// is what stops a negative income reducing a spending total.
func ParsePositive(s string) (Cents, error) {
	c, err := Parse(s)
	if err != nil {
		return 0, err
	}
	if c <= 0 {
		return 0, fmt.Errorf("%w: must be greater than zero", ErrInvalidAmount)
	}
	return c, nil
}

// String renders the amount with a thousands separator and two decimals,
// without a currency symbol: -1234567 becomes "-12,345.67".
func (c Cents) String() string {
	neg := c < 0
	n := int64(c)
	if neg {
		n = -n
	}

	dollars := n / 100
	cents := n % 100

	d := strconv.FormatInt(dollars, 10)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, ch := range d {
		if i > 0 && (len(d)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	fmt.Fprintf(&b, ".%02d", cents)
	return b.String()
}

// Display renders the amount for a template, prefixed with a dollar sign and
// with the sign outside the symbol: "-$12,345.67".
func (c Cents) Display() string {
	if c < 0 {
		return "-$" + (-c).String()
	}
	return "$" + c.String()
}

// Input renders the amount for an <input type="number" step="0.01"> value:
// plain digits and a dot, no separators.
func (c Cents) Input() string {
	neg := c < 0
	n := int64(c)
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d.%02d", n/100, n%100)
	if neg {
		return "-" + s
	}
	return s
}

// Ratio returns c/of as a percentage in [0, 100], clamped, and 0 when the
// denominator is zero.
func Ratio(c, of Cents) float64 {
	if of <= 0 {
		return 0
	}
	p := float64(c) / float64(of) * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
