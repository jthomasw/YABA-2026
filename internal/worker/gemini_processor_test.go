package worker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// writeTempReceipt writes bytes that sniff as a JPEG (Gemini's supported-kind
// check runs before any HTTP call is made, so the body only needs a real JPEG
// header) and returns its path.
func writeTempReceipt(t *testing.T, dir string, body []byte) string {
	t.Helper()
	full := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	full = append(full, body...)
	p := filepath.Join(dir, "receipt.jpg")
	if err := os.WriteFile(p, full, 0o600); err != nil {
		t.Fatalf("write temp receipt: %v", err)
	}
	return p
}

// stubGemini starts a test server standing in for the real API and points the
// processor at it for the duration of the test.
func stubGemini(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := geminiBaseURL
	geminiBaseURL = srv.URL
	t.Cleanup(func() { geminiBaseURL = old })
}

func geminiReply(t *testing.T, extract geminiExtract) []byte {
	t.Helper()
	text, err := json.Marshal(extract)
	if err != nil {
		t.Fatalf("marshal extract: %v", err)
	}
	reply := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{"text": string(text)},
					},
				},
			},
		},
	}
	out, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	return out
}

func TestGeminiProcessorReadsALegibleReceipt(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("not a real jpeg body, just needs the header"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("api key header = %q, want test-key", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Write(geminiReply(t, geminiExtract{
			Legible:    true,
			Merchant:   "Trader Joe's",
			Category:   "Groceries",
			Date:       "2026-09-12",
			Total:      "45.78",
			Subtotal:   "42.10",
			Tax:        "3.68",
			Confidence: 0.92,
			Items: []geminiLineOut{
				{Description: "Bananas", Amount: "1.99"},
				{Description: "Oat milk", Amount: "4.29"},
			},
		}))
	})

	p := NewGeminiProcessor("test-key", "")
	draft, err := p.Process(t.Context(), store.ReceiptJob{ID: 1, Path: path})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if draft.Amount != 4578 {
		t.Errorf("Amount = %d, want 4578", draft.Amount)
	}
	if draft.Payee != "Trader Joe's" {
		t.Errorf("Payee = %q, want Trader Joe's", draft.Payee)
	}
	if draft.Label != "Groceries" {
		t.Errorf("Label = %q, want Groceries", draft.Label)
	}
	if len(draft.Items) != 2 || draft.Items[0].Amount != 199 {
		t.Errorf("Items = %+v", draft.Items)
	}
	if draft.Confidence != 0.92 {
		t.Errorf("Confidence = %v, want 0.92", draft.Confidence)
	}
}

func TestGeminiProcessorAsksForReviewWhenIllegible(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("blurry"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(geminiReply(t, geminiExtract{Legible: false}))
	})

	p := NewGeminiProcessor("test-key", "")
	draft, err := p.Process(t.Context(), store.ReceiptJob{ID: 2, Path: path})
	if err != ErrNeedsReview {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}
	// No amount was read, so this genuinely needs review -- but the category
	// fallback still applies even here: whatever gets saved against this job
	// prefills the review form, and a blank category there would block the
	// save exactly the same way a blank category on a "successful" read would.
	if draft.Amount != 0 {
		t.Errorf("Amount = %d, want 0", draft.Amount)
	}
	if draft.Label != fallbackCategory {
		t.Errorf("Label = %q, want Uncategorised", draft.Label)
	}
}

func TestGeminiProcessorNeverGuessesWhenNoTotalIsReported(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("faded receipt"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(geminiReply(t, geminiExtract{
			Legible:  true,
			Merchant: "Shell",
			Total:    "", // nothing confident enough to report
		}))
	})

	p := NewGeminiProcessor("test-key", "")
	draft, err := p.Process(t.Context(), store.ReceiptJob{ID: 3, Path: path})
	if err != ErrNeedsReview {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}
	if draft.Amount != 0 {
		t.Errorf("Amount = %d, want 0", draft.Amount)
	}
	if draft.Payee != "Shell" {
		t.Errorf("Payee = %q, want Shell (whatever was legible should still be kept)", draft.Payee)
	}
}

func TestGeminiProcessorTreatsASafetyBlockAsIllegible(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("something the filter refused"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"promptFeedback": {"blockReason": "SAFETY"}}`))
	})

	p := NewGeminiProcessor("test-key", "")
	_, err := p.Process(t.Context(), store.ReceiptJob{ID: 4, Path: path})
	if err != ErrNeedsReview {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}
}

func TestGeminiProcessorRetriesOnAnAPIError(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("fine, but the API is down"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error": {"message": "model overloaded"}}`))
	})

	p := NewGeminiProcessor("test-key", "")
	_, err := p.Process(t.Context(), store.ReceiptJob{ID: 5, Path: path})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if err == ErrNeedsReview {
		t.Fatal("a server error must be retried, not treated as an illegible receipt")
	}
}

func TestGeminiProcessorSkipsAFormatItDoesNotSend(t *testing.T) {
	dir := t.TempDir()
	// A GIF header: not in geminiSupportedKinds, so no HTTP call should happen.
	p := filepath.Join(dir, "receipt.gif")
	if err := os.WriteFile(p, []byte("GIF89a"), 0o600); err != nil {
		t.Fatalf("write temp receipt: %v", err)
	}

	called := false
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	proc := NewGeminiProcessor("test-key", "")
	_, err := proc.Process(t.Context(), store.ReceiptJob{ID: 6, Path: p})
	if err != ErrNeedsReview {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}
	if called {
		t.Error("Gemini should not have been called for an unsupported format")
	}
}

func TestGeminiProcessorNeverLeavesTheCategoryBlank(t *testing.T) {
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("a receipt of something nobody could categorise"))

	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(geminiReply(t, geminiExtract{
			Legible: true,
			Total:   "9.00",
			// Category deliberately left empty, as if the model omitted it.
		}))
	})

	p := NewGeminiProcessor("test-key", "")
	draft, err := p.Process(t.Context(), store.ReceiptJob{ID: 7, Path: path})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if draft.Label != fallbackCategory {
		t.Errorf("Label = %q, want Uncategorised -- the expense form requires a category and must never come back blocked on one", draft.Label)
	}
	if draft.Category != fallbackCategory {
		t.Errorf("Category = %q, want Uncategorised", draft.Category)
	}
}

func TestNewGeminiProcessorRequiresAKey(t *testing.T) {
	if NewGeminiProcessor("", "") != nil {
		t.Error("expected nil processor with no API key, so a deployment without a key falls back to manual entry")
	}
	if p := NewGeminiProcessor("k", ""); p.Model != DefaultGeminiModel {
		t.Errorf("Model = %q, want default %q", p.Model, DefaultGeminiModel)
	}
}

func TestParseGeminiAmountNeverGuesses(t *testing.T) {
	cases := map[string]store.Cents{
		"":        0,
		"12.34":   1234,
		"$12.34":  1234,
		"junk":    0,
		"-3.00":   -300,
		"1,234.5": 123450,
	}
	for in, want := range cases {
		if got := parseGeminiAmount(in); got != want {
			t.Errorf("parseGeminiAmount(%q) = %d, want %d", in, got, want)
		}
	}
}
