package worker

import (
	"context"
	"net/http"
	"testing"
)

// TestIllegibleTotalIsNotPrefilled: when Gemini says it could not read the
// receipt, any total it still returns is not stored to prefill the form, and
// its confidence is not shown as "very likely right".
func TestIllegibleTotalIsNotPrefilled(t *testing.T) {
	q := newQueueRig(t)
	path := writeTempReceipt(t, t.TempDir(), []byte("blurry"))
	stubGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(geminiReply(t, geminiExtract{Legible: false, Total: "45.70", Confidence: 0.9, Merchant: "Shell"}))
	})
	id, err := q.store.EnqueueReceipt(context.Background(), q.scope, path, "r.jpg")
	if err != nil {
		t.Fatal(err)
	}
	q.drain(NewGeminiProcessor("k", ""))
	job, err := q.store.UnattachedReceipt(context.Background(), q.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Draft == nil {
		t.Fatal("the receipt should still have a draft to finish by hand")
	}
	if job.Draft.HasTotal() {
		t.Errorf("an illegible total of %s was stored to prefill the form", job.Draft.Total.Display())
	}
	if job.Draft.Certainty() == "high" {
		t.Error("an illegible receipt is labelled very likely right")
	}
}
