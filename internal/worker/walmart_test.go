package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/store"
)

// walmartReceipt is what Gemini returns for the Walmart receipt in the bug
// report: sixteen items adding up to the $70.45 subtotal, three printed tax
// lines (one of them 0%), and a $73.53 total.
func walmartReceipt() geminiExtract {
	return geminiExtract{
		Legible: true, Merchant: "Walmart", Category: "Groceries", Date: "2023-10-31",
		Subtotal: "70.45", Tax: "3.08", Total: "73.53", Confidence: 0.97,
		Items: []geminiLineOut{
			{"STRAWB DONU", "2.98"}, {"SBUX SLCR 1", "8.97"}, {"DOWNY", "7.97"},
			{"GRANULATED", "3.24"}, {"GV FLOUR", "1.32"}, {"MM 6.3LB", "9.84"},
			{"LAC GEL LLA", "1.98"}, {"WM SEA SALT", "2.23"}, {"SLVR GLITTE", "1.24"},
			{"PNK PASTEL", "1.24"}, {"GRN MONSTER", "2.44"}, {"ELF SHADOW", "3.00"},
			{"RED CREAM M", "0.57"}, {"CRAYON", "0.99"}, {"LIP STICK", "1.00"},
			{"CLOWN", "21.44"},
		},
		TaxLines: []geminiLineOut{
			{"TAX 1 5.5 %", "2.84"}, {"TAX 12 0 %", "0.00"}, {"TAX 4 8 %", "0.24"},
		},
	}
}

// TestTheWalmartReceiptIsReadInFull: every item, each tax line that charged
// something, and a breakdown that adds up to the total -- so the form saves as
// it stands.
func TestTheWalmartReceiptIsReadInFull(t *testing.T) {
	var request map[string]any
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &request)
		// A thinking model may put its reasoning first, marked as a thought.
		reply := geminiReply(t, walmartReceipt())
		var parsed map[string]any
		json.Unmarshal(reply, &parsed)
		content := parsed["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)
		content["parts"] = append([]any{map[string]any{"text": "Reading the receipt...", "thought": true}},
			content["parts"].([]any)...)
		out, _ := json.Marshal(parsed)
		w.Write(out)
	})

	q := newQueueRig(t)
	dir := t.TempDir()
	path := writeTempReceipt(t, dir, []byte("walmart"))
	id, err := q.store.EnqueueReceipt(context.Background(), q.scope, filepath.ToSlash(path), "walmart.jpg")
	if err != nil {
		t.Fatal(err)
	}
	q.drain(NewGeminiProcessor("test-key", ""))

	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil || job.Draft == nil {
		t.Fatalf("no reading was stored: %v", err)
	}
	d := job.Draft
	if d.Total != 7353 || d.Merchant != "Walmart" || d.Date != "2023-10-31" {
		t.Errorf("draft = total %d, merchant %q, date %q", d.Total, d.Merchant, d.Date)
	}
	if n := d.ReadItemCount(); n != 16 {
		t.Errorf("%d items read, want all 16", n)
	}
	var added []string
	for _, it := range d.Items {
		if it.Added {
			added = append(added, it.Description)
		}
	}
	if strings.Join(added, "|") != "TAX 1 5.5 %|TAX 4 8 %" {
		t.Errorf("added lines = %q, want the two tax lines that charged something", added)
	}
	if d.ItemsTotal() != d.Total {
		t.Errorf("items add up to %d, not the total %d", d.ItemsTotal(), d.Total)
	}

	// The request itself: no temperature (Gemini 3 guidance), and the schema
	// asks for the tax lines.
	cfg, _ := request["generationConfig"].(map[string]any)
	if _, set := cfg["temperature"]; set {
		t.Error("the request sets a temperature; Gemini 3 models should use the default")
	}
	schema, _ := json.Marshal(cfg["responseSchema"])
	if !strings.Contains(string(schema), "tax_lines") {
		t.Error("the response schema does not ask for tax lines")
	}

	if ns := q.notifications(); len(ns) != 1 || !strings.Contains(ns[0].Text, "$73.53") {
		t.Errorf("notification = %+v", ns)
	}
	_ = os.Remove(path)
}

// TestNoKeyMeansReadingIsUnavailableNotUnreadable: a server with no Gemini key
// must not tell the user their clear photo "could not be read".
func TestNoKeyMeansReadingIsUnavailableNotUnreadable(t *testing.T) {
	q := newQueueRig(t)
	path := writeTempReceipt(t, t.TempDir(), []byte("clear photo"))
	id, err := q.store.EnqueueReceipt(context.Background(), q.scope, filepath.ToSlash(path), "clear.jpg")
	if err != nil {
		t.Fatal(err)
	}
	q.drain(nil) // what main passes when YABA_GEMINI_KEY is unset

	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if !job.ReaderUnavailable() {
		t.Errorf("job = status %q error %q; want it marked reader-unavailable", job.Status, job.Error)
	}
	ns := q.notifications()
	if len(ns) != 1 || strings.Contains(ns[0].Text, "could not be read") ||
		!strings.Contains(ns[0].Text, "isn't available") {
		t.Errorf("notification = %+v", ns)
	}
}

// TestARejectedKeyAlsoMeansUnavailable: Google refusing the server's own key
// is a configuration problem, not an unreadable receipt.
func TestARejectedKeyAlsoMeansUnavailable(t *testing.T) {
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"API key not valid","status":"INVALID_ARGUMENT"}}`))
	})
	q := newQueueRig(t)
	path := writeTempReceipt(t, t.TempDir(), []byte("x"))
	id, _ := q.store.EnqueueReceipt(context.Background(), q.scope, filepath.ToSlash(path), "x.jpg")
	q.drain(NewGeminiProcessor("bad-key", ""))
	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil || !job.ReaderUnavailable() {
		t.Errorf("job = %+v, %v; want reader-unavailable", job, err)
	}
	_ = store.UnreadReaderUnavailable
}
