package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/jthomasw/YABA-2026/internal/ocr"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// OCRProcessor reads an amount off a receipt using the ocr package. It is the
// interesting implementation of the Processor seam that ReviewProcessor left
// open, and it plugs in without the worker knowing anything has changed.
type OCRProcessor struct {
	Engine *ocr.Engine
}

// NewOCRProcessor builds a processor around whatever tools the machine has. It
// returns nil when OCR cannot run at all, which the caller turns back into
// ReviewProcessor -- so a server without tesseract behaves exactly as it did
// before, rather than failing every upload.
func NewOCRProcessor() *OCRProcessor {
	e := ocr.NewEngine()
	log.Printf("receipts: %s", e.Describe())
	if !e.Available() {
		return nil
	}
	return &OCRProcessor{Engine: e}
}

// Process reads the stored image and proposes a draft.
//
// The three outcomes the worker distinguishes are preserved exactly:
//
//   - a real error, worth retrying and eventually reporting, for a file that is
//     missing, empty or unreadable as a file;
//   - ErrNeedsReview for a perfectly good file whose contents could not be
//     turned into an amount, which is not a failure and must never be retried
//     into a failure notification;
//   - success, for a draft with an amount in it.
func (p OCRProcessor) Process(ctx context.Context, job store.ReceiptJob) (Draft, error) {
	path := localPath(job.Path)

	// The same file checks ReviewProcessor makes, first and for the same reason:
	// a missing file is a genuine failure and must be retried, whereas an
	// illegible one never will be.
	info, err := os.Stat(path)
	if err != nil {
		return Draft{}, fmt.Errorf("stored receipt is unreadable: %w", err)
	}
	if info.Size() == 0 {
		return Draft{}, errors.New("stored receipt is empty")
	}

	receipt, err := p.Engine.Read(ctx, path)
	switch {
	case errors.Is(err, ocr.ErrNoEngine):
		// The binary went away between startup and now.
		return Draft{}, ErrNeedsReview

	case errors.Is(err, ocr.ErrUnsupportedImage):
		// A format this server cannot decode -- an iPhone HEIC without
		// ImageMagick installed, say. The file is fine and the user can still
		// describe it, so this asks for review rather than reporting a failure
		// they can do nothing about.
		log.Printf("worker: receipt job %d is in a format this server cannot read: %v",
			job.ID, err)
		return Draft{}, ErrNeedsReview

	case err != nil:
		// Something genuinely went wrong: tesseract crashed, the timeout fired,
		// the disk is full. Worth a retry.
		return Draft{}, err
	}

	d := toDraft(receipt)

	log.Printf("worker: receipt job %d read %s (confidence %.2f, %d items): %s",
		job.ID, receipt.Total.Display(), receipt.Confidence, len(receipt.Items),
		receipt.Summary())

	if !receipt.HasTotal() {
		// Everything else read -- merchant, date, raw text -- is still returned,
		// so the form is prefilled with what there is even though the amount has
		// to be typed.
		return d, ErrNeedsReview
	}
	return d, nil
}

// toDraft maps the ocr package's result onto the worker's contract.
//
// The mapping onto YABA's five Ws is deliberate: the merchant is the "Who?", so
// it becomes the payee, and the guessed category is the "What?", so it becomes
// the label. Getting these the wrong way round would put "Groceries" in the
// payee field and a shop name in the category, and the category is what every
// breakdown chart groups by.
func toDraft(r ocr.Receipt) Draft {
	items := make([]store.DraftItem, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, store.DraftItem{
			Description: it.Description,
			Amount:      it.Amount,
		})
	}

	return Draft{
		Label:      r.Category,
		Payee:      r.Merchant,
		Amount:     r.Total,
		Date:       r.Date,
		Subtotal:   r.Subtotal,
		Tax:        r.Tax,
		Tip:        r.Tip,
		Items:      items,
		Confidence: r.Confidence,
		Text:       r.Text,
	}
}
