package money

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Cents
		ok   bool
	}{
		{"0", 0, true},
		{"1", 100, true},
		{"12.5", 1250, true},
		{"12.50", 1250, true},
		{".5", 50, true},
		{"$1,234.56", 123456, true},
		{"  42.07  ", 4207, true},
		{"-3.00", -300, true},
		{"+3", 300, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1.234", 0, false}, // sub-cent precision is rejected, not rounded
		{"-", 0, false},
		{"999999999999", 0, false}, // beyond the $1B ceiling
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if tc.ok && err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("Parse(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParsePositiveRejectsZeroAndNegative(t *testing.T) {
	for _, in := range []string{"0", "0.00", "-1", "-0.01"} {
		if _, err := ParsePositive(in); err == nil {
			t.Errorf("ParsePositive(%q) should have failed", in)
		}
	}
	if c, err := ParsePositive("0.01"); err != nil || c != 1 {
		t.Errorf("ParsePositive(0.01) = %d, %v; want 1, nil", c, err)
	}
}

// The reason this package exists: the float path drifts, the cents path does not.
func TestCentsSumDoesNotDrift(t *testing.T) {
	var float float64
	var cents Cents
	for i := 0; i < 10000; i++ {
		float += 0.01
		cents += 1
	}
	if cents != 10000 {
		t.Fatalf("cents sum = %d, want 10000", cents)
	}
	if float == 100.0 {
		t.Log("float happened to land exactly; cents are guaranteed to")
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		in   Cents
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{100, "1.00"},
		{123456, "1,234.56"},
		{100000000, "1,000,000.00"},
		{-123456, "-1,234.56"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Cents(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisplayPutsSignOutsideSymbol(t *testing.T) {
	if got := Cents(-123456).Display(); got != "-$1,234.56" {
		t.Errorf("Display() = %q, want %q", got, "-$1,234.56")
	}
	if got := Cents(123456).Display(); got != "$1,234.56" {
		t.Errorf("Display() = %q, want %q", got, "$1,234.56")
	}
}

func TestRatioClampsAndGuardsDivideByZero(t *testing.T) {
	if got := Ratio(50, 0); got != 0 {
		t.Errorf("Ratio(50, 0) = %v, want 0", got)
	}
	if got := Ratio(50, 100); got != 50 {
		t.Errorf("Ratio(50, 100) = %v, want 50", got)
	}
	if got := Ratio(500, 100); got != 100 {
		t.Errorf("Ratio(500, 100) = %v, want 100 (clamped)", got)
	}
	if got := Ratio(-5, 100); got != 0 {
		t.Errorf("Ratio(-5, 100) = %v, want 0 (clamped)", got)
	}
}

func TestParseRoundTripThroughInput(t *testing.T) {
	for _, in := range []string{"0.00", "1.00", "12.34", "-12.34", "1000000.99"} {
		c, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := c.Input(); got != in {
			t.Errorf("round trip %q -> %d -> %q", in, c, got)
		}
	}
}

// TestParseRejectsASecondSign is the regression test for a parser bypass that
// could store an amount ninety-two thousand times larger than the documented
// ceiling.
//
// Parse strips one leading sign itself and then handed the rest to ParseInt,
// which is willing to strip another. "--5" arrived at ParseInt as "-5" and came
// back as -5; the "too large" ceiling only ever compared upwards, so a negative
// whole part passed it; and the final `if neg { total = -total }` turned the
// result positive again. With seventeen digits the same path yielded
// 9223372036854775800 cents, which overflows SUM(amount_cents) in SQLite and
// makes every page that totals money 500 for that household permanently.
func TestParseRejectsASecondSign(t *testing.T) {
	for _, in := range []string{
		"--5",
		"---5",
		"--92233720368547758",
		"$-,-5",
		"-+5",
		"+-5",
		"+ 5",
		"5-",
		"1-2",
		"5.-1",
		"5.1-",
		"- 5",
	} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d cents, want an error", in, int64(got))
		}
	}
}

// Nothing but ASCII digits, a single dot and the documented decorations may
// appear in an amount. Each of these used to be worth checking by hand; they are
// cheap to keep checked.
func TestParseRejectsNonDigits(t *testing.T) {
	for _, in := range []string{
		"1e9",      // scientific notation is not an amount
		"0x10",     // nor is hex
		"1_000",    // Go literal syntax is not user syntax
		"१२",       // Devanagari digits
		"12 5",     // non-breaking space inside the number
		"1 2",      // ordinary space inside the number
		"12.3.4",   // two decimal points
		"12..5",    //
		".",        // a lone point
		"$",        //
		"-",        //
		"NaN",      //
		"Inf",      //
		"infinity", //
		"٠١",       // Arabic-Indic digits
	} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d cents, want an error", in, int64(got))
		}
	}
}

// The boundary itself, from both sides. $1,000,000,000.00 is the documented cap
// and must be accepted; one cent more must not be.
func TestParseCeiling(t *testing.T) {
	if _, err := Parse("1000000000.00"); err != nil {
		t.Errorf("the documented maximum was refused: %v", err)
	}
	for _, over := range []string{
		"1000000000.01",
		"1000000001",
		"9223372036854775807",
		"99999999999999999999",
	} {
		if got, err := Parse(over); err == nil {
			t.Errorf("Parse(%q) = %d cents, want an error", over, int64(got))
		}
	}
}
