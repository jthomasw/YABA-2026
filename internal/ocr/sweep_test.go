package ocr

// A parameter sweep over the working resolution and the binarisation switch.
// Downscaling is the cheapest denoiser there is -- a box filter averages every
// source pixel in the target cell -- so the working resolution is the single
// most important knob in the pipeline, and it deserves to be chosen by
// measurement rather than by taste.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/money"
)

func TestSweepWorkingResolution(t *testing.T) {
	dir := os.Getenv("YABA_OCR_SWEEP")
	if dir == "" {
		t.Skip("set YABA_OCR_SWEEP to a directory of sample receipts to run")
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

	targets := []int{800, 1100, 1400, 1800}
	fmt.Printf("\n%-20s", "SAMPLE")
	for _, tg := range targets {
		fmt.Printf(" %-13s", fmt.Sprintf("%d/grey", tg))
		fmt.Printf(" %-13s", fmt.Sprintf("%d/bin", tg))
	}
	fmt.Println("  TRUTH")

	type key struct {
		target int
		bin    bool
	}
	hits := map[key]int{}

	for _, c := range cases {
		src := filepath.Join(dir, c.File)
		fmt.Printf("%-20s", c.File)

		for _, tg := range targets {
			for _, bin := range []bool{false, true} {
				name := fmt.Sprintf("%s-%d-%v.png", c.File, tg, bin)
				dst := filepath.Join(work, name)
				if err := prepareWith(src, dst, tg, bin); err != nil {
					t.Fatalf("prepare %s: %v", name, err)
				}
				// One segmentation mode only: the sweep is comparing
				// preprocessing, and running both would double the runtime
				// while changing every column equally.
				text, conf, _, err := e.runTesseract(ctx, dst, "6")
				var got money.Cents
				if err == nil {
					got = Parse(text, conf).Total
				}
				mark := got.Display()
				if got == c.Truth.Total {
					hits[key{tg, bin}]++
				} else {
					mark += "X"
				}
				fmt.Printf(" %-13s", mark)
			}
		}
		fmt.Printf("  %s\n", c.Truth.Total.Display())
	}

	fmt.Printf("\n%-12s %-8s %s\n", "TARGET", "MODE", "TOTALS CORRECT")
	bestKey, bestHits := key{}, -1
	for _, tg := range targets {
		for _, bin := range []bool{false, true} {
			k := key{tg, bin}
			mode := "grey"
			if bin {
				mode = "sauvola"
			}
			fmt.Printf("%-12d %-8s %d/%d\n", tg, mode, hits[k], len(cases))
			if hits[k] > bestHits {
				bestKey, bestHits = k, hits[k]
			}
		}
	}
	fmt.Printf("\nbest: target=%d binarise=%v with %d/%d\n\n",
		bestKey.target, bestKey.bin, bestHits, len(cases))
}
