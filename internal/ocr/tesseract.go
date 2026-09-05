package ocr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The engine shells out to the tesseract binary rather than binding to
// libtesseract through cgo. That is a deliberate trade: cgo would be marginally
// faster per page, but it would end the project's single static binary, make
// cross-compiling from Windows to Linux painful, and add a C toolchain to a
// build that currently needs nothing but Go. Receipt OCR happens a few times a
// day in a background worker, so process startup is not the bottleneck.

// DefaultTimeout bounds one page. Tesseract on a t3.micro takes a second or two
// for a receipt; anything approaching a minute means it is thrashing on an
// image it will not read anyway.
const DefaultTimeout = 45 * time.Second

// DefaultMaxPages caps a multi-page PDF. Nobody photographs a 200-page receipt,
// and without a cap one malformed file could occupy the worker indefinitely.
const DefaultMaxPages = 5

// Engine reads receipts using external tools discovered on PATH.
type Engine struct {
	// Tesseract is the OCR binary. Empty means it was not found, and the engine
	// reports itself unavailable rather than failing at the first upload.
	Tesseract string

	// Pdftoppm rasterises PDFs. Part of poppler-utils. Optional: without it,
	// PDF receipts fall back to manual entry.
	Pdftoppm string

	// Convert handles the formats Go cannot decode -- HEIC from an iPhone, and
	// WebP. ImageMagick's `convert` or `magick`. Optional.
	Convert string

	Lang     string
	Timeout  time.Duration
	MaxPages int

	// Binarise applies the Sauvola threshold before OCR. On by default: it is
	// what makes an unevenly lit photo readable.
	Binarise bool
}

// NewEngine discovers the available tools. It never fails: an engine with no
// tesseract is simply unavailable, and the worker then behaves exactly as it
// did before OCR existed.
func NewEngine() *Engine {
	e := &Engine{
		Lang:     "eng",
		Timeout:  DefaultTimeout,
		MaxPages: DefaultMaxPages,
		Binarise: true,
	}
	e.Tesseract = lookPath("tesseract")
	e.Pdftoppm = lookPath("pdftoppm")
	// magick is ImageMagick 7's entry point; convert is ImageMagick 6's.
	// heif-convert ships with libheif and handles iPhone photos on its own.
	for _, name := range []string{"magick", "convert", "heif-convert"} {
		if p := lookPath(name); p != "" {
			e.Convert = p
			break
		}
	}
	return e
}

func lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Available reports whether OCR can be attempted at all.
func (e *Engine) Available() bool { return e != nil && e.Tesseract != "" }

// Describe summarises the engine for the startup log, so a deployment missing
// poppler is visible immediately rather than the first time somebody uploads a
// PDF.
func (e *Engine) Describe() string {
	if !e.Available() {
		return "OCR disabled: no tesseract binary on PATH — " +
			"receipts will be queued for manual entry"
	}
	parts := []string{"tesseract " + e.Tesseract}
	if e.Pdftoppm != "" {
		parts = append(parts, "pdf via "+filepath.Base(e.Pdftoppm))
	} else {
		parts = append(parts, "no PDF support (install poppler-utils)")
	}
	if e.Convert != "" {
		parts = append(parts, "heic/webp via "+filepath.Base(e.Convert))
	} else {
		parts = append(parts, "no HEIC/WebP support (install ImageMagick)")
	}
	return "OCR enabled: " + strings.Join(parts, ", ")
}

// ErrNoEngine means OCR was asked for on a machine that cannot do it.
var ErrNoEngine = errors.New("no OCR engine available")

// Read runs the whole pipeline over one stored receipt and returns the draft.
// A file it cannot read is not an error: it returns a Receipt with no total,
// which the caller presents to the user to complete by hand.
func (e *Engine) Read(ctx context.Context, path string) (Receipt, error) {
	if !e.Available() {
		return Receipt{}, ErrNoEngine
	}

	work, err := os.MkdirTemp("", "yaba-ocr-")
	if err != nil {
		return Receipt{}, fmt.Errorf("create work directory: %w", err)
	}
	// The whole directory goes at the end, so a page image cannot outlive the
	// job even if a later step fails.
	defer os.RemoveAll(work)

	pages, err := e.pageImages(ctx, path, work)
	if err != nil {
		return Receipt{}, err
	}
	if len(pages) == 0 {
		return Receipt{}, ErrUnsupportedImage
	}

	var texts []string
	var confs []float64

	for i, page := range pages {
		prepared := filepath.Join(work, fmt.Sprintf("prep-%d.png", i))
		if err := prepare(page, prepared, e.Binarise); err != nil {
			// A page that will not decode is skipped rather than failing the
			// whole receipt: page two being corrupt should not lose page one.
			if errors.Is(err, ErrUnsupportedImage) {
				continue
			}
			return Receipt{}, err
		}

		text, conf, err := e.recognise(ctx, prepared)
		if err != nil {
			return Receipt{}, err
		}
		if strings.TrimSpace(text) != "" {
			texts = append(texts, text)
			confs = append(confs, conf)
		}
	}

	combined := strings.Join(texts, "\n")
	return Parse(combined, medianOf(confs)), nil
}

// recognise runs tesseract twice with different page-segmentation modes and
// keeps the better result. PSM 6 treats the image as one uniform block, which
// suits a receipt photographed square-on; PSM 4 assumes variable-width columns
// and rescues the ones that are skewed or shot at an angle. Running both costs
// about a second and turns a fair number of failures into successes.
func (e *Engine) recognise(ctx context.Context, imgPath string) (string, float64, error) {
	type attempt struct {
		text  string
		conf  float64
		words int
	}
	var best attempt
	var firstErr error

	for _, psm := range []string{"6", "4"} {
		text, conf, words, err := e.runTesseract(ctx, imgPath, psm)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// More words at comparable confidence means more of the receipt was
		// read. Confidence alone would prefer an attempt that found three
		// characters it was very sure about.
		if score := float64(words) * (0.5 + conf); score > float64(best.words)*(0.5+best.conf) {
			best = attempt{text: text, conf: conf, words: words}
		}
	}

	if best.words == 0 && firstErr != nil {
		return "", 0, firstErr
	}
	return best.text, best.conf, nil
}

// runTesseract invokes the binary in TSV mode, which yields the recognised text
// and a per-word confidence in one pass. The plain text output would need a
// second run to get confidences at all.
func (e *Engine) runTesseract(ctx context.Context, imgPath, psm string) (string, float64, int, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lang := e.Lang
	if lang == "" {
		lang = "eng"
	}

	cmd := exec.CommandContext(ctx, e.Tesseract, imgPath, "stdout",
		"--psm", psm, "-l", lang, "tsv")
	// Tesseract writes progress and warnings to stderr and would otherwise
	// inherit the server's, filling the journal with noise on every receipt.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", 0, 0, fmt.Errorf("tesseract timed out after %s", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", 0, 0, fmt.Errorf("tesseract failed: %s", firstLine(msg))
	}

	text, conf, words := parseTSV(string(out))
	return text, conf, words, nil
}

// parseTSV reconstructs lines from tesseract's tab-separated word list.
//
// The columns are:
//
//	level page block par line word left top width height conf text
//
// Words carry conf -1 when the row describes a block or a line rather than a
// word, so those rows are skipped for the confidence average but still tell us
// where the line breaks go.
func parseTSV(tsv string) (text string, confidence float64, words int) {
	sc := bufio.NewScanner(strings.NewReader(tsv))
	// A dense receipt produces long rows; the default 64KB token limit is
	// generous but a single pathological line should not end the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type key struct{ block, par, line int }
	var order []key
	byLine := map[key][]string{}
	var confs []float64

	first := true
	for sc.Scan() {
		row := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(row, "level\t") {
				continue // header
			}
		}
		cols := strings.Split(row, "\t")
		if len(cols) < 12 {
			continue
		}
		word := strings.TrimSpace(cols[11])
		if word == "" {
			continue
		}
		conf, err := strconv.ParseFloat(cols[10], 64)
		if err != nil || conf < 0 {
			continue
		}

		block, _ := strconv.Atoi(cols[2])
		par, _ := strconv.Atoi(cols[3])
		line, _ := strconv.Atoi(cols[4])
		k := key{block, par, line}

		if _, seen := byLine[k]; !seen {
			order = append(order, k)
		}
		byLine[k] = append(byLine[k], word)
		confs = append(confs, conf/100)
		words++
	}

	// Sort the lines into reading order. Tesseract emits them in order already,
	// but relying on that would break silently if it ever stopped being true.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.block != b.block {
			return a.block < b.block
		}
		if a.par != b.par {
			return a.par < b.par
		}
		return a.line < b.line
	})

	var b strings.Builder
	for _, k := range order {
		b.WriteString(strings.Join(byLine[k], " "))
		b.WriteByte('\n')
	}
	return b.String(), medianOf(confs), words
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ── turning a stored file into page images ────────────────────────────────────

// pageImages produces one image file per page. Most receipts are a single
// photo and this returns the original path untouched.
func (e *Engine) pageImages(ctx context.Context, path, work string) ([]string, error) {
	kind, err := sniffFile(path)
	if err != nil {
		return nil, err
	}

	switch kind {
	case KindPDF:
		return e.rasterisePDF(ctx, path, work)

	case KindHEIC, KindWebP:
		// Go's standard library decodes neither. Convert to PNG if a converter
		// is installed, and otherwise report the format as unsupported so the
		// receipt is queued for manual entry with a clear reason.
		if e.Convert == "" {
			return nil, fmt.Errorf("%w: %s needs ImageMagick or libheif installed on the server",
				ErrUnsupportedImage, kind)
		}
		dst := filepath.Join(work, "converted.png")
		if err := e.convertImage(ctx, path, dst); err != nil {
			return nil, err
		}
		return []string{dst}, nil

	case KindUnknown:
		return nil, ErrUnsupportedImage

	default:
		return []string{path}, nil
	}
}

// rasterisePDF turns each page into a PNG with pdftoppm.
func (e *Engine) rasterisePDF(ctx context.Context, path, work string) ([]string, error) {
	if e.Pdftoppm == "" {
		return nil, fmt.Errorf("%w: PDF receipts need poppler-utils installed on the server",
			ErrUnsupportedImage)
	}

	maxPages := e.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// A multi-page document needs proportionally longer than a single image.
	ctx, cancel := context.WithTimeout(ctx, timeout*time.Duration(maxPages))
	defer cancel()

	prefix := filepath.Join(work, "page")
	// 200 DPI is the sweet spot: 150 loses the small print on a thermal
	// receipt, and 300 doubles the pixel count for very little gain.
	cmd := exec.CommandContext(ctx, e.Pdftoppm,
		"-png", "-r", "200",
		"-f", "1", "-l", strconv.Itoa(maxPages),
		path, prefix)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("rasterising the PDF timed out")
		}
		return nil, fmt.Errorf("could not read the PDF: %s", firstLine(strings.TrimSpace(stderr.String())))
	}

	// pdftoppm names its output page-1.png, page-01.png or page-001.png
	// depending on the page count, so the files are found by glob rather than
	// by reconstructing the name.
	matches, err := filepath.Glob(prefix + "-*.png")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) > maxPages {
		matches = matches[:maxPages]
	}
	return matches, nil
}

// convertImage shells out for the formats Go cannot decode.
func (e *Engine) convertImage(ctx context.Context, src, dst string) error {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := filepath.Base(e.Convert)
	var cmd *exec.Cmd
	switch {
	case strings.HasPrefix(base, "heif-convert"):
		// heif-convert takes plain in/out arguments and infers the output
		// format from the extension.
		cmd = exec.CommandContext(ctx, e.Convert, src, dst)
	case strings.HasPrefix(base, "magick"):
		cmd = exec.CommandContext(ctx, e.Convert, src, dst)
	default:
		cmd = exec.CommandContext(ctx, e.Convert, src, dst)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("converting the image timed out")
		}
		return fmt.Errorf("%w: %s", ErrUnsupportedImage,
			firstLine(strings.TrimSpace(stderr.String())))
	}
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("%w: the converter produced no output", ErrUnsupportedImage)
	}
	return nil
}
