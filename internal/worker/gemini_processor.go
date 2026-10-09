package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/money"
	"github.com/jthomasw/YABA-2026/internal/ocr"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// DefaultGeminiModel is used when YABA_GEMINI_MODEL is not set. It is Google's
// cheapest current multimodal model -- picked deliberately, because reading a
// receipt is exactly the high-volume, low-stakes-per-call job a "lite" tier is
// for, and nothing here is ever trusted without a person confirming it anyway
// (see the Draft type in worker.go).
const DefaultGeminiModel = "gemini-3.1-flash-lite"

// fallbackCategory is the category a receipt gets when the model offers none.
// It is spelled the way the store and the charts already spell it (they group
// blank labels under "Uncategorised"), so a reading that falls back lands in
// the same slice as everything else nobody categorised rather than starting a
// second one that differs by one letter. It is the store's own constant, so
// the two cannot drift apart again.
const fallbackCategory = store.Uncategorised

// geminiModelPattern is the shape of a Gemini model ID: lower-case letters,
// digits, dots and hyphens, like "gemini-3.1-flash-lite". It exists because a
// production deployment set YABA_GEMINI_MODEL to the display name "Google
// Gemini Flash 3.1", which went into the request path and failed every receipt
// with a 404 that only showed up in the logs as a run of retries.
var geminiModelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]*$`)

// NormalizeGeminiModel checks a configured model name and returns the bare ID
// to put in the request URL. Surrounding whitespace is dropped (an env file
// with Windows line endings leaves a \r on every value), the "models/" prefix
// Google's own docs and API listings print is accepted and stripped, and empty
// means DefaultGeminiModel. Anything else that is not a model ID is an error,
// so a typo stops the server at startup instead of silently failing uploads.
func NormalizeGeminiModel(raw string) (string, error) {
	model := strings.TrimPrefix(strings.TrimSpace(raw), "models/")
	if model == "" {
		return DefaultGeminiModel, nil
	}
	if !geminiModelPattern.MatchString(model) {
		return "", fmt.Errorf("%q is not a Gemini model ID: expected lower-case letters, "+
			"digits, dots and hyphens, like %q", raw, DefaultGeminiModel)
	}
	return model, nil
}

// maxGeminiInlineData is the largest base64-encoded image this server will
// send. Gemini caps a whole generateContent request carrying inline_data at
// about 20MB, and base64 makes a file a third bigger, so the 15MB the upload
// form allows can come out well over the limit. The 100KB held back covers
// the prompt, the response schema and the JSON around them (a few KB today)
// with room to grow. A request over the limit is refused with a 400 that no
// retry will ever fix, so it is not sent at all.
const maxGeminiInlineData = 20_000_000 - 100_000

// geminiBaseURL is a var, not a const, purely so a test can point it at an
// httptest.Server instead of the real API.
var geminiBaseURL = "https://generativelanguage.googleapis.com"

// geminiSupportedKinds are the formats sent to Gemini as-is. GIF is left out
// even though the upload form accepts it -- a receipt is never legitimately an
// animation, and it is simplest to let that one case fall through to "needs
// review" rather than assume the API will accept it.
var geminiSupportedKinds = map[ocr.Kind]bool{
	ocr.KindJPEG: true,
	ocr.KindPNG:  true,
	ocr.KindWebP: true,
	ocr.KindHEIC: true,
	ocr.KindPDF:  true,
}

// GeminiProcessor reads a receipt by sending the image itself to Google's
// Gemini API and asking for the amount, date, merchant and line items back as
// JSON. It replaces the local tesseract pipeline this package used to run:
// nothing to install on the server, no preprocessing to tune by hand, and it
// reads far more receipts correctly.
//
// The trade a deployment is making by setting YABA_GEMINI_KEY is that every
// receipt photo now leaves this server and is sent to Google. about.html says
// so; that wording has to change together with this file, not after it.
//
// This is built against Gemini's generateContent endpoint rather than the
// newer "Interactions" API Google introduced in 2026, because generateContent
// is still supported and its request/response shape has been stable for
// years. If Google retires it, this is the one place that needs to change --
// nothing else in the worker package knows or cares how the reading was done.
type GeminiProcessor struct {
	APIKey string
	Model  string
	Client *http.Client
}

// NewGeminiProcessor builds a processor around the given key, or returns nil
// when no key is configured -- so a deployment without YABA_GEMINI_KEY set
// degrades to queueing receipts for manual entry instead of failing uploads.
func NewGeminiProcessor(apiKey, model string) *GeminiProcessor {
	// A key pasted with a stray space, or read from an env file with CRLF line
	// endings, is otherwise sent as-is and rejected as invalid -- or, with a
	// \r in it, refused by net/http before it leaves the machine.
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	if model == "" {
		model = DefaultGeminiModel
	}
	return &GeminiProcessor{
		APIKey: apiKey,
		Model:  model,
		// A receipt reading should never hang the queue: this is generous
		// enough for a slow connection to Google but short enough that one
		// stuck request cannot back up every upload behind it.
		Client: &http.Client{Timeout: 45 * time.Second},
	}
}

// Describe names what goes in the startup log, without ever printing the key
// itself.
func (p *GeminiProcessor) Describe() string {
	return fmt.Sprintf("reading receipts with the Gemini API (%s)", p.Model)
}

// geminiExtract is the shape asked of the model. Every amount is a plain
// decimal string ("12.34", no currency symbol) rather than a JSON number, so it
// goes through money.Parse exactly like a user's own typed figure would --
// a malformed string becomes a blank field, never a silently wrong one built
// from a float.
type geminiExtract struct {
	Legible    bool            `json:"legible"`
	Merchant   string          `json:"merchant"`
	Category   string          `json:"category"`
	Date       string          `json:"date"`
	Subtotal   string          `json:"subtotal"`
	Tax        string          `json:"tax"`
	TaxLines   []geminiLineOut `json:"tax_lines"`
	Tip        string          `json:"tip"`
	Total      string          `json:"total"`
	Confidence float64         `json:"confidence"`
	Items      []geminiLineOut `json:"items"`
}

type geminiLineOut struct {
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

// geminiResponseSchema constrains the model's reply to exactly this shape, so
// parsing it is not guesswork over free-form prose.
var geminiResponseSchema = map[string]any{
	"type": "OBJECT",
	"properties": map[string]any{
		"legible": map[string]any{
			"type":        "BOOLEAN",
			"description": "false if the receipt is too blurry, dark, cropped or otherwise unreadable to give a total with any confidence",
		},
		"merchant": map[string]any{"type": "STRING"},
		"category": map[string]any{
			"type":        "STRING",
			"description": "a short one or two word expense category guessed from the merchant and items, e.g. Groceries, Dining, Gas, Pharmacy, Hardware",
		},
		"date": map[string]any{
			"type":        "STRING",
			"description": "purchase date as YYYY-MM-DD; empty if no year is visible anywhere on the receipt",
		},
		"subtotal": map[string]any{"type": "STRING", "description": "plain decimal amount, no currency symbol, e.g. 12.34; empty if not printed"},
		"tax":      map[string]any{"type": "STRING", "description": "total tax as a plain decimal amount; empty if not printed"},
		"tax_lines": map[string]any{
			"type":        "ARRAY",
			"description": "every tax line printed on the receipt, in order, e.g. TAX 1 5.5% and TAX 4 8%; include 0.00 lines",
			"items": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"description": map[string]any{"type": "STRING", "description": "the tax line's label as printed, e.g. TAX 1 5.5%"},
					"amount":      map[string]any{"type": "STRING", "description": "plain decimal amount of this tax line"},
				},
			},
		},
		"tip":   map[string]any{"type": "STRING", "description": "plain decimal amount; empty if not printed or not applicable"},
		"total": map[string]any{"type": "STRING", "description": "the final amount actually charged, as a plain decimal amount; empty if it cannot be read with confidence"},
		"confidence": map[string]any{
			"type":        "NUMBER",
			"description": "0 to 1: how sure you are that total specifically is correct",
		},
		"items": map[string]any{
			"type": "ARRAY",
			"items": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"description": map[string]any{"type": "STRING"},
					"amount":      map[string]any{"type": "STRING", "description": "plain decimal amount for this one line"},
				},
			},
		},
	},
	// category and items are required so the model always attempts them
	// rather than omitting the field entirely -- an empty string or an empty
	// array is still a valid answer, but a field the model never had to
	// produce is one it will skip under any time pressure.
	"required": []string{"legible", "category", "items", "tax_lines"},
}

// geminiPrompt is deliberately explicit about never guessing: a confident
// wrong total is the one failure mode this whole feature exists to avoid (see
// the Draft type's doc comment in worker.go).
const geminiPrompt = `You are reading a single photographed or scanned retail or restaurant receipt for a personal budgeting app. Extract the merchant, purchase date, subtotal, tax, tip, final total, a short expense category, and every line item, as JSON matching the response schema exactly.

Rules:
- Never guess a MONEY figure. If the total cannot be read with confidence, set legible to false and leave total (and any other illegible amount) empty.
- category is different: always give your best one or two word guess (Groceries, Dining, Gas, Pharmacy, Hardware, Coffee, Household, Clothing, and similar) from the merchant name and whatever items you can make out. Category is never left blank -- if you are genuinely unsure, answer "` + fallbackCategory + `" rather than omitting it. Being wrong and correctable beats leaving the form blocked on a required field.
- items: list EVERY purchased product line, top to bottom, one entry per printed line -- a long grocery receipt can have dozens, and none may be skipped or merged. Use the product name as printed for description (leave out barcodes/UPC numbers and the trailing tax flag letters such as F, N, X). Do NOT put subtotal, tax, total, payment, change or balance lines in items. A discount or coupon line is an item with a negative amount. If a line's own price is illegible, still include the line with its amount left empty rather than dropping it. Only send an empty items array when the receipt truly shows one undifferentiated charge with no line breakdown at all.
- tax_lines: every tax line printed, in order, with its label as printed and its amount (include 0.00 lines). tax is the sum of all of them. If only one tax amount is printed, tax_lines has that one line. If none is printed, send an empty array.
- Every amount is a plain decimal string with no currency symbol or thousands separator, e.g. "12.34", not "$12.34" or "12,34".
- date is YYYY-MM-DD. If no year appears anywhere on the receipt, leave date empty rather than assuming one.
- confidence is your own 0 to 1 estimate that the total specifically is correct, not an average over the whole receipt.`

// Process sends the stored image to Gemini and turns its answer into a draft.
//
// The same three outcomes OCRProcessor distinguished still apply: a missing or
// empty file is a real failure worth retrying; a format Gemini was not asked
// to read, a refusal, or an illegible receipt asks for review without ever
// being retried into a failure notification; and a read with an amount in it
// is success.
func (p *GeminiProcessor) Process(ctx context.Context, job store.ReceiptJob) (Draft, error) {
	path := localPath(job.Path)

	data, err := os.ReadFile(path)
	if err != nil {
		return Draft{}, fmt.Errorf("stored receipt is unreadable: %w", err)
	}
	if len(data) == 0 {
		return Draft{}, errors.New("stored receipt is empty")
	}

	kind := ocr.Sniff(data)
	if !geminiSupportedKinds[kind] {
		log.Printf("worker: receipt job %d is in a format this server does not send to Gemini (%s)",
			job.ID, kind.Describe())
		return Draft{}, ErrNeedsReview
	}

	if n := base64.StdEncoding.EncodedLen(len(data)); n > maxGeminiInlineData {
		log.Printf("worker: receipt job %d is too large to send to Gemini (%d bytes, %d once encoded; the limit is %d)",
			job.ID, len(data), n, maxGeminiInlineData)
		return Draft{}, ErrNeedsReview
	}

	extract, err := p.extract(ctx, data, string(kind))
	var apiErr *geminiAPIError
	if errors.As(err, &apiErr) && apiErr.Permanent() {
		// Google refused the request itself: a bad or revoked key, a model that
		// does not exist, a request it will never accept. Retrying cannot help,
		// and ending in "could not import" would throw away an upload that is
		// perfectly fine -- so the user gets the manual-entry form with their
		// image, and the operator gets an ERROR naming the likely cause.
		log.Printf("worker: ERROR: receipt job %d: Gemini rejected the request (%v). "+
			"This is usually configuration -- check YABA_GEMINI_KEY and YABA_GEMINI_MODEL (%q). "+
			"The receipt is kept for manual entry.", job.ID, apiErr, p.Model)
		return Draft{}, ErrReaderUnavailable
	}
	if err != nil {
		// A genuine, possibly transient failure -- a network error, a timeout, a
		// 429 or a 5xx from Google -- worth retrying and eventually reporting,
		// exactly like a stat() failure on the file itself.
		return Draft{}, fmt.Errorf("gemini: %w", err)
	}

	d := toGeminiDraft(extract)

	if !extract.Legible || d.Amount <= 0 {
		log.Printf("worker: receipt job %d: Gemini could not read a total", job.ID)
		return d, ErrNeedsReview
	}

	log.Printf("worker: receipt job %d read %s (confidence %.2f, %d item(s)) via Gemini",
		job.ID, d.Amount.Display(), d.Confidence, len(d.Items))
	return d, nil
}

// extract makes the one API call and returns what the model reported.
func (p *GeminiProcessor) extract(ctx context.Context, image []byte, mimeType string) (geminiExtract, error) {
	reqBody := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]any{
					{
						"inline_data": map[string]any{
							"mime_type": mimeType,
							"data":      base64.StdEncoding.EncodeToString(image),
						},
					},
					{"text": geminiPrompt},
				},
			},
		},
		// No temperature: Google's guidance for Gemini 3 models is to leave it
		// at the default 1.0, warning that lower values "may lead to unexpected
		// behavior, such as looping or degraded performance". The schema and
		// the prompt are what keep the answer to the facts on the receipt.
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   geminiResponseSchema,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return geminiExtract{}, fmt.Errorf("build request: %w", err)
	}

	// PathEscape is belt and braces: the model is validated at startup, but it
	// is still a configured string going into a URL path, and a stray "/" or
	// "?" in it must not be able to change which endpoint is called.
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", geminiBaseURL, url.PathEscape(p.Model))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return geminiExtract{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The key goes in a header, not a query parameter, so it never ends up in a
	// proxy access log or anything that records the request URL.
	httpReq.Header.Set("x-goog-api-key", p.APIKey)

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return geminiExtract{}, fmt.Errorf("call: %w", err)
	}
	defer closeBody(resp.Body)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return geminiExtract{}, fmt.Errorf("read response: %w", err)
	}

	// The status decides what kind of failure this is, so it is checked before
	// the body is trusted to be JSON: a proxy or load balancer in front of the
	// API answers with an HTML page, and that must still come out as a 502
	// worth retrying rather than a decode error.
	if resp.StatusCode != http.StatusOK {
		return geminiExtract{}, newGeminiAPIError(resp, raw)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return geminiExtract{}, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}

	if errObj, ok := parsed["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		if msg == "" {
			msg = "error object in a 200 response"
		}
		return geminiExtract{}, errors.New(msg)
	}

	text, blockReason, err := geminiText(parsed)
	if err != nil {
		return geminiExtract{}, err
	}
	if blockReason != "" {
		// Google's own safety filter refused the image outright. That will
		// happen again on a retry, so this reads as an illegible receipt
		// instead of a failure worth requeuing.
		return geminiExtract{Legible: false}, nil
	}

	var extract geminiExtract
	if err := json.Unmarshal([]byte(text), &extract); err != nil {
		return geminiExtract{}, fmt.Errorf("decode extract: %w", err)
	}
	return extract, nil
}

// closeBody drains what is left of a response (bounded, so a huge error page
// cannot stall the worker) before closing it, which lets the Transport reuse
// the connection for the next receipt instead of opening a new TLS session.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

// geminiAPIError is a non-200 answer from the API. Whether it is worth
// retrying is decided by its status alone, so Process can tell a
// misconfiguration from a bad minute at Google.
type geminiAPIError struct {
	Status  int
	Message string

	// retryAfter is what the response's Retry-After header asked for, or zero.
	retryAfter time.Duration
}

func (e *geminiAPIError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Status, e.Message)
}

// Permanent reports whether the same request can never succeed. Every 4xx is
// the request's own fault -- a bad key (400/401/403), a model that does not
// exist (404), a body it will not take -- except 408 (the request timed out)
// and 429 (slow down), which are about timing and are worth another try, like
// every 5xx.
func (e *geminiAPIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 &&
		e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}

// RetryAfter lets the worker wait as long as the API asked, rather than
// coming back on its own schedule only to be refused again.
func (e *geminiAPIError) RetryAfter() time.Duration { return e.retryAfter }

// newGeminiAPIError builds the error for a non-200 response, preferring the
// API's own message (which says things like "API key not valid") over a bare
// status code, and falling back to the start of whatever else came back.
func newGeminiAPIError(resp *http.Response, raw []byte) *geminiAPIError {
	e := &geminiAPIError{
		Status:     resp.StatusCode,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
		e.Message = body.Error.Message
		if body.Error.Status != "" {
			e.Message = body.Error.Status + ": " + e.Message
		}
		return e
	}

	snippet := strings.Join(strings.Fields(string(raw)), " ")
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	if snippet == "" {
		snippet = http.StatusText(resp.StatusCode)
	}
	e.Message = snippet
	return e
}

// parseRetryAfter reads a Retry-After header, which is either a number of
// seconds or an HTTP date. Anything unreadable, or already in the past, is
// treated as no request at all.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// geminiText pulls the model's text reply out of a generateContent response,
// reading both the camelCase field names Google's docs have used for years and
// the snake_case ones some of its API surfaces also accept -- cheap insurance
// against a naming detail changing under this integration.
func geminiText(parsed map[string]any) (text, blockReason string, err error) {
	if pf, ok := anyOf(parsed, "promptFeedback", "prompt_feedback").(map[string]any); ok {
		if br, _ := anyOf(pf, "blockReason", "block_reason").(string); br != "" {
			return "", br, nil
		}
	}

	candidates, _ := anyOf(parsed, "candidates").([]any)
	if len(candidates) == 0 {
		return "", "", errors.New("no candidates in response")
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := anyOf(candidate, "content").(map[string]any)
	parts, _ := anyOf(content, "parts").([]any)
	for _, partAny := range parts {
		part, _ := partAny.(map[string]any)
		// A thinking model can return its reasoning as a part marked
		// "thought"; that is not the JSON answer.
		if thought, _ := part["thought"].(bool); thought {
			continue
		}
		if t, ok := anyOf(part, "text").(string); ok && t != "" {
			return t, "", nil
		}
	}
	return "", "", errors.New("no text in response")
}

// anyOf returns the first key present in m, trying each name in turn.
func anyOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// toGeminiDraft converts what the model returned into the worker's contract,
// parsing every amount through money.Parse exactly as a user's own typed
// figure would be.
func toGeminiDraft(e geminiExtract) Draft {
	items := make([]store.DraftItem, 0, len(e.Items))
	for _, it := range e.Items {
		items = append(items, store.DraftItem{
			Description: it.Description,
			Amount:      parseGeminiAmount(it.Amount),
		})
	}

	confidence := e.Confidence
	switch {
	case confidence < 0:
		confidence = 0
	case confidence > 1:
		confidence = 1
	}

	// The category field is required on the expense form -- leaving it blank
	// would hand back a form the user cannot submit without typing something
	// first, which defeats the point of reading the receipt at all. A wrong
	// guess is one click to fix; a blocked save is not a convenience.
	category := strings.TrimSpace(e.Category)
	if category == "" {
		category = fallbackCategory
	}

	var taxLines []store.DraftItem
	var taxSum store.Cents
	for _, t := range e.TaxLines {
		amount := parseGeminiAmount(t.Amount)
		taxLines = append(taxLines, store.DraftItem{Description: strings.TrimSpace(t.Description), Amount: amount})
		taxSum += amount
	}
	tax := parseGeminiAmount(e.Tax)
	if tax == 0 {
		tax = taxSum
	}

	return Draft{
		Label:      category,
		Payee:      e.Merchant,
		Amount:     parseGeminiAmount(e.Total),
		Date:       e.Date,
		Category:   category,
		Subtotal:   parseGeminiAmount(e.Subtotal),
		Tax:        tax,
		TaxLines:   taxLines,
		Tip:        parseGeminiAmount(e.Tip),
		Items:      items,
		Confidence: confidence,
	}
}

// parseGeminiAmount reads one of the model's amount strings, treating anything
// it cannot parse as simply absent rather than guessing at it.
func parseGeminiAmount(s string) store.Cents {
	if s == "" {
		return 0
	}
	c, err := money.Parse(s)
	if err != nil {
		return 0
	}
	return c
}
