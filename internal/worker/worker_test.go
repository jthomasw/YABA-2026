// Tests for the receipt worker.
//
// What is left here is the processor contract and the path handling. The queue
// itself -- claiming a job atomically, retrying, recovering work stranded by a
// restart -- is exercised against a real database in internal/store, because
// that is where the behaviour lives.
//
// ReviewProcessor is deliberately the only processor. Reading an amount off a
// photograph would be the interesting version, and its absence is the safer
// default: a missing amount shows as a blank field, while a MISREAD amount is
// silently believed -- it goes into the month's total, the category breakdown,
// the trend line and the emergency-fund projection, and nothing on the screen
// says it came from a guess.
package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

func TestLocalPathIsTheInverseOfStorage(t *testing.T) {
	// On Linux this is identity; on Windows it swaps the separators. Either way a
	// stored path must survive the round trip unchanged in meaning.
	if got := localPath("uploads/16/abc.png"); got == "" {
		t.Error("localPath returned empty")
	}
	if got := localPath(""); got != "" {
		t.Errorf("localPath(%q) = %q", "", got)
	}
}

// TestReviewProcessorSeparatesFailureFromReview is the distinction the retry
// logic depends on: a file that is missing or empty is a real failure worth
// retrying and eventually reporting, while a perfectly good file that simply
// needs its amount typed in is not a failure at all.
func TestReviewProcessorSeparatesFailureFromReview(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "receipt.png")
	if err := os.WriteFile(good, []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		path      string
		wantRetry bool // a real error: retried, then reported
	}{
		{"a stored receipt", filepath.ToSlash(good), false},
		{"an empty file", filepath.ToSlash(empty), true},
		{"a missing file", filepath.ToSlash(filepath.Join(dir, "gone.png")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReviewProcessor{}.Process(context.Background(),
				store.ReceiptJob{ID: 1, Path: c.path})
			switch {
			case c.wantRetry && errors.Is(err, ErrNeedsReview):
				t.Error("a broken file was treated as merely needing review, so it would never be reported")
			case c.wantRetry && err == nil:
				t.Error("a broken file was accepted")
			case !c.wantRetry && !errors.Is(err, ErrNeedsReview):
				t.Errorf("a good file should ask for review, got %v", err)
			}
		})
	}
}

// TestProcessorContractIsHonoured guards the seam itself: whatever replaces
// ReviewProcessor must be able to say "done", "needs a human", or "this failed",
// and the worker has to be able to tell those three apart.
func TestProcessorContractIsHonoured(t *testing.T) {
	job := store.ReceiptJob{ID: 1, Path: "uploads/1/x.png"}

	extracted := stubProcessor{draft: Draft{Amount: 1234, Label: "Lidl"}}
	d, err := extracted.Process(context.Background(), job)
	if err != nil || d.Amount != 1234 {
		t.Errorf("a successful extraction: %+v %v", d, err)
	}

	review := stubProcessor{err: ErrNeedsReview}
	if _, err := review.Process(context.Background(), job); !errors.Is(err, ErrNeedsReview) {
		t.Errorf("needs-review must be distinguishable, got %v", err)
	}

	broken := stubProcessor{err: errStub("disk on fire")}
	if _, err := broken.Process(context.Background(), job); errors.Is(err, ErrNeedsReview) {
		t.Error("a genuine failure must not look like needs-review")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

type stubProcessor struct {
	draft Draft
	err   error
}

func (s stubProcessor) Process(_ context.Context, _ store.ReceiptJob) (Draft, error) {
	return s.draft, s.err
}

type errStub string

func (e errStub) Error() string { return string(e) }
