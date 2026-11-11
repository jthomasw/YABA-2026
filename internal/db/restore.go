package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sidecars are the files that, in WAL mode, hold part of a database's committed
// state. They belong with the main file wherever it goes.
var sidecars = []string{"-wal", "-shm"}

// Restore puts a snapshot back at dbPath, verifying it first so a corrupt backup
// cannot destroy a working database on its way to being discovered.
//
// A database already at dbPath is replaced only with force, and even then it is never
// deleted: it is checkpointed and moved aside, together with any -wal and -shm, as
// dbPath.before-restore-<stamp>[-wal|-shm]. The paths it was moved to are returned.
// Because the sidecars keep their suffix relative to the new name, opening the
// set-aside file with SQLite still sees every commit it held.
//
// The caller must hold the database's lock (AcquireLock) for the duration. Moving a
// database out from under a running server would leave the server writing to a file
// that is no longer the database, and every write it accepted afterwards would vanish.
func Restore(ctx context.Context, snapshotPath, dbPath string, force bool) ([]string, error) {
	if _, err := VerifySnapshot(ctx, snapshotPath); err != nil {
		return nil, fmt.Errorf("restore refused: %w", err)
	}

	existing, statErr := os.Stat(dbPath)
	if statErr == nil && !force {
		return nil, fmt.Errorf("%s already exists; pass -force to overwrite it", dbPath)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("restore: %w", statErr)
	}
	if statErr == nil && existing.IsDir() {
		return nil, fmt.Errorf("restore: %s is a directory", dbPath)
	}

	// Anything at dbPath, including sidecars orphaned by a crash, is set aside rather
	// than deleted. An orphaned -wal left in place would be replayed into the restored
	// file, which it does not belong to.
	aside, err := setAside(ctx, dbPath, "before-restore")
	if err != nil {
		return nil, fmt.Errorf("restore: %w; nothing was replaced", err)
	}

	if err := copyFileTo(snapshotPath, dbPath); err != nil {
		// Put the original back, so a failed restore leaves things as they were.
		if rbErr := moveBack(aside, dbPath); rbErr != nil {
			return aside, fmt.Errorf("restore: copy %s: %w (and moving the original back failed: %v; "+
				"it is at %s)", snapshotPath, err, rbErr, strings.Join(aside, ", "))
		}
		return nil, fmt.Errorf("restore: copy %s: %w; the original is back in place", snapshotPath, err)
	}

	// A restore is typically run as root (sudo) on behalf of a service account. A
	// replacement owned by root would leave the server unable to open its own database,
	// so the new file takes the owner of the one it replaced.
	if existing != nil {
		if err := matchOwner(dbPath, existing); err != nil {
			log.Printf("restore: could not give %s the owner of the database it replaced: %v", dbPath, err)
		}
	}
	return aside, nil
}

// setAside moves the database at dbPath, and any sidecars, to dbPath.<label>-<stamp>,
// first folding the WAL into the main file where it can. It returns the new paths, the
// main file first, or nil if there was nothing to move.
func setAside(ctx context.Context, dbPath, label string) ([]string, error) {
	present := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	mainExists := present(dbPath)
	var parts []string // suffixes to move: "" for the main file
	if mainExists {
		parts = append(parts, "")
	}
	for _, s := range sidecars {
		if present(dbPath + s) {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if !mainExists {
		log.Printf("restore: %s is missing but has sidecars; moving them aside", dbPath)
	}

	base, err := freeAsidePath(dbPath, label, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	// Fold uncheckpointed commits into the main file, so the set-aside copy is complete
	// on its own. If that is impossible -- the file being replaced is often the damaged
	// one -- the sidecars are moved with it instead, which loses nothing either.
	//
	// SQLite is not trusted with the only copy of the WAL while it tries: on a file it
	// cannot read as a database it unlinks the -wal and -shm when the connection
	// closes. So a raw copy goes to the set-aside name first, and is replaced by the
	// original below if the original survives, or discarded once a checkpoint has
	// provably folded every frame into the main file.
	walCopy := ""
	if mainExists && isSQLiteFile(dbPath) {
		if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() > 0 {
			if err := copyFileTo(dbPath+"-wal", base+"-wal"); err != nil {
				return nil, fmt.Errorf("preserve %s-wal before checkpointing: %w", dbPath, err)
			}
			walCopy = base + "-wal"
		}
		if err := checkpoint(ctx, dbPath); err != nil {
			log.Printf("restore: could not checkpoint %s (%v); moving its -wal and -shm aside with it",
				dbPath, err)
		} else if walCopy != "" {
			os.Remove(walCopy)
			walCopy = ""
		}
		// A clean checkpoint and close normally removes the sidecars, so look again.
		parts = parts[:1]
		for _, s := range sidecars {
			if present(dbPath + s) {
				parts = append(parts, s)
			}
		}
	}

	// Main file first: if a later rename fails, the ones already done are undone, so the
	// set is never split between two names.
	var moved []string
	for _, s := range parts {
		if err := os.Rename(dbPath+s, base+s); err != nil {
			if rbErr := moveBack(moved, dbPath); rbErr != nil {
				return moved, fmt.Errorf("move %s%s aside: %w (and undoing the earlier moves failed: %v)",
					dbPath, s, err, rbErr)
			}
			if walCopy != "" {
				return nil, fmt.Errorf("move %s%s aside: %w (a copy of its WAL is at %s)",
					dbPath, s, err, walCopy)
			}
			return nil, fmt.Errorf("move %s%s aside: %w", dbPath, s, err)
		}
		moved = append(moved, base+s)
		if base+s == walCopy {
			walCopy = "" // the original replaced the copy
		}
	}
	if walCopy != "" {
		// The checkpoint failed and SQLite removed the original: the copy is the WAL now.
		moved = append(moved, walCopy)
	}
	return moved, nil
}

// isSQLiteFile reports whether path starts with the SQLite header. Anything else is
// moved aside byte for byte without SQLite ever opening it.
func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, 16)
	if _, err := io.ReadFull(f, header); err != nil {
		return false
	}
	return string(header) == "SQLite format 3\x00"
}

// freeAsidePath returns dbPath.<label>-<stamp>, with a numeric suffix if two set-asides
// land in the same second, checking every name the set would take.
func freeAsidePath(dbPath, label string, at time.Time) (string, error) {
	stem := dbPath + "." + label + "-" + at.Format(stampLayout)
	for i := 1; i <= 100; i++ {
		base := stem
		if i > 1 {
			base = fmt.Sprintf("%s-%d", stem, i)
		}
		free := true
		for _, s := range append([]string{""}, sidecars...) {
			if _, err := os.Lstat(base + s); err == nil {
				free = false
				break
			}
		}
		if free {
			return base, nil
		}
	}
	return "", fmt.Errorf("no free name to move %s aside to", dbPath)
}

// moveBack undoes setAside for the given paths, each of which ends in its suffix
// relative to the set-aside base name.
func moveBack(moved []string, dbPath string) error {
	if len(moved) == 0 {
		return nil
	}
	base := moved[0]
	for _, s := range sidecars {
		base = strings.TrimSuffix(base, s)
	}
	var errs []error
	for i := len(moved) - 1; i >= 0; i-- {
		suffix := strings.TrimPrefix(moved[i], base)
		if err := os.Rename(moved[i], dbPath+suffix); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkpoint copies every committed WAL frame into the main database file and
// truncates the WAL, using a connection of its own that is closed before returning
// (Windows cannot rename a file that is still open).
func checkpoint(ctx context.Context, dbPath string) error {
	// No journal_mode pragma here, unlike Open: this must not change the file's mode,
	// only fold in what is already committed. A short busy timeout, because the caller
	// holds the lock and nothing should be competing.
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1)

	var busy, logFrames, done int
	err = conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done)
	closeErr := conn.Close()
	switch {
	case err != nil:
		return err
	case busy != 0:
		return errors.New("another connection prevented the checkpoint from completing")
	case closeErr != nil:
		return closeErr
	}
	return nil
}

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to a temporary name in the destination directory and rename, so an interrupted
	// copy cannot leave half a database where the real one was.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
