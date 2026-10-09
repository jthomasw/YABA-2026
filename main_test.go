// Tests for the startup checks main makes before anything is served.
package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/worker"
)

// TestStrayArgumentsAreRefused: "./yaba backup" against the server binary used
// to start a second server on a relative database instead of failing.
func TestStrayArgumentsAreRefused(t *testing.T) {
	if err := checkNoArgs(nil); err != nil {
		t.Errorf("no arguments: %v", err)
	}
	err := checkNoArgs([]string{"backup", "--keep", "3"})
	if err == nil {
		t.Fatal("a stray argument was accepted")
	}
	for _, want := range []string{`"backup"`, "web server", "./cmd/yaba"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %s", err, want)
		}
	}
}

func TestGeminiSettings(t *testing.T) {
	cases := []struct {
		name               string
		key, model         string
		wantKey, wantModel string
		wantErr            string // substring; empty means no error
	}{
		{"clean", "abc123", "gemini-3.1-flash-lite", "abc123", "gemini-3.1-flash-lite", ""},
		{"leading space on the key", " abc123", "gemini-2.5-pro", "abc123", "gemini-2.5-pro", ""},
		{"CRLF env file", "abc123\r", "gemini-2.5-pro\r", "abc123", "gemini-2.5-pro", ""},
		{"models/ prefix", "abc", "models/gemini-2.5-flash", "abc", "gemini-2.5-flash", ""},
		{"no key is fine", "", "gemini-2.5-flash", "", "gemini-2.5-flash", ""},
		{"empty model is the default", "abc", "", "abc", worker.DefaultGeminiModel, ""},

		{"display name", "abc", "Google Gemini Flash 3.1", "", "", "YABA_GEMINI_MODEL"},
		{"space inside the key", "abc 123", "gemini-2.5-pro", "", "", "YABA_GEMINI_KEY"},
		{"newline inside the key", "abc\n123", "gemini-2.5-pro", "", "", "YABA_GEMINI_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, model, err := geminiSettings(c.key, c.model)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, c.wantErr)
				}
				if c.key != "" && strings.Contains(err.Error(), strings.TrimSpace(c.key)) {
					t.Errorf("the error %q echoes the key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key != c.wantKey || model != c.wantModel {
				t.Errorf("got (%q, %q), want (%q, %q)", key, model, c.wantKey, c.wantModel)
			}
		})
	}
}

// TestAnUnversionedDatabaseIsRecognisedAndCopied: a database from before
// schema versioning reports version 0 exactly like a new file, and used to be
// migrated -- by the riskiest migration there is -- with no backup at all.
func TestAnUnversionedDatabaseIsRecognisedAndCopied(t *testing.T) {
	dir := t.TempDir()

	fresh, err := db.Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if legacy, err := hasUserTables(fresh); err != nil || legacy {
		t.Errorf("a brand-new file: hasUserTables = %v, %v; want false", legacy, err)
	}

	old, err := db.Open(filepath.Join(dir, "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(`CREATE TABLE expenses (id INTEGER PRIMARY KEY, amount REAL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO expenses(amount) VALUES (12.5)`); err != nil {
		t.Fatal(err)
	}
	if v := db.Version(old); v != 0 {
		t.Fatalf("legacy database reports version %d, want 0", v)
	}
	if legacy, err := hasUserTables(old); err != nil || !legacy {
		t.Fatalf("a database with tables: hasUserTables = %v, %v; want true", legacy, err)
	}

	backups := filepath.Join(dir, "backups")
	path, err := preVersioningBackup(context.Background(), old, backups)
	if err != nil {
		t.Fatalf("preVersioningBackup: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "pre-versioning-") {
		t.Errorf("copy %s is named like a rotating snapshot and would be pruned", path)
	}

	copied, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var amount float64
	if err := copied.QueryRow(`SELECT amount FROM expenses`).Scan(&amount); err != nil || amount != 12.5 {
		t.Errorf("copy holds %v, %v; want the original row", amount, err)
	}

	// A second copy in the same second must not collide with the first.
	again, err := preVersioningBackup(context.Background(), old, backups)
	if err != nil || again == path {
		t.Errorf("second copy = %q, %v; want a distinct file", again, err)
	}
	if _, err := os.Stat(again); err != nil {
		t.Errorf("second copy missing: %v", err)
	}
}

// TestOldDefaultBackupsArePointedOut: an install upgraded from the version
// that wrote snapshots to the cache directory is told where they are, rather
// than finding later that `yaba restore` cannot see them.
func TestOldDefaultBackupsArePointedOut(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // os.UserCacheDir on Linux
	t.Setenv("HOME", cache)           // ...and on macOS
	if runtime.GOOS == "windows" {
		t.Skip("UserCacheDir is not redirectable by env on Windows")
	}
	old, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	old = filepath.Join(old, "YABA", "backups")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	current := filepath.Join(t.TempDir(), "backups")
	warnAboutOldBackupDir(current)
	if buf.Len() != 0 {
		t.Errorf("warned with no old snapshots: %q", buf.String())
	}

	if err := os.WriteFile(filepath.Join(old, "yaba-20260101-000000Z.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnAboutOldBackupDir(current)
	if !strings.Contains(buf.String(), old) || !strings.Contains(buf.String(), current) {
		t.Errorf("warning does not name both directories: %q", buf.String())
	}
}
