package db

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// In WAL mode the committed state spans the database, its -wal and its -shm.

const (
	// DefaultBackupKeep is how many snapshots survive the retention sweep.
	DefaultBackupKeep = 14

	// DefaultBackupEvery is the interval for the in-process timer.
	DefaultBackupEvery = 24 * time.Hour

	// DefaultConfirmEmptyAfter is how long a guarded table has to stay empty, across
	// at least two backup attempts, before Backup believes it is the database's real
	// state rather than a broken snapshot. See acceptEmptied.
	DefaultConfirmEmptyAfter = time.Hour

	// AcceptEmptyEnv, set to a true value, makes every backup accept a guarded table
	// that has gone empty. It exists so a server whose backups are being refused can be
	// unstuck without a rebuild; unset it again afterwards, or the guard stays off.
	AcceptEmptyEnv = "YABA_BACKUP_ACCEPT_EMPTY"

	backupPrefix = "yaba-"
	backupExt    = ".db"
	uploadsSuf   = "-uploads.zip"

	// preEmptyPrefix marks the last snapshot that still held rows in a table which has
	// since gone empty. Snapshots does not list it, so retention never removes it.
	preEmptyPrefix = "pre-empty-"

	// emptiedMarker records, in the backup directory, when the current emptied state
	// was first seen, so a later attempt can tell "still empty" from "newly empty".
	emptiedMarker = ".yaba-emptied"

	// Stamps are UTC, and a second snapshot within one second gets a "-2", "-3"...
	// suffix. Ordering is by the parsed (stamp, suffix) pair, never by name: '-' sorts
	// before '.', so "…Z-2.db" would sort ahead of the "…Z.db" it follows.
	stampLayout = "20060102-150405Z"
)

// Startup and retry timing for BackupLoop. Variables rather than constants so the
// tests do not have to wait a minute.
var (
	// backupStartDelay is the shortest wait before the first scheduled backup. Long
	// enough that a server crash-looping under systemd does not snapshot on every
	// restart, short enough that a server restarted daily still gets backed up.
	backupStartDelay = time.Minute

	// backupRetryAfter caps the wait after a failed backup, so one transient failure
	// does not cost a whole interval of protection.
	backupRetryAfter = time.Hour
)

// guardedTables are the tables whose emptiness means a broken snapshot rather than a
// small one.
var guardedTables = []string{
	"users", "households", "household_members", "transactions", "funds",
}

// Counts is a row count per table, used to compare one snapshot with the last.
type Counts map[string]int64

// Snapshot describes one completed backup.
type Snapshot struct {
	Path    string        // the .db file written
	Uploads string        // the receipts archive, or "" if there were none
	Bytes   int64         // size of the .db file
	Counts  Counts        // row counts, read back out of the snapshot itself
	Pruned  []string      // files removed by the retention sweep
	Took    time.Duration // wall clock for snapshot plus verification
}

// BackupConfig configures Backup.
type BackupConfig struct {
	// Dir is where snapshots are written. Required.
	Dir string

	// UploadDir is the receipts directory. Optional, but a database-only backup restores
	// rows pointing at files that are not there: receipts live on disk, not in SQLite.
	UploadDir string

	// Keep is how many snapshots to retain. Zero means DefaultBackupKeep.
	Keep int

	// AcceptEmpty accepts a snapshot in which a guarded table has gone from rows to
	// none, for an operator who knows the deletion was deliberate (`yaba backup
	// -accept-empty`). AcceptEmptyEnv does the same for a process that cannot be
	// given a flag.
	AcceptEmpty bool

	// ConfirmEmptyAfter is how long an emptied table must persist before it is
	// accepted without an operator. Zero means DefaultConfirmEmptyAfter.
	ConfirmEmptyAfter time.Duration
}

// Backup writes a verified snapshot and sweeps old ones.
func Backup(ctx context.Context, sqlDB *sql.DB, cfg BackupConfig) (Snapshot, error) {
	started := time.Now()

	if strings.TrimSpace(cfg.Dir) == "" {
		return Snapshot{}, errors.New("backup: no directory configured")
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultBackupKeep
	}

	// 0o700: a snapshot is a complete copy of every user's finances, including
	// their bcrypt hashes. It deserves the same protection as the original.
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("backup: create %s: %w", cfg.Dir, err)
	}

	// Read the previous snapshot's counts before writing the new one, so the
	// comparison below has something to compare against.
	previous, prevPath := lastSnapshotCounts(ctx, cfg.Dir)

	path, err := freeSnapshotPath(cfg.Dir, time.Now().UTC())
	if err != nil {
		return Snapshot{}, err
	}

	// VACUUM INTO refuses to overwrite, so a half-written file from a crashed run would
	// block every future backup at this timestamp.
	if _, err := sqlDB.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return Snapshot{}, fmt.Errorf("backup: vacuum into %s: %w", path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("backup: stat %s: %w", path, err)
	}

	counts, err := VerifySnapshot(ctx, path)
	if err != nil {
		// An unusable snapshot is worse than none, because its presence implies
		// a safety that is not there. Delete it and report.
		os.Remove(path)
		return Snapshot{}, fmt.Errorf("backup: %w", err)
	}

	if err := compareCounts(previous, counts, prevPath); err != nil {
		var emptied *EmptiedError
		if !errors.As(err, &emptied) {
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: %w", err)
		}
		accepted, why := acceptEmptied(cfg, emptied, time.Now())
		if !accepted {
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: %w; %s", err, why)
		}
		log.Printf("backup: WARNING: accepting a snapshot in which %s went empty: %s",
			strings.Join(emptied.Tables, ", "), why)
		if kept, err := keepPreEmpty(prevPath); err != nil {
			log.Printf("backup: could not preserve %s outside retention: %v",
				filepath.Base(prevPath), err)
		} else {
			log.Printf("backup: kept %s outside retention, as the last snapshot holding those rows",
				filepath.Base(kept))
		}
	}
	// Whether the counts were normal or an emptied state has just been accepted,
	// nothing is pending confirmation any more.
	clearEmptiedMarker(cfg.Dir)

	snap := Snapshot{Path: path, Bytes: info.Size(), Counts: counts}

	if cfg.UploadDir != "" {
		dest := strings.TrimSuffix(path, backupExt) + uploadsSuf
		n, err := archiveUploads(cfg.UploadDir, dest)
		switch {
		case err != nil:
			// The database refers to these files.  Reporting a snapshot as complete
			// while pruning the last complete pair makes recovery less safe than no
			// backup at all.
			os.Remove(path)
			return Snapshot{}, fmt.Errorf("backup: archive %s: %w", cfg.UploadDir, err)
		case n > 0:
			snap.Uploads = dest
		}
	}

	pruned, err := Prune(cfg.Dir, cfg.Keep)
	if err != nil {
		log.Printf("backup: retention sweep failed: %v", err)
	}
	snap.Pruned = pruned
	snap.Took = time.Since(started)
	return snap, nil
}

// freeSnapshotPath returns a path that does not exist yet: two backups inside one
// second collide, so take a suffix rather than fail.
//
// The suffix is one past the highest already used for this second, not the first
// free one. Otherwise, once retention had removed "…Z.db" and kept "…Z-2.db", the
// next backup in that second would reuse "…Z.db", which sorts as the older of the
// two, and the following prune would delete the snapshot just taken.
func freeSnapshotPath(dir string, at time.Time) (string, error) {
	base := filepath.Join(dir, backupPrefix+at.Format(stampLayout))
	stamp := at.Truncate(time.Second)

	highest := 0
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if t, seq, ok := parseSnapshotName(e.Name()); ok && t.Equal(stamp) && seq > highest {
				highest = seq
			}
		}
	}
	for seq := highest + 1; seq <= highest+100; seq++ {
		path := base + backupExt
		if seq > 1 {
			path = fmt.Sprintf("%s-%d%s", base, seq, backupExt)
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
	}
	return "", fmt.Errorf("backup: %s is already full of snapshots for this second", dir)
}

// VerifySnapshot proves a snapshot is usable and returns its row counts.
func VerifySnapshot(ctx context.Context, path string) (Counts, error) {
	snap, err := openForVerify(ctx, path)
	if err != nil {
		return nil, err
	}
	defer snap.Close()

	// 1. integrity_check returns one row reading "ok", or many rows describing
	//    the damage.
	rows, err := snap.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("integrity_check: %w", err)
	}
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("snapshot %s failed integrity_check: %s",
			filepath.Base(path), strings.Join(problems[:min(len(problems), 3)], "; "))
	}

	// 2. Dangling references.
	fk, err := snap.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	broken := 0
	for fk.Next() {
		broken++
	}
	fkErr := fk.Err()
	fk.Close()
	if fkErr != nil {
		return nil, fmt.Errorf("foreign_key_check on %s: %w", filepath.Base(path), fkErr)
	}
	if broken > 0 {
		return nil, fmt.Errorf("snapshot %s has %d dangling foreign key(s)",
			filepath.Base(path), broken)
	}

	// 3. The schema must be one this build understands, and the application's
	//    own invariants must hold.
	var version int
	if err := snap.QueryRowContext(ctx,
		`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return nil, fmt.Errorf("snapshot %s has no schema_migrations: %w",
			filepath.Base(path), err)
	}
	if version == 0 {
		return nil, fmt.Errorf("snapshot %s records no applied migrations", filepath.Base(path))
	}
	// A snapshot written by a newer build has tables or columns this one
	// doesn't know about. Restoring it would leave Migrate reporting "up to
	// date" while the code runs against a layout it doesn't understand.
	if latest := LatestVersion(); version > latest {
		return nil, fmt.Errorf("snapshot %s was made by a newer version of YABA (schema %d; this build knows up to %d). Restore it with that version or later",
			filepath.Base(path), version, latest)
	}

	if version < 4 {
		return snapshotCounts(ctx, snap)
	}

	var unowned, unhoused int
	if err := snap.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM households h
		         WHERE NOT EXISTS (SELECT 1 FROM household_members m
		                            WHERE m.household_id = h.id AND m.role = 'owner')),
		       (SELECT COUNT(*) FROM users u
		         WHERE NOT EXISTS (SELECT 1 FROM household_members m
		                            WHERE m.user_id = u.id))`,
	).Scan(&unowned, &unhoused); err != nil {
		return nil, fmt.Errorf("snapshot %s: invariant query failed: %w",
			filepath.Base(path), err)
	}
	if unowned > 0 {
		return nil, fmt.Errorf("snapshot %s has %d household(s) with no owner",
			filepath.Base(path), unowned)
	}
	if unhoused > 0 {
		return nil, fmt.Errorf("snapshot %s has %d user(s) with no household",
			filepath.Base(path), unhoused)
	}

	// 4. Counts, for the caller to compare against the previous snapshot.
	counts := Counts{}
	for _, t := range guardedTables {
		var n int64
		if err := snap.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM "`+t+`"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("snapshot %s: count %s: %w", filepath.Base(path), t, err)
		}
		counts[t] = n
	}
	return counts, nil
}

func snapshotCounts(ctx context.Context, snap *sql.DB) (Counts, error) {
	counts := Counts{}
	for _, table := range guardedTables {
		var exists int
		if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			continue
		}
		var n int64
		if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = n
	}
	return counts, nil
}

// openForVerify opens a snapshot read-only. query_only stops the verification
// modifying what it verifies, but a driver that refuses the pragma falls back rather
// than blocking startup on every pending migration.
func openForVerify(ctx context.Context, path string) (*sql.DB, error) {
	// Opening a path that isn't there makes SQLite create it, so a mistyped
	// "-from" used to leave an empty file in the backup folder that then
	// looked like the newest snapshot. Check it is a real, non-empty file first.
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("snapshot %s does not exist", path)
		}
		return nil, fmt.Errorf("snapshot %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("snapshot %s is not a file", path)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("snapshot %s is empty, so it is not a database", filepath.Base(path))
	}
	attempts := []string{
		"?_pragma=query_only(true)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
		"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
	}
	var lastErr error
	for i, params := range attempts {
		snap, err := sql.Open("sqlite", path+params)
		if err == nil {
			snap.SetMaxOpenConns(1)
			if err = snap.PingContext(ctx); err == nil {
				if i > 0 {
					log.Printf("backup: query_only is unavailable, verifying %s without it",
						filepath.Base(path))
				}
				return snap, nil
			}
			snap.Close()
		}
		lastErr = err
	}
	return nil, fmt.Errorf("snapshot %s is not a readable database: %w",
		filepath.Base(path), lastErr)
}

// EmptiedError is the shape of a truncated backup: one or more guarded tables that
// held rows in the previous snapshot and hold none in this one.
type EmptiedError struct {
	Tables   []string // guarded tables that went from rows to none, in guardedTables order
	Was      Counts   // their counts in the previous snapshot
	Previous string   // the previous snapshot's path
}

func (e *EmptiedError) Error() string {
	parts := make([]string, len(e.Tables))
	for i, t := range e.Tables {
		parts[i] = fmt.Sprintf("%s held %d row(s)", t, e.Was[t])
	}
	return fmt.Sprintf("snapshot looks truncated: %s in %s and holds none now",
		strings.Join(parts, ", "), filepath.Base(e.Previous))
}

// key identifies one emptied state, so a marker left by an earlier attempt is only
// taken as confirmation of the same thing.
func (e *EmptiedError) key() string {
	return filepath.Base(e.Previous) + " " + strings.Join(e.Tables, ",")
}

// compareCounts rejects the shape of a truncated backup: a guarded table that had rows
// and now has none. Shrinking without reaching zero is only logged, because deliberate
// deletions (`yaba reset -keep`, a user closing their account) do exactly that.
func compareCounts(prev, now Counts, prevPath string) error {
	if prev == nil {
		return nil
	}
	var emptied *EmptiedError
	for _, t := range guardedTables {
		was, is := prev[t], now[t]
		if was > 0 && is == 0 {
			if emptied == nil {
				emptied = &EmptiedError{Was: Counts{}, Previous: prevPath}
			}
			emptied.Tables = append(emptied.Tables, t)
			emptied.Was[t] = was
			continue
		}
		if is < was {
			log.Printf("backup: %s shrank from %d to %d since %s "+
				"(expected after a deliberate deletion, suspicious otherwise)",
				t, was, is, filepath.Base(prevPath))
		}
	}
	if emptied != nil {
		return emptied
	}
	return nil
}

// acceptEmptied decides whether an emptied guarded table is the database's real state.
//
// Refusing forever is not an option: the comparison is against the newest surviving
// snapshot, and a refused snapshot is deleted, so once the only household is
// legitimately deleted every later backup would be refused too and the server would
// silently go without backups for good. So the emptied state is accepted when either
//
//   - an operator says so (cfg.AcceptEmpty, or AcceptEmptyEnv), or
//   - the same tables have been seen empty, against the same previous snapshot, on
//     an earlier attempt at least ConfirmEmptyAfter ago. A snapshot that is broken
//     by accident does not keep coming out broken in exactly the same way across
//     attempts an hour apart; a database that really is empty does.
//
// Either way the snapshot that still held the rows is then preserved outside
// retention (keepPreEmpty), so accepting cannot rotate the last good copy away.
func acceptEmptied(cfg BackupConfig, e *EmptiedError, now time.Time) (bool, string) {
	if cfg.AcceptEmpty {
		return true, "accepted by the operator (-accept-empty)"
	}
	if envTrue(os.Getenv(AcceptEmptyEnv)) {
		return true, AcceptEmptyEnv + " is set (unset it again, or this check stays off)"
	}

	wait := cfg.ConfirmEmptyAfter
	if wait <= 0 {
		wait = DefaultConfirmEmptyAfter
	}
	marker := filepath.Join(cfg.Dir, emptiedMarker)
	hint := fmt.Sprintf("if that deletion was deliberate, run `yaba backup -accept-empty` "+
		"or set %s=1 once", AcceptEmptyEnv)

	if key, first, ok := readEmptiedMarker(marker); ok && key == e.key() {
		if seen := now.Sub(first); seen >= wait {
			return true, fmt.Sprintf("the same table(s) have been empty on every attempt since %s (%s ago), "+
				"so this is the database's real state", first.UTC().Format(time.RFC3339),
				seen.Round(time.Second))
		}
		return false, fmt.Sprintf("first seen empty at %s; it will be accepted automatically "+
			"if it is still the case after %s, or %s",
			first.UTC().Format(time.RFC3339), first.Add(wait).UTC().Format(time.RFC3339), hint)
	}

	content := e.key() + "\n" + now.UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(marker, []byte(content), 0o600); err != nil {
		log.Printf("backup: could not record %s: %v", marker, err)
	}
	return false, fmt.Sprintf("it will be accepted automatically if it is still the case after %s, or %s",
		now.Add(wait).UTC().Format(time.RFC3339), hint)
}

func readEmptiedMarker(path string) (key string, first time.Time, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		return "", time.Time{}, false
	}
	first, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(lines[1]))
	if err != nil {
		return "", time.Time{}, false
	}
	return strings.TrimSpace(lines[0]), first, true
}

func clearEmptiedMarker(dir string) {
	if err := os.Remove(filepath.Join(dir, emptiedMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("backup: could not remove %s: %v", emptiedMarker, err)
	}
}

// keepPreEmpty preserves the snapshot that still held the rows, and its receipts, under
// a name Snapshots does not list. A hard link costs no space; a copy is the fallback
// for filesystems without links.
func keepPreEmpty(prevPath string) (string, error) {
	dir, base := filepath.Split(prevPath)
	kept := filepath.Join(dir, preEmptyPrefix+base)
	pairs := [][2]string{{prevPath, kept}}
	archive := strings.TrimSuffix(prevPath, backupExt) + uploadsSuf
	if _, err := os.Stat(archive); err == nil {
		pairs = append(pairs, [2]string{archive, filepath.Join(dir, preEmptyPrefix+filepath.Base(archive))})
	}
	for _, p := range pairs {
		if _, err := os.Stat(p[1]); err == nil {
			continue // already preserved by an earlier acceptance
		}
		if err := os.Link(p[0], p[1]); err != nil {
			if err := copyFileTo(p[0], p[1]); err != nil {
				return "", err
			}
		}
	}
	return kept, nil
}

// envTrue reads a boolean the way the server's own envBool does (strconv.ParseBool).
// Anything unparseable is false: this switches a safety check off, so a typo must not.
func envTrue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("WARNING: %s=%q is not a boolean; treating it as false", AcceptEmptyEnv, v)
		return false
	}
	return b
}

// lastSnapshotCounts reads the counts out of the newest snapshot itself rather than a
// sidecar, because a record of what a backup holds is only as trustworthy as its
// agreement with the backup.
func lastSnapshotCounts(ctx context.Context, dir string) (Counts, string) {
	found, err := Snapshots(dir)
	if err != nil || len(found) == 0 {
		return nil, ""
	}
	newest := found[len(found)-1]
	counts, err := VerifySnapshot(ctx, newest)
	if err != nil {
		// The previous snapshot being bad is not a reason to refuse to take a new one.
		log.Printf("backup: previous snapshot %s does not verify: %v",
			filepath.Base(newest), err)
		return nil, ""
	}
	return counts, newest
}

// parseSnapshotName reads the timestamp and same-second sequence number out of a
// snapshot's file name: "yaba-20260801-030000Z.db" is (that instant, 1) and
// "yaba-20260801-030000Z-2.db" is (that instant, 2). ok is false for anything
// freeSnapshotPath would not have written.
func parseSnapshotName(name string) (at time.Time, seq int, ok bool) {
	if !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupExt) {
		return time.Time{}, 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix), backupExt)
	if len(rest) < len(stampLayout) {
		return time.Time{}, 0, false
	}
	at, err := time.Parse(stampLayout, rest[:len(stampLayout)])
	if err != nil {
		return time.Time{}, 0, false
	}
	suffix := rest[len(stampLayout):]
	if suffix == "" {
		return at, 1, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(suffix, "-"))
	if !strings.HasPrefix(suffix, "-") || err != nil || n < 2 {
		return time.Time{}, 0, false
	}
	return at, n, true
}

// Snapshots lists snapshot paths in the directory, oldest first.
//
// Only names freeSnapshotPath writes are listed. Prune deletes from the front of this
// list, so a file that merely happens to start with "yaba-" and end in ".db" must not
// be on it.
func Snapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	type named struct {
		path string
		at   time.Time
		seq  int
	}
	var found []named
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		at, seq, ok := parseSnapshotName(e.Name())
		if !ok {
			continue
		}
		found = append(found, named{filepath.Join(dir, e.Name()), at, seq})
	}
	// By (timestamp, sequence), not by name: '-' sorts before '.', so lexically
	// "…Z-2.db" comes before the "…Z.db" it was written after, and prune, the
	// truncation baseline and "restore the newest" would all pick the wrong file.
	sort.Slice(found, func(i, j int) bool {
		if !found[i].at.Equal(found[j].at) {
			return found[i].at.Before(found[j].at)
		}
		return found[i].seq < found[j].seq
	})
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths, nil
}

// newestSnapshotTime is when the newest snapshot in dir was taken, read from its name.
func newestSnapshotTime(dir string) (time.Time, bool) {
	found, err := Snapshots(dir)
	if err != nil || len(found) == 0 {
		return time.Time{}, false
	}
	at, _, ok := parseSnapshotName(filepath.Base(found[len(found)-1]))
	return at, ok
}

// Prune keeps the newest keep snapshots and the receipt archive belonging to each.
func Prune(dir string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	found, err := Snapshots(dir)
	if err != nil {
		return nil, err
	}
	if len(found) <= keep {
		return nil, nil
	}

	var removed []string
	for _, path := range found[:len(found)-keep] {
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("prune %s: %w", path, err)
		}
		removed = append(removed, path)

		// The receipts archive is part of the same snapshot, so it goes at the same time.
		archive := strings.TrimSuffix(path, backupExt) + uploadsSuf
		if err := os.Remove(archive); err == nil {
			removed = append(removed, archive)
		} else if !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("prune %s: %w", archive, err)
		}
	}
	return removed, nil
}

// archiveUploads zips the receipt directory alongside the snapshot.
//
// Receipts change far less often than the database, and every backup used to
// zip all of them again, so a site keeping fourteen snapshots held fourteen
// copies of every receipt. Each archive now records a fingerprint of what it
// holds (in the zip comment); when the newest existing archive already holds
// exactly these files, the new one is a hard link to it -- the same bytes under
// a second name, costing no space, and still removed independently by Prune.
// Restore is unchanged: every snapshot still has its own archive.
func archiveUploads(uploadDir, dest string) (int, error) {
	info, err := os.Stat(uploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", uploadDir)
	}

	fingerprint, files, err := uploadsFingerprint(uploadDir)
	if err != nil || files == 0 {
		return 0, err
	}
	if prev := newestArchiveWith(filepath.Dir(dest), fingerprint); prev != "" {
		if err := os.Link(prev, dest); err == nil {
			return files, nil
		}
		// A filesystem without hard links: write a fresh archive instead.
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(out)
	if err := zw.SetComment(fingerprint); err != nil {
		zw.Close()
		out.Close()
		os.Remove(dest)
		return 0, err
	}

	stored := 0
	walkErr := filepath.WalkDir(uploadDir, func(path string, d os.DirEntry, err error) error {
		// A receipt (or a whole folder) deleted between the listing and the
		// read -- a user discarding one while the nightly backup runs -- is
		// simply not in this archive. It used to fail the whole backup.
		if errors.Is(err, fs.ErrNotExist) && path != uploadDir {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(uploadDir, path)
		if err != nil {
			return err
		}
		// Opened before its entry is created, so a file that has just vanished
		// leaves no empty entry behind.
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		// Always forward slashes: a zip written on Windows with backslashes in
		// its entry names does not extract correctly anywhere else.
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		stored++
		return nil
	})

	// The zip writer flushes its central directory on Close, so it must close before the
	// file beneath it.
	zipErr := zw.Close()
	fileErr := out.Close()

	for _, err := range []error{walkErr, zipErr, fileErr} {
		if err != nil {
			os.Remove(dest)
			return 0, err
		}
	}
	if stored == 0 {
		// An empty archive is noise in the backup directory.
		os.Remove(dest)
	}
	return stored, nil
}

// uploadsFingerprint summarises the receipt directory as a hash of every file's
// relative path, size and modification time, and counts the files. Receipts are
// written once and never edited, so this changes exactly when one is added or
// removed.
func uploadsFingerprint(dir string) (string, int, error) {
	h := sha256.New()
	files := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		// Gone between the listing and the stat: deleted mid-backup, and so
		// not part of what is being backed up.
		if errors.Is(err, fs.ErrNotExist) && path != dir {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		files++
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return "yaba-uploads-v1 " + hex.EncodeToString(h.Sum(nil)), files, nil
}

// newestArchiveWith returns the newest receipt archive in dir whose recorded
// fingerprint is want, or "" when the newest archive differs (or none exists).
// Only the newest is checked: it is the one the last backup wrote.
func newestArchiveWith(dir, want string) string {
	found, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*"+uploadsSuf))
	if err != nil || len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	newest := found[len(found)-1]
	zr, err := zip.OpenReader(newest)
	if err != nil {
		return ""
	}
	defer zr.Close()
	if zr.Comment != want {
		return ""
	}
	return newest
}

// BackupLoop takes a snapshot on a timer until ctx is cancelled, inside the server
// process, so backups do not depend on an external scheduler.
//
// The schedule is anchored to the newest snapshot on disk rather than to process
// start. A plain ticker only fired a full interval after start, so a server restarted
// more often than that (a deploy a day, say) never took a scheduled backup at all.
func BackupLoop(ctx context.Context, sqlDB *sql.DB, cfg BackupConfig, every time.Duration) {
	if every <= 0 {
		every = DefaultBackupEvery
	}
	wait := firstBackupDelay(cfg.Dir, every, time.Now())
	log.Printf("backups: next snapshot in %s", wait.Round(time.Second))

	t := time.NewTimer(wait)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		next := every
		snap, err := Backup(ctx, sqlDB, cfg)
		switch {
		case err != nil && ctx.Err() != nil:
			return // shutting down mid-backup is not a failure worth shouting about
		case err != nil:
			// Loud, and every time. A backup that quietly stopped working two months ago is
			// worse than none at all, because it removed the worry without removing the risk.
			next = min(every, backupRetryAfter)
			log.Printf("BACKUP FAILED: %v (retrying in %s)", err, next)
		default:
			log.Printf("backup: %s (%s) verified in %s%s",
				filepath.Base(snap.Path), humanBytes(snap.Bytes),
				snap.Took.Round(time.Millisecond), prunedNote(snap.Pruned))
		}
		t.Reset(next)
	}
}

// firstBackupDelay is how long BackupLoop waits before its first snapshot: until the
// newest snapshot is an interval old, but never less than backupStartDelay, and
// immediately-ish (backupStartDelay) when there is no snapshot or it is overdue.
func firstBackupDelay(dir string, every time.Duration, now time.Time) time.Duration {
	newest, ok := newestSnapshotTime(dir)
	if !ok {
		return backupStartDelay
	}
	wait := newest.Add(every).Sub(now)
	switch {
	case wait < backupStartDelay:
		return backupStartDelay
	case wait > every:
		// A snapshot stamped in the future means the clock moved; do not wait longer
		// than one interval because of it.
		return every
	}
	return wait
}

func prunedNote(pruned []string) string {
	if len(pruned) == 0 {
		return ""
	}
	return fmt.Sprintf(", pruned %d old file(s)", len(pruned))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// BackupDirFor is where snapshots go when nothing configures a directory: "backups"
// beside the database file.
//
// This used to be os.UserCacheDir(), which depends on who is asking. Under systemd
// with User=yaba the server wrote to ~yaba/.cache, while `yaba restore` run by an
// admin looked in the admin's own cache and found nothing -- and a cache directory is
// by definition something the system may clean. A path derived from the database is
// the same for every user and every process that agrees on the database. Production
// should still set YABA_BACKUP_DIR (ideally another disk) and copy snapshots off the
// machine, because a backup beside its original shares its fate.
func BackupDirFor(dbPath string) string {
	dir := filepath.Dir(dbPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "backups")
}

// DefaultBackupDir is BackupDirFor applied to YABA_DB (or "yaba.db", the default both
// binaries use), for a flag default computed before flags are parsed. A caller that
// knows its database path after parsing should prefer BackupDirFor(thatPath), so a
// -db flag moves the default with it.
func DefaultBackupDir() string {
	dbPath := strings.TrimSpace(os.Getenv("YABA_DB"))
	if dbPath == "" {
		dbPath = "yaba.db"
	}
	return BackupDirFor(dbPath)
}
