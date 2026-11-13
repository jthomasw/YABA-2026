// Command yaba is the maintenance tool for a YABA database. It is kept out of the
// server binary so a destructive action is not one mistyped character away from the
// thing you run every day.
//
//	reset     empty the database, or reduce it to a single account
//	passwd    set an account's password, or list accounts
//	backup    take a verified snapshot, or list the ones you have
//	restore   put a snapshot back
//	repair    find fund movements that could not have happened, and remove them
//
// Run a subcommand with -h for its own flags. The defaults come from the same
// environment variables the server reads (YABA_DB, YABA_UPLOADS, YABA_BACKUP_DIR,
// YABA_BACKUP_KEEP), so running it with the server's environment file points it at the
// server's data.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/envfile"
)

func main() {
	// The same .env the server reads, so the CLI finds the same database,
	// uploads and backups without being told.
	if _, err := envfile.Load(envfile.Path()); err != nil {
		fmt.Fprintf(os.Stderr, "yaba: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// app carries the command's input, output and environment, so the tests can drive
// every subcommand in-process: typed confirmations included, and without os.Exit.
type app struct {
	// One reader for the whole run. A fresh bufio.Reader per prompt would buffer past
	// the first line and silently swallow the answer to the next one.
	in *bufio.Reader
	// term is stdin when it is a file (a terminal, usually), so a password
	// prompt can switch echo off. Nil when the input is not a file, as in tests.
	term   *os.File
	out    io.Writer
	errOut io.Writer
	getenv func(string) string
}

// run dispatches on the subcommand and returns the process exit status: 0 success,
// 1 the command failed, 2 it was used wrongly. Each subcommand owns its own FlagSet, so
// two can define -db without colliding and -h prints only the flags that apply.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	a := &app{in: bufio.NewReader(stdin), out: stdout, errOut: stderr, getenv: getenv}
	if f, ok := stdin.(*os.File); ok {
		a.term = f
	}

	if len(args) < 1 {
		a.usage()
		return 2
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)

	// Flags shared by several subcommands, with the server's environment as defaults.
	dbFlag := func(usage string) *string {
		return fs.String("db", a.envOr("YABA_DB", "yaba.db"), usage+" (env YABA_DB)")
	}
	uploadsFlag := func(usage string) *string {
		return fs.String("uploads", a.envOr("YABA_UPLOADS", "uploads"), usage+" (env YABA_UPLOADS)")
	}
	dirFlag := func(usage string) *string {
		return fs.String("dir", a.envOr("YABA_BACKUP_DIR", ""),
			usage+` (env YABA_BACKUP_DIR; default "backups" beside -db, as the server does)`)
	}

	var exec func() error
	switch cmd {
	case "reset":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			uploadDir = uploadsFlag("receipt directory to clear")
			dir       = dirFlag("where the safety snapshot goes")
			keep      = fs.String("keep", "", "email of the one account to keep; everything else is deleted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
			backup    = fs.Bool("backup", true, "copy the database aside before wiping it")
		)
		exec = func() error {
			return a.runReset(*dbPath, *uploadDir, backupDir(*dir, *dbPath), *keep, *yes, *backup)
		}

	case "passwd":
		var (
			dbPath = dbFlag("path to the SQLite database")
			email  = fs.String("email", "", "email address of the account to update")
			list   = fs.Bool("list", false, "list accounts and exit")
		)
		// There is deliberately no -password flag: a flag value sits in the
		// process list and the shell history. The password is read from
		// stdin -- typed at a prompt that does not echo, or piped in as two
		// lines (the password and its confirmation).
		exec = func() error { return a.runPasswd(*dbPath, *email, *list) }

	case "backup":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			dir       = dirFlag("directory for snapshots")
			uploadDir = uploadsFlag("receipt directory to archive alongside")
			keep      = fs.Int("keep", int(a.envInt64("YABA_BACKUP_KEEP", db.DefaultBackupKeep)),
				"how many snapshots to retain (env YABA_BACKUP_KEEP)")
			list        = fs.Bool("list", false, "list existing snapshots and exit")
			acceptEmpty = fs.Bool("accept-empty", false,
				"accept a snapshot in which users, households, transactions or funds went from rows to none "+
					"(after a deliberate deletion of the last of them)")
		)
		exec = func() error {
			return a.runBackup(*dbPath, backupDir(*dir, *dbPath), *uploadDir, *keep, *list, *acceptEmpty)
		}

	case "restore":
		var (
			from   = fs.String("from", "", "snapshot to restore (omit to use the newest in -dir)")
			dir    = dirFlag("directory to look in when -from is omitted")
			dbPath = dbFlag("where to write the restored database; the only command that may create it")
			check  = fs.Bool("check", false, "verify the snapshot and stop without writing anything")
		)
		// Accepted so existing scripts keep working, and otherwise ignored: an existing
		// database is always moved aside, never overwritten, so there is nothing to force.
		fs.Bool("force", false, "accepted for compatibility; an existing database is always moved aside, never overwritten")
		exec = func() error { return a.runRestore(*from, backupDir(*dir, *dbPath), *dbPath, *check) }

	case "repair":
		var (
			dbPath    = dbFlag("path to the SQLite database")
			dir       = dirFlag("where the safety snapshot goes")
			uploadDir = uploadsFlag("receipt directory to archive with the snapshot")
			del       = fs.String("delete", "", "comma-separated transaction ids to remove; only ids this command reported are accepted")
			yes       = fs.Bool("yes", false, "skip the confirmation prompt")
		)
		exec = func() error { return a.runRepair(*dbPath, backupDir(*dir, *dbPath), *uploadDir, *del, *yes) }

	case "help", "-h", "-help", "--help":
		a.usage()
		return 0

	default:
		fmt.Fprintf(stderr, "yaba: unknown subcommand %q\n\n", cmd)
		a.usage()
		return 2
	}

	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// "yaba backup list" would otherwise parse as no flags at all and take a backup.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "yaba %s: unexpected argument %q (flags start with -; see yaba %s -h)\n",
			cmd, fs.Arg(0), cmd)
		return 2
	}
	if err := exec(); err != nil {
		fmt.Fprintf(stderr, "yaba %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func (a *app) usage() {
	fmt.Fprint(a.errOut, `yaba - maintenance tool for a YABA database

Usage:
  yaba reset   [-db path] [-uploads dir] [-dir path] [-keep email] [-yes] [-backup=false]
  yaba passwd  [-db path] [-email addr] [-list]
  yaba backup  [-db path] [-dir path] [-uploads dir] [-keep n] [-list] [-accept-empty]
  yaba restore [-from snapshot] [-dir path] [-db path] [-check]
  yaba repair  [-db path] [-dir path] [-uploads dir] [-delete ids] [-yes]

Defaults come from YABA_DB, YABA_UPLOADS, YABA_BACKUP_DIR and YABA_BACKUP_KEEP, as
for the server. Every command except restore refuses a -db that does not exist.
reset, restore and repair also refuse to run while the server has the database open.

Run "yaba <subcommand> -h" for the flags of one subcommand.
`)
}

// backupDir applies the server's rule: an explicit directory wins, otherwise
// "backups" beside the database.
func backupDir(dir, dbPath string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return db.BackupDirFor(dbPath)
}

// requireDB refuses a database path that does not exist. db.Open would otherwise
// create an empty database there, and the command would go on to fail confusingly
// ("no such table") or report that there was nothing to do -- typically because it
// was run from the wrong directory with the relative default.
func requireDB(dbPath string) error {
	info, err := os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		where := ""
		if !filepath.IsAbs(dbPath) {
			if abs, err := filepath.Abs(dbPath); err == nil {
				where = " (" + abs + ", relative to the current directory)"
			}
		}
		return fmt.Errorf("no database at %s%s; nothing was created. "+
			"Pass -db or set YABA_DB to the server's database "+
			"(in production, YABA_DB in /etc/yaba.env)", dbPath, where)
	}
	if err != nil {
		return fmt.Errorf("check %s: %w", dbPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a database file", dbPath)
	}
	return nil
}

// lockDB takes the database's lock for a command that must not run beside the server.
func lockDB(dbPath string) (*db.Lock, error) {
	lock, err := db.AcquireLock(dbPath)
	if errors.Is(err, db.ErrLocked) {
		return nil, fmt.Errorf("%w. Stop the server first (sudo systemctl stop yaba), "+
			"run this again, then start it", err)
	}
	return lock, err
}

// confirm asks for a typed word, so a destructive step is never one Enter away.
func (a *app) confirm(question, word string) bool {
	if question != "" {
		fmt.Fprint(a.out, question, " ")
	}
	fmt.Fprintf(a.out, "Type '%s' to continue: ", word)
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line) == word
}

// envInt64 reads a positive integer from the environment.
func (a *app) envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(a.getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		fmt.Fprintf(a.errOut, "warning: %s=%q is not a positive integer; using %d\n",
			key, v, fallback)
		return fallback
	}
	return n
}

// envOr matches the server's envOr exactly (trimmed, empty means unset), so both read
// one environment file the same way.
func (a *app) envOr(key, fallback string) string {
	if v := strings.TrimSpace(a.getenv(key)); v != "" {
		return v
	}
	return fallback
}
