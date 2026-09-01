// Command yaba runs the YABA budgeting server.
//
// main does three things and nothing else: read configuration, wire the
// packages together, and start listening. The schema lives in internal/db, the
// SQL in internal/store, and the request handling in internal/web.
//
// The previous main.go also held every CREATE TABLE statement and a
// hand-rolled migration routine that re-ran ALTER TABLE on every boot.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jthomasw/YABA-2026/internal/db"
	"github.com/jthomasw/YABA-2026/internal/mail"
	"github.com/jthomasw/YABA-2026/internal/store"
	"github.com/jthomasw/YABA-2026/internal/web"
	"github.com/jthomasw/YABA-2026/internal/worker"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	var (
		addr      = flag.String("addr", envOr("YABA_ADDR", ":8000"), "address to listen on")
		dbPath    = flag.String("db", envOr("YABA_DB", "yaba.db"), "path to the SQLite database")
		uploadDir = flag.String("uploads", envOr("YABA_UPLOADS", "uploads"), "directory for stored receipts")
		secure    = flag.Bool("secure-cookie", envBool("YABA_SECURE_COOKIE", false),
			"mark the session cookie Secure (enable when serving over HTTPS)")
		maxUpload = flag.Int64("max-upload-mb", envInt64("YABA_MAX_UPLOAD_MB", 5),
			"maximum receipt upload size in megabytes")
		backupDir = flag.String("backup-dir", envOr("YABA_BACKUP_DIR", db.DefaultBackupDir()),
			`directory for database snapshots, or "off" to disable backups`)
		backupEvery = flag.Duration("backup-every", envDuration("YABA_BACKUP_EVERY", db.DefaultBackupEvery),
			"how often to take a snapshot while running")
		backupKeep = flag.Int("backup-keep", int(envInt64("YABA_BACKUP_KEEP", db.DefaultBackupKeep)),
			"how many snapshots to retain")

		// Links in emails have to be absolute, and the host cannot be taken from
		// the request: honouring a client-supplied Host header would let anyone
		// mint a password reset link pointing wherever they liked.
		baseURL  = flag.String("base-url", envOr("YABA_BASE_URL", ""), "externally reachable root, for links in emails")
		smtpHost = flag.String("smtp-host", envOr("YABA_SMTP_HOST", ""), "SMTP host; unset writes emails to the log")
		smtpPort = flag.Int("smtp-port", int(envInt64("YABA_SMTP_PORT", 587)), "SMTP port (587 STARTTLS, 465 TLS)")
		smtpUser = flag.String("smtp-user", envOr("YABA_SMTP_USER", ""), "SMTP username")
		smtpFrom = flag.String("smtp-from", envOr("YABA_SMTP_FROM", ""), `sender, e.g. "YABA <you@example.com>"`)

	)
	flag.Parse()

	cfg := config{
		addr:    *addr,
		baseURL: *baseURL,
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
	}
	if strings.EqualFold(strings.TrimSpace(cfg.backupDir), "off") {
		cfg.backupDir = ""
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
	maxUploadMB  int64
	backupDir    string
	backupEvery  time.Duration
	backupKeep   int
	baseURL      string
	mail         mail.Config
}

func run(cfg config) error {
	addr, dbPath, uploadDir := cfg.addr, cfg.dbPath, cfg.uploadDir
	secureCookie, maxUploadMB := cfg.secureCookie, cfg.maxUploadMB
	sessionKey, err := sessionKey()
	if err != nil {
		return err
	}

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
	//
	// A migration is the most dangerous thing that happens to this file, and the
	// one moment a backup is unarguably worth its cost. Refusing to start is the
	// right response: continuing would apply an irreversible change with no way
	// back, which is precisely the situation the backup exists to prevent.
	if backupCfg.Dir != "" {
		pending, err := db.Pending(sqlDB)
		if err != nil {
			return fmt.Errorf("check pending migrations: %w", err)
		}
		switch {
		case pending > 0 && db.Version(sqlDB) == 0:
			// A database with no schema at all. There is nothing to lose, and
			// VerifySnapshot rightly refuses a snapshot with no schema_migrations
			// table -- so attempting one here would make every first run fatal.
			log.Printf("startup: new database — nothing to back up before migrating")
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

	// Sweep sessions that can no longer be used. Nothing depends on this for
	// correctness -- an expired row is refused whether or not it still exists --
	// so a failure is logged rather than fatal. It is here so the table does not
	// grow forever on a server that is restarted more often than it is logged
	// into.
	if n, err := st.PurgeExpiredSessions(context.Background()); err != nil {
		log.Printf("startup: could not purge expired sessions: %v", err)
	} else if n > 0 {
		log.Printf("startup: purged %d expired session(s)", n)
	}

	// The same reasoning for the three other tables that accumulate rows nobody
	// can use: spent reset tokens, lapsed invitations, and login-attempt windows
	// that have aged out. None of them affect correctness -- every one is filtered
	// by the queries that read them -- so a failure here is logged, not fatal.
	if n, err := st.PurgeExpiredResets(context.Background()); err != nil {
		log.Printf("startup: could not purge reset tokens: %v", err)
	} else if n > 0 {
		log.Printf("startup: purged %d expired reset token(s)", n)
	}
	if n, err := st.PurgeOldAttempts(context.Background()); err != nil {
		log.Printf("startup: could not purge login attempts: %v", err)
	} else if n > 0 {
		log.Printf("startup: purged %d stale login-attempt window(s)", n)
	}
	if n, err := st.PurgeStaleInvites(context.Background()); err != nil {
		log.Printf("startup: could not purge stale invitations: %v", err)
	} else if n > 0 {
		log.Printf("startup: purged %d long-expired invitation(s)", n)
	}
	if n, err := st.PurgeOldFormTokens(context.Background()); err != nil {
		log.Printf("startup: could not purge form tokens: %v", err)
	} else if n > 0 {
		log.Printf("startup: purged %d unused form token(s)", n)
	}

	// The receipt queue is drained by a background goroutine, so an upload
	// handler returns as soon as the file is on disk. Started before the server
	// so that anything left over from a previous run is picked up immediately.
	ctx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	// nil means the default processor: the receipt is stored and queued, and the
	// amount is typed in. Passing a processor here is the seam for reading it
	// automatically, and nothing else in the wiring would change.
	receipts := worker.New(st, nil, 5*time.Second)
	go receipts.Run(ctx)

	// Snapshots on a timer, sharing the worker's cancellation so they stop with
	// the process.
	if backupCfg.Dir != "" {
		log.Printf("backups: %s every %s, keeping %d",
			backupCfg.Dir, cfg.backupEvery, cfg.backupKeep)
		go db.BackupLoop(ctx, sqlDB, backupCfg, cfg.backupEvery)
	}

	mailer := mail.New(cfg.mail)

	srv, err := web.New(st, web.Config{
		Addr:         addr,
		Mail:         mailer,
		SessionKey:   sessionKey,
		UploadDir:    uploadDir,
		SecureCookie: secureCookie,
		MaxUploadMB:  maxUploadMB,
		// Passing the worker in lets an upload nudge it awake rather than
		// waiting up to one interval. The web package only knows it as a Waker,
		// so the dependency does not point back at worker.
		Worker: receipts,
	})
	if err != nil {
		return err
	}

	// addr is ":8000" when only a port was given and "127.0.0.1:8000" when a host
	// was too. Only the first form needs a host pasted on to become a real URL;
	// the old line printed "http://localhost127.0.0.1:8000" for the second.
	shown := addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	log.Printf("YABA listening on http://%s", shown)
	return srv.ListenAndServe()
}

// sessionKeyEnv is the variable holding the cookie signing key.
const sessionKeyEnv = "YABA_SESSION_KEY"

// sessionKey loads the cookie signing key.
//
// The old code used a literal: sessions.NewCookieStore([]byte("super-secret-key")).
// That string is in the repository's git history, so anyone who has ever seen
// the source can forge a session cookie for any account. A signing key has to
// come from the environment.
//
// When the variable is unset a random key is generated so the app still starts
// for local development, with a warning: the consequence is that restarting the
// server signs out everyone, which is the correct trade-off for a default.
func sessionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(sessionKeyEnv))

	if raw == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate session key: %w", err)
		}
		log.Printf("WARNING: %s is not set, so a random key was generated.", sessionKeyEnv)
		log.Printf("         Everyone will be signed out when this process restarts.")
		log.Printf("         Set a persistent key with:")
		log.Printf("           %s=%s", sessionKeyEnv, base64.StdEncoding.EncodeToString(key))
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
