package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/ocr"
	"github.com/jthomasw/YABA-2026/internal/store"
)

// allowedReceiptKinds are the formats a receipt may be uploaded in.
//
// The types are decided by ocr.Sniff rather than http.DetectContentType, which
// cannot recognise HEIC at all: it reports it as application/octet-stream, so
// the old allowlist silently rejected the format every recent iPhone
// photographs in by default -- which is to say, the single most likely thing
// somebody would try to upload a receipt as.
var allowedReceiptKinds = map[ocr.Kind]bool{
	ocr.KindJPEG: true,
	ocr.KindPNG:  true,
	ocr.KindWebP: true,
	ocr.KindGIF:  true,
	ocr.KindHEIC: true,
	ocr.KindAVIF: true,
	ocr.KindPDF:  true,
}

// defaultMaxUploadMB is the receipt size limit when Config leaves it unset. It
// matches the -max-upload-mb default in main.go: a modern phone photo is
// routinely 6-10 MB, and a lower fallback here once meant a server built
// without the flag refused the very receipts the flag's default was raised for.
const defaultMaxUploadMB = 15

// maxUploadMB is the receipt size limit in force, in megabytes. Every message
// that names the limit uses this, never cfg.MaxUploadMB directly, which is
// zero when unset and produced "larger than 0 MB".
func (s *Server) maxUploadMB() int64 {
	if s.cfg.MaxUploadMB <= 0 {
		return defaultMaxUploadMB
	}
	return s.cfg.MaxUploadMB
}

func (s *Server) maxUploadBytes() int64 {
	return s.maxUploadMB() << 20
}

// saveReceipt stores an uploaded receipt and returns its path and original name.
func (s *Server) saveReceipt(r *http.Request, userID int64) (path, original string, err error) {
	if r.MultipartForm == nil {
		// Not a multipart submission, so no file was attached.
		return "", "", nil
	}

	file, header, err := r.FormFile("receipt")
	if errors.Is(err, http.ErrMissingFile) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("could not read the attached file")
	}
	defer file.Close()

	if header.Size == 0 {
		return "", "", nil
	}
	if header.Size > s.maxUploadBytes() {
		return "", "", fmt.Errorf("that file is larger than %d MB", s.maxUploadMB())
	}

	// Sniff the first 512 bytes, which is what http.DetectContentType reads,
	// then rewind so the whole file still gets copied.
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", "", fmt.Errorf("could not read the attached file")
	}
	head = head[:n]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("could not read the attached file")
	}

	kind := ocr.Sniff(head)
	if !allowedReceiptKinds[kind] {
		return "", "", fmt.Errorf(
			"receipts must be a photo or a PDF — JPEG, PNG, HEIC, WebP, GIF or PDF")
	}
	ext := kind.Ext()

	dir := s.cfg.UploadDir
	if dir == "" {
		dir = "uploads"
	}
	// Per-user subdirectory keeps one user's files from being listed alongside
	// another's, and keeps the directory from growing to tens of thousands of
	// entries in one flat folder.
	dir = filepath.Join(dir, strconv.FormatInt(userID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}

	name, err := randomFilename(ext)
	if err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}
	full := filepath.Join(dir, name)

	// O_EXCL means a name collision fails rather than overwriting an existing receipt.
	dst, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("could not store the receipt")
	}
	// Cap the copy as well as checking header.Size: the declared size is a
	// hint, and LimitReader is what actually bounds what reaches the disk.
	if _, err := io.Copy(dst, io.LimitReader(file, s.maxUploadBytes())); err != nil {
		dst.Close()
		os.Remove(full)
		return "", "", fmt.Errorf("could not store the receipt")
	}

	// Close is checked, not deferred. A full disk or an exceeded quota
	// surfaces at close(2), not at write: deferring it meant the handler
	// queued the job and told the user their upload had worked, and only
	// the worker discovered minutes later that the file was truncated.
	if err := dst.Close(); err != nil {
		log.Printf("ERROR storing receipt %s: %v", full, err)
		os.Remove(full)
		return "", "", fmt.Errorf("could not store the receipt")
	}

	// Stored with forward slashes whatever the local separator is.
	return filepath.ToSlash(full), cleanOriginalName(header.Filename), nil
}

// removeReceiptFiles deletes stored receipts whose rows are gone. A receipt is
// somebody's personal financial record, so it must not outlive the entry it
// belonged to -- on disk, or in every backup archive taken after. A failure is
// logged rather than reported: the database change has already committed.
func (s *Server) removeReceiptFiles(paths []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(receiptFile(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("receipts: could not delete %s: %v", p, err)
		}
	}
}

// receiptFile turns a stored path back into one the local filesystem understands: a
// no-op on Linux, and the inverse of the ToSlash above on Windows.
func receiptFile(stored string) string {
	return filepath.FromSlash(stored)
}

// handleImportRedirect keeps the old /import path working.
func (s *Server) handleImportRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/expense", http.StatusMovedPermanently)
}

// handleReceiptUpload stores the image, queues it and returns immediately.
func (s *Server) handleReceiptUpload(w http.ResponseWriter, r *http.Request) {
	user := mustUser(r)

	path, name, err := s.saveReceipt(r, user.ID)
	if err != nil {
		s.redirectError(w, r, "/expense",
			s.safeMessage(r, err, "That receipt could not be stored. Try again."))
		return
	}
	if path == "" {
		s.redirectError(w, r, "/expense", "Choose a receipt image or PDF to upload.")
		return
	}

	job, err := s.store.EnqueueReceipt(r.Context(), scopeOf(r), path, name)
	if err != nil {
		s.removeReceiptFiles([]string{path})
		s.serverError(w, r, err)
		return
	}

	// Nudge the worker so it starts now rather than on its next tick.
	s.wakeWorker()

	// Back where they were, watching this receipt, rather than off to the
	// dashboard to wait for a notification. Uploading a receipt is the middle of
	// a task, not the end of one: the next thing the user wants is the filled-in
	// form, and the shortest path to it is to stay on this page until it is
	// ready. The redirect carries the id so the result survives a refresh and
	// works with scripting off, where the page simply says it is being read.
	http.Redirect(w, r, "/expense?receipt="+strconv.FormatInt(job, 10), http.StatusSeeOther)
}

// receiptStatus is the JSON the upload page polls while a receipt is read. It is
// deliberately small: a stage, a figure to draw, and somewhere to go once there
// is somewhere to go.
type receiptStatus struct {
	Status  string `json:"status"`
	Percent int    `json:"percent"`
	Label   string `json:"label"`
	Done    bool   `json:"done"`
	Failed  bool   `json:"failed"`
	Next    string `json:"next,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// handleReceiptStatus reports how far along one receipt is.
//
// The percentages are honest about being stages rather than progress: the
// Gemini call reports nothing while it works, so there is no true fraction to show. Each
// stage names a ceiling and the page eases towards it, which is why 'processing'
// stops short of 100 -- a ring that sits at 100 while the work continues is a
// lie the user catches immediately.
func (s *Server) handleReceiptStatus(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	job, err := s.store.ReceiptInFlight(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		// Either it never existed in this budget, or it has already become an
		// expense. Both are "nothing left to watch", and neither is worth
		// telling a guesser apart.
		s.writeJSON(w, r, receiptStatus{
			Status: "gone", Percent: 100, Label: "Already entered", Done: true,
		})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	out := receiptStatus{Status: string(job.Status)}
	switch job.Status {
	case store.JobQueued:
		out.Percent, out.Label = 20, "Queued"
	case store.JobProcessing:
		out.Percent, out.Label = 70, "Reading the receipt"
	case store.JobFailed:
		out.Percent, out.Label, out.Failed = 100, "Could not be read", true
		// NOT job.Error. That string is the worker's internal cause --
		// "stat uploads/42/9f3c....jpg: no such file or directory", or a raw
		// API error body -- and import.js writes whatever arrives straight
		// into the page, so the server's filesystem layout was printed in the
		// browser of anyone whose receipt failed. The real cause is already in
		// the log, recorded against this job id.
		out.Detail = "Enter the details by hand instead. The image is saved."
		out.Next = "/transactions/new?type=expense&receipt=" + strconv.FormatInt(job.ID, 10)
	case store.JobDone:
		out.Percent, out.Label, out.Done = 100, "Ready", true
		out.Next = "/transactions/new?type=expense&receipt=" + strconv.FormatInt(job.ID, 10)
		switch d := job.Draft; {
		case job.ReaderUnavailable():
			// Not the picture's fault: nothing read it. See ErrReaderUnavailable.
			out.Label = "Saved"
			out.Detail = "Automatic reading isn't available on this server — enter the details yourself"
		case d != nil && d.Total > 0:
			out.Detail = d.Total.Display()
			if d.Merchant != "" {
				out.Detail += " · " + d.Merchant
			}
			if n := d.ReadItemCount(); n > 0 {
				out.Detail += fmt.Sprintf(" · %d item%s", n, map[bool]string{true: "", false: "s"}[n == 1])
			}
		default:
			out.Detail = "No amount could be read — you will be asked for it"
		}
	}
	s.writeJSON(w, r, out)
}

// handleReceiptsPage lists every receipt in this budget that nobody has turned
// into an expense yet.
//
// It used to sit at the top of Add Expense, where it was the first thing anybody
// saw on a page whose job is to offer two choices. Here it is somewhere to go
// rather than something to get past -- and it still exists, because without it a
// dismissed notification would strand a receipt in the queue with no way to
// reach it.
func (s *Server) handleReceiptsPage(w http.ResponseWriter, r *http.Request) {
	v := receiptsView{view: s.baseView(w, r, "Receipts", "receipts")}

	var err error
	if v.Waiting, err = s.store.UnattachedReceipts(r.Context(), scopeOf(r), 50); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "receipts.html", v)
}

// receiptsView backs the receipts page.
type receiptsView struct {
	view
	Waiting []store.ReceiptJob
}

// handleReceipt serves a stored receipt to the user who owns it.
func (s *Server) handleReceiptDiscard(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	path, err := s.store.DiscardReceipt(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		s.redirectError(w, r, "/expense", "That receipt has already been used or is no longer there.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The row is gone, so the file is unreachable either way.
	s.removeReceiptFiles([]string{path})

	s.redirectSuccess(w, r, "/expense", "Receipt discarded.")
}

// handleReceiptPreview serves the image of a receipt that has not yet become an
// expense, so the confirmation form can show the photograph beside the figures
// read off it. Checking a number against the picture is the whole point of
// confirming, and it cannot be done from memory.
//
// This is the one place in the application that serves a stored file inline
// rather than as an attachment, so it is worth being explicit about why that is
// safe here. handleReceipt below sends Content-Disposition: attachment because
// an HTML or SVG file rendered in this origin could run script with access to
// the session cookie. Here the bytes are sniffed first and only ever labelled
// with a raster image type this server recognised itself -- never a type taken
// from the upload -- and X-Content-Type-Options: nosniff stops the browser
// second-guessing that label. A JPEG cannot execute anything, and anything that
// is not one of the four raster formats is refused rather than served.
func (s *Server) handleReceiptPreview(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// Scoped to the household, so a guessed id returns 404 rather than somebody
	// else's shopping.
	job, err := s.store.UnattachedReceipt(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	f, err := os.Open(receiptFile(job.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	head := make([]byte, 32)
	n, _ := f.Read(head)
	kind := ocr.Sniff(head[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Only formats a browser will draw in an <img>, and only ones whose type
	// this server determined for itself.
	switch kind {
	case ocr.KindJPEG, ocr.KindPNG, ocr.KindGIF, ocr.KindWebP:
	default:
		// A PDF or a HEIC is a perfectly good receipt but not a previewable one,
		// and guessing would either fail silently or serve a type that is not
		// what the bytes are.
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", string(kind))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A receipt is one person's shopping. Private stops a shared proxy caching
	// it, and the URL is only reachable by a member of the household anyway.
	w.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(w, r, filepath.Base(job.Path), info.ModTime(), f)
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)

	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	// Ownership is checked by looking the transaction up scoped to this user,
	// so a guessed transaction id returns 404 rather than someone else's file.
	t, err := s.store.ByID(r.Context(), sc, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.ReceiptPath == "") {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	f, err := os.Open(receiptFile(t.ReceiptPath))
	if err != nil {
		// The row survives even if the file is gone, so this is a 404 rather than a 500.
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Content-Disposition attachment plus nosniff means a stored file is downloaded rather
	// than rendered in this origin, where an SVG or HTML file would run script with access
	// to the site's cookies.
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+safeDownloadName(t.ReceiptName, t.ReceiptPath)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(t.ReceiptPath), info.ModTime(), f)
}

func randomFilename(ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b) + ext, nil
}

// receiptLabel turns an uploaded filename into a first guess at a description.
func receiptLabel(original string) string {
	name := cleanOriginalName(original)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.NewReplacer("_", " ", "-", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")

	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}

	// Camera and screenshot defaults carry no information.
	lower := strings.ToLower(name)
	for _, prefix := range []string{"img", "image", "photo", "screenshot", "scan", "dsc", "pxl"} {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}
	// A name that is only digits and punctuation is a timestamp, not a label.
	if strings.IndexFunc(name, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}) < 0 {
		return ""
	}
	return name
}

func cleanOriginalName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '/' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "receipt"
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// safeDownloadName picks the filename offered to the browser.
func safeDownloadName(original, storedPath string) string {
	if n := cleanOriginalName(original); n != "receipt" {
		return n
	}
	return filepath.Base(storedPath)
}
