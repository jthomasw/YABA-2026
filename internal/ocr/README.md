# Receipt OCR

Reads the amount, date, merchant and line items off a photographed receipt and
proposes them as a **draft** the user confirms. Nothing here writes to the
ledger.

## The one design decision worth knowing

OCR never creates a transaction. The worker reads the receipt, saves what it
found against the `receipt_jobs` row, and notifies the user with a link to a
prefilled expense form. A transaction exists only after somebody presses Save.

This is not caution for its own sake. On a hard photograph the parser reads
`$45.78` for a `$45.70` receipt — wrong in a way that looks entirely reasonable
and would never be noticed again once it was inside a month's total, a category
breakdown, a trend line and an emergency-fund projection. The old
`worker_test.go` header called this out as the reason not to attempt extraction
at all; confirm-before-save is the answer to it.

## How it works

```
upload ─▶ receipt_jobs (queued)
             │
             ▼  background worker
        pageImages()      PDF → pdftoppm, HEIC/WebP → ImageMagick, else as-is
             │
             ▼
        prepare()         greyscale → downscale to 1100px → contrast stretch
             │            → Sauvola local threshold
             ▼
        tesseract         PSM 6 and PSM 4, TSV output, better result wins
             │
             ▼
        Parse()           label-ranked total, date, merchant, items, confidence
             │
             ▼
        SaveReceiptDraft()  ── notification ──▶  /transactions/new?receipt=N
```

### Preprocessing is where the accuracy is

Measured on deliberately hard receipt photos (noisy, unevenly lit, skewed):

| pipeline | totals read correctly |
|---|---|
| raw image straight to tesseract | 5/12 |
| greyscale + downscale only | 6/12 |
| **full pipeline (greyscale + downscale + Sauvola)** | **10/12** |

The working resolution matters more than anything else, because downscaling with
a box filter is the cheapest denoiser there is. A sweep over it:

| target long edge | greyscale only | with Sauvola |
|---|---|---|
| 800 | 0/6 | 3/6 |
| **1100** | 0/6 | **4/6** |
| 1400 | 0/6 | 1/6 |
| 1800 | 0/6 | 0/6 |

Above ~1400 the pixel noise survives and recognition collapses. Greyscale alone
scores zero at every resolution, because tesseract's own global threshold cannot
handle a page brighter at the top than the bottom. Both tables are reproducible
with `sweep_test.go` and `bench_test.go`.

### Picking the total is a ranking problem, not a maximum

Choosing the largest amount on the receipt fails on three of six real samples:

- a hardware receipt shows `CASH 60.00` against an `AMOUNT DUE 58.79`
- a pharmacy receipt shows `Total 30.93` above a settled `BALANCE DUE 0.00`
  — which OCR misread as `8.00`
- a fuel receipt shows `GALLONS 11,482` and `PRICE/GAL 3.399`

So amounts must have exactly two decimal places to count as money at all, and
candidates are ranked by label tier (`grand total` > `total` > `balance due`),
never by size. Confidence is raised sharply when `subtotal + tax + tip == total`,
because three independently OCR'd numbers do not agree by accident.

## Requirements on the server

| tool | needed for | without it |
|---|---|---|
| `tesseract` | everything | OCR disabled; receipts queue for manual entry exactly as before |
| `poppler-utils` (`pdftoppm`) | PDF receipts | PDFs queue for manual entry |
| `ImageMagick` or `libheif` | HEIC and WebP | those formats queue for manual entry |

Every one of these degrades rather than failing. The startup log says which are
present — look for the `receipts:` line.

```sh
# Amazon Linux 2023 (the EC2 box)
sudo dnf install -y tesseract poppler-utils ImageMagick
```

No cgo, no `libtesseract` linkage: the engine shells out, so the single static
binary and the cross-compile from Windows both survive.

## Building and testing

```sh
go build ./... && go test ./...          # the package's own tests need no tesseract
```

`parse_test.go` runs against **captured** tesseract output checked into
`testdata/`, so it is hermetic and cannot break when a different tesseract
version reads a character differently.

The two measurement harnesses skip unless pointed at sample images:

```sh
YABA_OCR_SAMPLES=/path/to/samples go test ./internal/ocr/ -run Preprocessing -v
YABA_OCR_SWEEP=/path/to/samples   go test ./internal/ocr/ -run Sweep -v
```

Each sample directory needs a `manifest.json` of
`[{"file": "x.jpg", "level": 2, "truth": {"total": 4719, "date": "2026-03-14"}}]`.

## Tuning

Everything worth adjusting is a named constant with the measurement that chose it
in the comment above it:

- `targetLongEdge` (preprocess.go) — the working resolution, and the highest-leverage knob
- `k` in `sauvola()` — threshold aggressiveness; 0.34 suits dark text on white receipt paper
- `totalTiers` / `neverTotal` / `summaryTerms` (parse.go) — the label vocabularies
- `DefaultTimeout`, `DefaultMaxPages` (tesseract.go)

`neverTotal` and `summaryTerms` are deliberately **not** the same list. The first
stops a number being mistaken for the amount charged and rightly contains `gal`;
reusing it for the second ended the item scan at `WHOLE MILK 1GAL`.

## Swapping in a cloud OCR service later

`worker.Processor` is the seam. `OCRProcessor` implements it with tesseract; an
AWS Textract `AnalyzeExpense` implementation would return the same `Draft` and
need no changes anywhere else. Textract is materially more accurate on real
receipts and costs about a cent a page.
