package ocr

// This file is a measurement harness, not part of the shipped test suite in
// spirit: it needs the tesseract binary and a directory of sample images, and
// skips itself when either is missing. It exists to answer one question with
// data rather than opinion -- does the preprocessing in preprocess.go actually
// improve OCR accuracy, or is it ceremony?

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/money"
)

type harshCase struct {
	File  string `json:"file"`
	Level int    `json:"level"`
	Truth struct {
		Merchant string      `json:"merchant"`
		Date     string      `json:"date"`
		Total    money.Cents `json:"total"`
	} `json:"truth"`
}

func TestPreprocessingIsWorthIt(t *testing.T) {
	dir := os.Getenv("YABA_OCR_SAMPLES")
	if dir == "" {
		t.Skip("set YABA_OCR_SAMPLES to a directory of sample receipts to run")
	}
	e := NewEngine()
	if !e.Available() {
		t.Skip("no tesseract binary")
	}

	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var cases []harshCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	work := t.TempDir()
	ctx := context.Background()

	type tally struct{ total, date, ok int }
	var raw, gray, bin tally

	fmt.Printf("\n%-18s %-10s %-10s %-10s %s\n", "SAMPLE", "RAW", "GREY", "SAUVOLA", "TRUTH")
	for _, c := range cases {
		src := filepath.Join(dir, c.File)

		run := func(path string) Receipt {
			text, conf, err := e.recognise(ctx, path)
			if err != nil {
				return Receipt{}
			}
			return Parse(text, conf)
		}

		rRaw := run(src)

		gp := filepath.Join(work, "g-"+c.File+".png")
		if err := prepare(src, gp, false); err != nil {
			t.Fatalf("prepare(grey) %s: %v", c.File, err)
		}
		rGray := run(gp)

		bp := filepath.Join(work, "b-"+c.File+".png")
		if err := prepare(src, bp, true); err != nil {
			t.Fatalf("prepare(sauvola) %s: %v", c.File, err)
		}
		rBin := run(bp)

		count := func(r Receipt, tl *tally) {
			if r.Total == c.Truth.Total {
				tl.total++
			}
			if r.Date == c.Truth.Date {
				tl.date++
			}
			if r.Total == c.Truth.Total && r.Date == c.Truth.Date {
				tl.ok++
			}
		}
		count(rRaw, &raw)
		count(rGray, &gray)
		count(rBin, &bin)

		mark := func(r Receipt) string {
			if r.Total == c.Truth.Total {
				return r.Total.Display()
			}
			return r.Total.Display() + " X"
		}
		fmt.Printf("%-18s %-10s %-10s %-10s %s\n", c.File,
			mark(rRaw), mark(rGray), mark(rBin), c.Truth.Total.Display())
	}

	n := len(cases)
	fmt.Printf("\ntotals correct   raw %d/%d   grey %d/%d   sauvola %d/%d\n",
		raw.total, n, gray.total, n, bin.total, n)
	fmt.Printf("dates correct    raw %d/%d   grey %d/%d   sauvola %d/%d\n",
		raw.date, n, gray.date, n, bin.date, n)
	fmt.Printf("both correct     raw %d/%d   grey %d/%d   sauvola %d/%d\n\n",
		raw.ok, n, gray.ok, n, bin.ok, n)

	// The pipeline must not be worse than doing nothing. This is the assertion
	// that keeps preprocess.go honest if anyone tunes it later.
	best := gray.ok
	if bin.ok > best {
		best = bin.ok
	}
	if best < raw.ok {
		t.Errorf("preprocessing made things worse: raw %d, grey %d, sauvola %d",
			raw.ok, gray.ok, bin.ok)
	}
}
