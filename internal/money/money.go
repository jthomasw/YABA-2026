// Package money represents currency amounts as integer cents.
//
// Storing money in a float64 is the single most common bug in financial
// software: 0.1 + 0.2 != 0.3, and SUM() over thousands of REAL rows drifts
// away from the truth. Cents are exact for every operation this app performs.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cents is a currency amount in the smallest unit (US cents).
// A negative value is legal (it represents a debit or a shortfall); handlers
// decide whether a negative amount is acceptable for a given field.
type Cents int64

// Zero is the additive identity, provided for readability.
const Zero Cents = 0

// ErrInvalidAmount is returned when a string cannot be read as an amount.
var ErrInvalidAmount = errors.New("not a valid amount")

// maxAmount caps a single transaction at $1 billion. Without a ceiling a user
// can type a number that overflows int64 once multiplied by 100, and absurd
// values wreck every chart's y-axis. (The legacy database contains a
// 50,000,000 fund deposit created through the old delete-fund exploit, which
// is exactly the kind of value this guards against.)
const maxAmount = 100_000_000_000 // $1,000,000,000.00 in cents

// FromFloat converts a float dollar amount to Cents, rounding half away from
// zero. It exists for the legacy migration path only; new code should parse
// user input with Parse and never round-trip through float64.
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

// ParsePositive is Parse plus the requirement that the amount is greater than
// zero. Almost every form in this app wants this: the old code accepted
// negative income, which let a user reduce their spending total by "earning"
// a negative salary.
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
// denominator is zero. Progress bars and "percent of spending" summaries all
// need the same guard against division by zero.
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
