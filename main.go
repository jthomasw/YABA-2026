// Command yaba runs the YABA budgeting server. main reads configuration, wires the
// packages together and listens: the schema lives in internal/db, the SQL in
// internal/store, and request handling in internal/web.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/envfile"
	"github.com/jthomasw/YABA-2026/internal/mail"
	"github.com/jthomasw/YABA-2026/internal/store"
	"github.com/jthomasw/YABA-2026/internal/web"
	"github.com/jthomasw/YABA-2026/internal/worker"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	// Settings from .env first, so the flags below see them as defaults. A
	// variable already in the environment wins over the file.
	{
		path := envfile.Path()
		set, err := envfile.Load(path)
		if err != nil {
			log.Fatalf("fatal: reading %s: %v", path, err)
		}
		if len(set) > 0 {
			// Names only: the values include secrets.
			log.Printf("config: loaded %d setting(s) from %s: %s", len(set), path, strings.Join(set, ", "))
		}
	}

	var (
		// Loopback by default, so a fresh install is not reachable from the
		// network until somebody decides it should be (YABA_ADDR=:8000), or
		// puts a reverse proxy in front -- which is the recommended setup.
		addr = flag.String("addr", envOr("YABA_ADDR", "127.0.0.1:8000"),
			"address to listen on (\":8000\" for every interface)")
		dbPath    = flag.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
		uploadDir = flag.String("uploads", envOr("YABA_UPLOADS", "uploads"), "directory for stored receipts")
		secure    = flag.Bool("secure-cookie", envBool("YABA_SECURE_COOKIE", true),
			"mark the session cookie Secure (disable only for local HTTP development)")
		timezone = flag.String("timezone", envOr("YABA_TIMEZONE", ""),
			"IANA timezone for date calculations, e.g. America/New_York (default: the server's own local timezone)")
		// 5MB used to be the default here and was too tight: a modern phone
		// camera photo of a receipt routinely runs 6-10MB, so an upload that
		// size failed with "that file is larger than 5 MB" and looked, at a
		// glance, like the page had simply bounced back to the chooser with
		// nothing having happened.
		maxUpload = flag.Int64("max-upload-mb", envInt64("YABA_MAX_UPLOAD_MB", 15),
			"maximum receipt upload size in megabytes")
		backupDir = flag.String("backup-dir", envOr("YABA_BACKUP_DIR", ""),
			`directory for database snapshots, or "off" to disable backups (default: a "backups" directory next to the database)`)
		backupEvery = flag.Duration("backup-every", envDuration("YABA_BACKUP_EVERY", db.DefaultBackupEvery),
			"how often to take a snapshot while running")
		backupKeep = flag.Int("backup-keep", int(envInt64("YABA_BACKUP_KEEP", db.DefaultBackupKeep)),
			"how many snapshots to retain")

		// Links in emails must be absolute, and the host cannot come from the request:
		// honouring a client-supplied Host header would let anyone mint a reset link.
		baseURL  = flag.String("base-url", envOr("YABA_BASE_URL", ""), "externally reachable root, for links in emails")
		smtpHost = flag.String("smtp-host", envOr("YABA_SMTP_HOST", ""), "SMTP host; unset writes emails to the log")
		smtpPort = flag.Int("smtp-port", int(envInt64("YABA_SMTP_PORT", 587)), "SMTP port (587 STARTTLS, 465 TLS)")
		smtpUser = flag.String("smtp-user", envOr("YABA_SMTP_USER", ""), "SMTP username")
		smtpFrom = flag.String("smtp-from", envOr("YABA_SMTP_FROM", ""), `sender, e.g. "YABA <you@example.com>"`)

		// The key itself is never a flag, for the same reason as the SMTP
		// password: a flag value sits in the process list for every other user
		// on the machine to read.
		geminiModel = flag.String("gemini-model", envOr("YABA_GEMINI_MODEL", worker.DefaultGeminiModel),
			"Gemini model used to read receipts")
	)
	flag.Parse()

	if err := checkNoArgs(flag.Args()); err != nil {
		log.Fatalf("fatal: %v", err)
	}

	geminiKey, geminiModelID, err := geminiSettings(os.Getenv(geminiKeyEnv), *geminiModel)
	if err != nil {
		log.Fatalf("fatal: %v", err)
	}

	cfg := config{
		addr:     *addr,
		baseURL:  *baseURL,
		timezone: *timezone,
		mail: mail.Config{
			Host: *smtpHost,
			Port: *smtpPort,
			User: *smtpUser,
			// Never a flag: a password on the command line is visible in the
			// process list to every other user on the machine.
			Pass:    os.Getenv("YABA_SMTP_PASS"),
			From:    *smtpFrom,
			BaseURL: *baseURL,
		},
		dbPath:       *dbPath,
		uploadDir:    *uploadDir,
		secureCookie: *secure,
		maxUploadMB:  *maxUpload,
		backupDir:    *backupDir,
		backupEvery:  *backupEvery,
		backupKeep:   *backupKeep,
		geminiKey:    geminiKey,
		geminiModel:  geminiModelID,
	}
	// The default is resolved here, after flag.Parse, so that -db moves the
	// backups with it. It is the same rule the maintenance CLI uses, which is
	// what lets `yaba restore` find what the server wrote without being told.
	switch dir := strings.TrimSpace(cfg.backupDir); {
	case strings.EqualFold(dir, "off"):
		cfg.backupDir = ""
	case dir == "":
		cfg.backupDir = db.BackupDirFor(cfg.dbPath)
		warnAboutOldBackupDir(cfg.backupDir)
	}

	// A deployment that has TLS but forgot the flag sends the session cookie in
	// clear on the first plain-HTTP request anyone's browser makes to it -- a
	// typed hostname, an old bookmark, a link in an email. Nothing failed, so
	// nothing said anything; now it does, loudly, once, at startup.
	if !cfg.secureCookie && strings.HasPrefix(strings.ToLower(cfg.baseURL), "https://") {
		log.Printf("WARNING: -base-url is https:// but the session cookie is not marked Secure.")
		log.Printf("         It will be sent over plain HTTP if anyone reaches this site without TLS.")
		log.Printf("         Set YABA_SECURE_COOKIE=1 (or pass -secure-cookie).")
	}

	// The cookie is Secure by default now, which a browser will simply not send
	// back over plain HTTP -- so local development over http://localhost would
	// silently fail to stay signed in unless this is pointed out once, clearly,
	// instead of looking like a broken login.
	if cfg.secureCookie && !strings.HasPrefix(strings.ToLower(cfg.baseURL), "https://") {
		log.Printf("NOTE: the session cookie is marked Secure, so it will not be sent over plain HTTP.")
		log.Printf("      For local development at http://localhost, set YABA_SECURE_COOKIE=0")
		log.Printf("      (or pass -secure-cookie=false).")
	}

	if err := run(cfg); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// config is every setting main reads, gathered so run's signature does not grow
// a seventh positional argument that callers get in the wrong order.
type config struct {
	addr         string
	dbPath       string
	uploadDir    string
	secureCookie bool
	timezone     string
	maxUploadMB  int64
	backupDir    string
	backupEvery  time.Duration
	backupKeep   int
	baseURL      string
	mail         mail.Config
	geminiKey    string
	geminiModel  string
}

// checkNoArgs refuses positional arguments. This binary is the web server and
// takes only flags, but the README once built it as ./yaba -- the same name as
// the maintenance tool -- and "./yaba backup" did not fail: flag.Parse stops
// at the first non-flag, so it quietly started a second server against a
// yaba.db relative to wherever the command was run.
func checkNoArgs(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q: this is the web server; "+
			"the maintenance tool is built from ./cmd/yaba", args[0])
	}
	return nil
}

// geminiSettings cleans and checks the Gemini configuration before anything
// starts, so a mistake stops the server with a message naming the variable
// rather than failing every upload later with a 400 or 404 buried in the log.
// Nothing here touches the network: whether the key is accepted is something
// only Google can say, and the worker reports that loudly when it happens.
//
// The key is trimmed because production had a leading space and an env file
// with CRLF line endings leaves a \r on the end. It is never echoed back, not
// even in an error.
func geminiSettings(rawKey, rawModel string) (key, model string, err error) {
	key = strings.TrimSpace(rawKey)
	if strings.IndexFunc(key, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", "", fmt.Errorf("%s contains a space or control character in the middle; "+
			"an API key is a single unbroken token", geminiKeyEnv)
	}

	model, err = worker.NormalizeGeminiModel(rawModel)
	if err != nil {
		return "", "", fmt.Errorf("invalid YABA_GEMINI_MODEL (or -gemini-model): %w; "+
			"use the model ID from Google's model list, not its display name", err)
	}
	return key, model, nil
}

func run(cfg config) error {
	addr, dbPath, uploadDir := cfg.addr, cfg.dbPath, cfg.uploadDir
	secureCookie, maxUploadMB := cfg.secureCookie, cfg.maxUploadMB

	// Set once, here, before anything starts serving: store.Today() reads this
	// on every request from then on, with no lock, because it never changes again.
	if cfg.timezone != "" {
		loc, err := time.LoadLocation(cfg.timezone)
		if err != nil {
			return fmt.Errorf("invalid -timezone %q: %w", cfg.timezone, err)
		}
		store.SetLocation(loc)
	}

	sessionKey, err := sessionKey()
	if err != nil {
		return err
	}

	// The lock is what lets `yaba restore`, `reset` and `repair` refuse to run
	// underneath a live server. It is advisory and the OS drops it if this
	// process dies, so a crash never leaves a stale lock behind.
	lock, err := db.AcquireLock(dbPath)
	if errors.Is(err, db.ErrLocked) {
		return fmt.Errorf("%w (is another yaba-server, or a yaba restore/reset/repair, using %s?)", err, dbPath)
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	backupCfg := db.BackupConfig{
		Dir:       cfg.backupDir,
		UploadDir: uploadDir,
		Keep:      cfg.backupKeep,
	}

	// Back up before changing the schema, and treat a failure as fatal.
	if backupCfg.Dir != "" {
		pending, err := db.Pending(sqlDB)
		if err != nil {
			return fmt.Errorf("check pending migrations: %w", err)
		}
		switch {
		case pending > 0 && db.Version(sqlDB) == 0:
			// Version 0 means one of two very different things. A brand-new
			// file has nothing to lose. A database from before migrations were
			// versioned has everything to lose, and migration 1 -- which rebuilds
			// every table around it -- is the riskiest one there is, so it must
			// not run without a copy. db.Backup cannot take that copy, because
			// VerifySnapshot rightly refuses a snapshot with no schema_migrations.
			legacy, err := hasUserTables(sqlDB)
			if err != nil {
				return fmt.Errorf("inspect database before migrating: %w", err)
			}
			if !legacy {
				log.Printf("startup: new database — nothing to back up before migrating")
				break
			}
			log.Printf("startup: database has tables but no migration history — copying it before migrating")
			path, err := preVersioningBackup(context.Background(), sqlDB, backupCfg.Dir)
			if err != nil {
				return fmt.Errorf("pre-migration backup of an unversioned database failed, "+
					"so the migration was not attempted: %w", err)
			}
			log.Printf("startup: backed up to %s", path)
		case pending > 0:
			log.Printf("startup: %d migration(s) pending — taking a backup first", pending)
			snap, err := db.Backup(context.Background(), sqlDB, backupCfg)
			if err != nil {
				return fmt.Errorf("pre-migration backup failed, so the migration "+
					"was not attempted: %w", err)
			}
			log.Printf("startup: backed up to %s", snap.Path)
		}
	} else {
		log.Printf("WARNING: backups are off. Set YABA_BACKUP_DIR to enable them.")
	}

	if err := db.Migrate(sqlDB); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	st := store.New(sqlDB)

	// The receipt queue is drained by a background goroutine, started before the server
	// so anything left over from a previous run is picked up immediately.
	ctx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	// Housekeeping, now and then daily: a server that ran for months used to
	// sweep only when it started, so the tables grew for as long as it stayed up.
	go housekeepingLoop(ctx, st, 24*time.Hour)

	// The Gemini processor reads the amount, date and merchant off the image by
	// sending it to Google's API. It returns nil when YABA_GEMINI_KEY is not
	// set, and worker.New turns a nil processor into the review-only behaviour
	// this always had -- so a deployment with no key degrades to queueing the
	// receipt for manual entry rather than failing every upload.
	//
	// Whatever it reads is a draft the user confirms. Nothing here writes a
	// transaction. See internal/worker/gemini_processor.go and about.html: a
	// deployment that sets this key is sending receipt photos to Google.
	var processor worker.Processor
	if p := worker.NewGeminiProcessor(cfg.geminiKey, cfg.geminiModel); p != nil {
		processor = p
		log.Printf("receipts: %s", p.Describe())
	} else {
		log.Printf("WARNING: receipts: %s is not set, so receipts will NOT be read automatically;", geminiKeyEnv)
		log.Printf("         uploads are saved for the user to enter by hand. Put the key in .env")
		log.Printf("         (see .env.example) or the environment, then restart.")
	}
	receipts := worker.New(st, processor, 5*time.Second)
	go receipts.Run(ctx)

	// Recurring income and expenses are caught up in the background too, not
	// only when somebody opens Add Income or Add Expense -- otherwise a
	// paycheck due today was missing from the dashboard until they did.
	go recurringLoop(ctx, st, time.Hour)

	// Snapshots on a timer, sharing the worker's cancellation so they stop with
	// the process.
	//
	// They read through a handle of their own: the server's handle is a single
	// connection, and a snapshot taken through it held up every request until
	// it finished.
	if backupCfg.Dir != "" {
		backupSrc, err := db.OpenReadOnly(dbPath)
		if err != nil {
			return err
		}
		defer backupSrc.Close()
		log.Printf("backups: %s every %s, keeping %d",
			backupCfg.Dir, cfg.backupEvery, cfg.backupKeep)
		go db.BackupLoop(ctx, backupSrc, backupCfg, cfg.backupEvery)
	}

	mailer := mail.New(cfg.mail)

	srv, err := web.New(st, web.Config{
		Addr:         addr,
		Mail:         mailer,
		SessionKey:   sessionKey,
		UploadDir:    uploadDir,
		SecureCookie: secureCookie,
		MaxUploadMB:  maxUploadMB,
		// Passing the worker in lets an upload nudge it awake rather than waiting a whole
		// interval. web knows it only as a Waker, so the dependency does not point back.
		Worker:         receipts,
		ReceiptReading: processor != nil,
	})
	if err != nil {
		return err
	}

	// addr is :8000 when only a port was given and 127.0.0.1:8000 when a host was too.
	shown := addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	log.Printf("YABA listening on http://%s", shown)

	// Serve until a termination signal arrives, then stop accepting and let
	// whatever is in flight finish. Cancelling this context also stops the
	// worker and the backup loop, and the deferred sqlDB.Close() finally runs --
	// none of which happened before, because nothing ever returned from here.
	serveErr := srv.ListenAndServe(signalled())

	// Give the worker a moment to notice and put down whatever it is holding. A
	// receipt it was reading goes back in the queue without losing an attempt
	// (see Worker.processNext), so a deploy no longer counts against an upload.
	//
	// This runs even when serving ended in an error -- a grace period that ran
	// out, say. Returning first skipped it, and main's log.Fatalf then exited
	// without the worker stopping or the database closing.
	cancelWorker()
	receipts.Stop(5 * time.Second)
	return serveErr
}

// housekeepingLoop deletes rows nothing needs any more, once at startup and
// then every interval, until ctx is cancelled. Every query that reads these
// tables already ignores expired rows, so a failure is logged, not fatal.
func housekeepingLoop(ctx context.Context, st *store.Store, every time.Duration) {
	sweeps := []struct {
		what string
		run  func(context.Context) (int64, error)
	}{
		{"expired session(s)", st.PurgeExpiredSessions},
		{"expired reset token(s)", st.PurgeExpiredResets},
		{"stale rate-limit window(s)", st.PurgeOldAttempts},
		{"long-expired invitation(s)", st.PurgeStaleInvites},
		{"unused form token(s)", st.PurgeOldFormTokens},
		{"old notification(s)", st.PurgeOldNotifications},
		{"audit entr(ies) past retention", st.PurgeOldAudit},
	}
	run := func() {
		for _, sweep := range sweeps {
			n, err := sweep.run(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				log.Printf("housekeeping: could not purge %s: %v", sweep.what, err)
			case n > 0:
				log.Printf("housekeeping: purged %d %s", n, sweep.what)
			}
		}
	}

	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// recurringLoop records every recurring income and expense that has fallen
// due, once at startup and then every interval, until ctx is cancelled. The
// catch-up is idempotent (one occurrence per schedule per due date), so
// running it here as well as on page load cannot double-charge anybody.
func recurringLoop(ctx context.Context, st *store.Store, every time.Duration) {
	run := func() {
		n, err := st.ProcessAllDueRecurring(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("recurring: %v", err)
		}
		if n > 0 {
			log.Printf("recurring: caught up %d household(s)", n)
		}
	}

	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// signalled returns a context that is cancelled on SIGINT or SIGTERM.
//
// SIGTERM is what a systemd restart, a container stop and most deploy scripts
// send; SIGINT is Ctrl-C.
//
// stop() is released as soon as the first signal arrives, which hands the
// default handler back: an operator who does not want to wait out the grace
// period can press Ctrl-C again and the process dies immediately.
//
// Nothing is logged here. ListenAndServe announces the shutdown itself, in
// order, and logging from a goroutine racing it produced "signal received"
// after "all requests finished".
func signalled() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}

// hasUserTables reports whether a database already holds tables of its own,
// as opposed to a file SQLite has only just created. Together with
// db.Version == 0 it identifies a database from before schema versioning.
func hasUserTables(sqlDB *sql.DB) (bool, error) {
	var n int
	err := sqlDB.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		  AND name <> 'schema_migrations'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("list tables: %w", err)
	}
	return n > 0, nil
}

// preVersioningBackup copies an unversioned database into dir before its first
// migration and checks the copy is sound, returning where it went.
//
// It is deliberately not named like db.Backup's snapshots: Prune would rotate
// it away after a couple of weeks, and the snapshot comparison would try to
// read it as a modern database. This copy is the one that cannot be
// recreated, so it stays until somebody deletes it by hand.
func preVersioningBackup(ctx context.Context, sqlDB *sql.DB, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	stamp := time.Now().UTC().Format("20060102-150405Z")
	var path string
	for i := 1; ; i++ {
		path = filepath.Join(dir, fmt.Sprintf("pre-versioning-%s.db", stamp))
		if i > 1 {
			path = filepath.Join(dir, fmt.Sprintf("pre-versioning-%s-%d.db", stamp, i))
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		if i >= 100 {
			return "", fmt.Errorf("%s already holds too many copies for this second", dir)
		}
	}

	if _, err := sqlDB.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("vacuum into %s: %w", path, err)
	}
	if err := checkCopy(ctx, path); err != nil {
		// A copy that will not open is worse than none, because its presence
		// implies a safety that is not there.
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// checkCopy opens a fresh copy read-only and runs SQLite's integrity check
// over it, which is as much as can be asked of a database whose schema this
// build does not yet understand.
func checkCopy(ctx context.Context, path string) error {
	c, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open copy %s: %w", path, err)
	}
	defer c.Close()

	var result string
	if err := c.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("check copy %s: %w", path, err)
	}
	if result != "ok" {
		return fmt.Errorf("copy %s failed integrity_check: %s", path, result)
	}
	return nil
}

// sessionKeyEnv is the variable holding the cookie signing key.
const sessionKeyEnv = "YABA_SESSION_KEY"

// geminiKeyEnv is the variable holding the Gemini API key used to read
// receipts. Never a flag, and never logged -- same reasoning as sessionKeyEnv.
const geminiKeyEnv = "YABA_GEMINI_KEY"

// sessionKey loads the cookie signing key, which has to come from the environment:
// a literal in the source sits in git history, and anyone who has seen it can forge
// a session. Unset generates a random key, so a restart signs everyone out.
func sessionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(sessionKeyEnv))

	if raw == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate session key: %w", err)
		}
		// The key itself is NOT logged. It used to be, as a convenience -- but
		// what was printed was not a suggestion, it was the live key this
		// process is about to sign and encrypt every cookie with. Anything that
		// reads logs (a shared journal, log shipping, a support bundle pasted
		// into a ticket) would have been handed it.
		//
		// The replacement is a command that generates a DIFFERENT key, which is
		// what somebody setting this up actually wants.
		log.Printf("WARNING: %s is not set, so a random key was generated for this process.", sessionKeyEnv)
		log.Printf("         Everyone will be signed out when it restarts.")
		log.Printf("         Generate a persistent key with:")
		log.Printf("           openssl rand -base64 32")
		log.Printf("         then set %s to the value it prints.", sessionKeyEnv)
		return key, nil
	}

	// Accept base64 first, falling back to treating the value as raw bytes so a
	// hand-written passphrase also works.
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) >= 32 {
		return key, nil
	}
	if len(raw) < 32 {
		return nil, errors.New(sessionKeyEnv + " must be at least 32 bytes; " +
			"generate one with: openssl rand -base64 32")
	}
	return []byte(raw), nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("WARNING: %s=%q is not a boolean; using %v", key, v, fallback)
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("WARNING: %s=%q is not a positive duration (e.g. 24h); using %s",
			key, v, fallback)
		return fallback
	}
	return d
}

func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("WARNING: %s=%q is not a positive integer; using %d", key, v, fallback)
		return fallback
	}
	return n
}

// warnAboutOldBackupDir points out snapshots left where older versions wrote
// them by default (the service account's cache directory). Backups now default
// to a directory next to the database instead, so those older snapshots are no
// longer pruned, compared against or found by `yaba restore`. They are not
// moved automatically: the operator decides whether to keep them.
func warnAboutOldBackupDir(current string) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return
	}
	old := filepath.Join(cache, "YABA", "backups")
	if old == current {
		return
	}
	matches, err := filepath.Glob(filepath.Join(old, "*.db"))
	if err != nil || len(matches) == 0 {
		return
	}
	log.Printf("backups: %d older snapshot(s) are still in %s, where previous versions wrote them by default; "+
		"new snapshots go to %s. Move them there (or set YABA_BACKUP_DIR=%s) to keep using them.",
		len(matches), old, current, old)
}
