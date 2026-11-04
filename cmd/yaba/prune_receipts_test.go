package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/db"
)

// reset -keep must keep the photo of a receipt that was uploaded to the kept
// budget by another member and not entered yet: its row survives the prune,
// so its file has to as well. Tried with the path stored absolute and, as the
// server stores it, relative to its own working directory.
func TestResetKeepKeepsPendingReceiptFiles(t *testing.T) {
	for _, relative := range []bool{false, true} {
		s := newSite(t)
		sqlDB, err := db.Open(s.db)
		if err != nil {
			t.Fatal(err)
		}
		seedUser(t, sqlDB, 2, "b@example.com")
		if _, err := sqlDB.Exec(`INSERT INTO household_members(household_id, user_id, role) VALUES(1, 2, 'editor')`); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(s.uploads, "2", "pending.jpg")
		stored := filepath.ToSlash(file)
		if relative {
			stored = "uploads/2/pending.jpg"
		}
		if _, err := sqlDB.Exec(`INSERT INTO receipt_jobs(user_id, household_id, path, original_name, status)
			VALUES(2, 1, ?, 'pending.jpg', 'done')`, stored); err != nil {
			t.Fatal(err)
		}
		sqlDB.Close()
		os.MkdirAll(filepath.Join(s.uploads, "2"), 0o700)
		os.WriteFile(file, []byte("jpeg"), 0o600)

		mustSucceed(t, yaba(t, s.env(), "prune\n", "reset", "-keep", "a@example.com"))
		if n := s.count(t, "receipt_jobs"); n != 1 {
			t.Fatalf("relative=%v: %d receipt jobs left, want 1", relative, n)
		}
		if _, err := os.Stat(file); err != nil {
			t.Errorf("relative=%v: the waiting receipt's photo was deleted: %v", relative, err)
		}
	}
}
