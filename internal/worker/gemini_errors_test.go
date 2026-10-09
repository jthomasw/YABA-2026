// Tests for how GeminiProcessor tells a misconfiguration from a bad minute at
// Google, and for the checks that stop a request that can never succeed from
// being sent at all.
package worker

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// TestGeminiErrorsAreClassifiedByStatus: a request Google refuses outright is
// never retried and ends on the manual-entry form, while anything that might
// succeed a minute later stays a retryable failure.
func TestGeminiErrorsAreClassifiedByStatus(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		permanent bool
	}{
		{http.StatusBadRequest, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, true},
		{http.StatusUnauthorized, `{"error":{"message":"unauthenticated"}}`, true},
		{http.StatusForbidden, `{"error":{"message":"permission denied"}}`, true},
		{http.StatusNotFound, `{"error":{"message":"models/Google Gemini Flash 3.1 is not found"}}`, true},
		{http.StatusRequestEntityTooLarge, ``, true},
		{http.StatusUnprocessableEntity, `not json at all`, true},

		{http.StatusRequestTimeout, ``, false},
		{http.StatusTooManyRequests, `{"error":{"message":"quota"}}`, false},
		{http.StatusInternalServerError, `{"error":{"message":"internal"}}`, false},
		{http.StatusBadGateway, `<html><body>502 Bad Gateway</body></html>`, false},
		{http.StatusServiceUnavailable, `{"error":{"message":"model overloaded"}}`, false},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			path := writeTempReceipt(t, t.TempDir(), []byte("body"))
			stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			logs := captureLog(t)

			_, err := NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 9, Path: path})

			if c.permanent {
				if !errors.Is(err, ErrNeedsReview) {
					t.Fatalf("err = %v, want ErrNeedsReview: status %d can never succeed on a retry", err, c.status)
				}
				if !strings.Contains(logs.String(), "ERROR") {
					t.Errorf("a permanent refusal was not logged at ERROR:\n%s", logs)
				}
				return
			}
			if err == nil || errors.Is(err, ErrNeedsReview) {
				t.Fatalf("err = %v, want a retryable failure for status %d", err, c.status)
			}
		})
	}
}

// TestAPermanentRefusalLogsTheAPIsOwnMessage: the operator needs to read
// "API key not valid" in the log, not a bare status code.
func TestAPermanentRefusalLogsTheAPIsOwnMessage(t *testing.T) {
	path := writeTempReceipt(t, t.TempDir(), []byte("body"))
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"API key not valid.","status":"INVALID_ARGUMENT"}}`))
	})
	logs := captureLog(t)

	NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 10, Path: path})

	if !strings.Contains(logs.String(), "API key not valid.") {
		t.Errorf("log does not carry the API's message:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "YABA_GEMINI_KEY") {
		t.Errorf("log does not point at the configuration:\n%s", logs)
	}
}

func TestANetworkErrorIsRetryable(t *testing.T) {
	path := writeTempReceipt(t, t.TempDir(), []byte("body"))
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		// Hang up without answering.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})

	_, err := NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 11, Path: path})
	if err == nil || errors.Is(err, ErrNeedsReview) {
		t.Fatalf("err = %v, want a retryable failure", err)
	}
}

// TestRetryAfterReachesTheWorker: a 429's Retry-After survives the wrapping
// Process adds, so finishFailed can find it.
func TestRetryAfterReachesTheWorker(t *testing.T) {
	path := writeTempReceipt(t, t.TempDir(), []byte("body"))
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	})

	_, err := NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 12, Path: path})
	var ra retryAfterer
	if !errors.As(err, &ra) {
		t.Fatalf("err = %v does not carry a Retry-After", err)
	}
	if got := ra.RetryAfter(); got != 2*time.Minute {
		t.Errorf("RetryAfter = %v, want 2m", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"30":                            30 * time.Second,
		" 5 ":                           5 * time.Second,
		"0":                             0,
		"-4":                            0,
		"soon":                          0,
		"Sat, 14 Mar 2026 12:01:30 GMT": 90 * time.Second,
		"Sat, 14 Mar 2026 11:00:00 GMT": 0, // already past
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestAPermanentRefusalEndsOnTheManualEntryForm runs a 401 through the real
// queue: one attempt, no retries, and the user is sent to the form with their
// image rather than told the import failed.
func TestAPermanentRefusalEndsOnTheManualEntryForm(t *testing.T) {
	q := newQueueRig(t)
	path := writeTempReceipt(t, t.TempDir(), []byte("body"))
	id, err := q.store.EnqueueReceipt(context.Background(), q.scope, filepath.ToSlash(path), "receipt.jpg")
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	})
	captureLog(t)

	w := New(q.store, NewGeminiProcessor("k", ""), time.Minute)
	clock := newFakeClock(w)
	for i := 0; i < store.MaxJobAttempts+1; i++ {
		w.processNext(context.Background())
		clock.advance(maxRetryAfter)
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("Gemini was called %d times, want 1: a 401 must not be retried", n)
	}
	if state, _ := q.status(id); state != "done" {
		t.Errorf("status = %q, want done (the same end state as an illegible receipt)", state)
	}
	ns := q.notifications()
	if len(ns) != 1 || ns[0].Kind != "info" || !strings.Contains(ns[0].Link, "receipt=") {
		t.Errorf("notifications = %+v, want one info message linking to the receipt form", ns)
	}
	if _, err := q.store.UnattachedReceipt(context.Background(), q.scope, id); err != nil {
		t.Errorf("the receipt is not waiting to be entered by hand: %v", err)
	}
}

// TestAnOversizeReceiptIsNeverSent: base64 makes a file a third bigger, so a
// receipt inside the upload limit can still be over Gemini's request limit.
// That request would only earn a 400, so it is not made.
func TestAnOversizeReceiptIsNeverSent(t *testing.T) {
	dir := t.TempDir()
	// Just enough raw bytes that the encoded form is over the limit.
	raw := maxGeminiInlineData/4*3 + 3
	path := writeTempReceipt(t, dir, make([]byte, raw))

	var called atomic.Bool
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) { called.Store(true) })
	logs := captureLog(t)

	_, err := NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 13, Path: path})
	if !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}
	if called.Load() {
		t.Error("an oversize receipt was sent to Gemini anyway")
	}
	if !strings.Contains(logs.String(), "too large") {
		t.Errorf("nothing was logged about the size:\n%s", logs)
	}
}

// TestAReceiptJustUnderTheLimitIsSent guards the other side of the boundary.
func TestAReceiptJustUnderTheLimitIsSent(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a ~15MB request")
	}
	dir := t.TempDir()
	// writeTempReceipt adds a 4-byte header; keep the total a multiple of 3
	// so the encoded size is exact and under the limit.
	raw := maxGeminiInlineData/4*3 - 4 - 3
	path := writeTempReceipt(t, dir, make([]byte, raw))

	var called atomic.Bool
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Write(geminiReply(t, geminiExtract{Legible: true, Total: "1.00"}))
	})

	if _, err := NewGeminiProcessor("k", "").Process(t.Context(), store.ReceiptJob{ID: 14, Path: path}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !called.Load() {
		t.Error("a receipt inside the limit was not sent")
	}
}

func TestNormalizeGeminiModel(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"gemini-3.1-flash-lite", "gemini-3.1-flash-lite", true},
		{"models/gemini-2.5-pro", "gemini-2.5-pro", true},
		{"  gemini-2.5-flash\r\n", "gemini-2.5-flash", true},
		{"", DefaultGeminiModel, true},
		{"   ", DefaultGeminiModel, true},

		{"Google Gemini Flash 3.1", "", false},
		{"Gemini-2.5-Pro", "", false},
		{"gemini/../../v1/files", "", false},
		{"gemini?key=x", "", false},
		{"-gemini", "", false},
		{".gemini", "", false},
		{"models/", DefaultGeminiModel, true},
	}
	for _, c := range cases {
		got, err := NormalizeGeminiModel(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("NormalizeGeminiModel(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("NormalizeGeminiModel(%q) = %q, want an error", c.in, got)
		}
	}
}

func TestNewGeminiProcessorTrimsTheKey(t *testing.T) {
	if p := NewGeminiProcessor(" test-key\r\n", ""); p == nil || p.APIKey != "test-key" {
		t.Errorf("processor = %+v, want the key trimmed to test-key", p)
	}
	if p := NewGeminiProcessor(" \r\n", ""); p != nil {
		t.Error("a key of only whitespace should mean no key at all")
	}
}

// TestTheModelIsEscapedInTheRequestPath: even a model that skipped startup
// validation cannot change which endpoint is called.
func TestTheModelIsEscapedInTheRequestPath(t *testing.T) {
	path := writeTempReceipt(t, t.TempDir(), []byte("body"))

	var gotPath atomic.Value
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.EscapedPath())
		w.Write(geminiReply(t, geminiExtract{Legible: true, Total: "1.00"}))
	})

	p := NewGeminiProcessor("k", "")
	p.Model = "evil/../../files"
	if _, err := p.Process(t.Context(), store.ReceiptJob{ID: 15, Path: path}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if want := "/v1beta/models/evil%2F..%2F..%2Ffiles:generateContent"; gotPath.Load() != want {
		t.Errorf("request path = %v, want %q", gotPath.Load(), want)
	}
}

func TestThePromptUsesTheStoresSpelling(t *testing.T) {
	if !strings.Contains(geminiPrompt, `"Uncategorised"`) || strings.Contains(geminiPrompt, "Uncategorized") {
		t.Error(`the prompt should ask for "Uncategorised", the spelling the store and charts use`)
	}
}
