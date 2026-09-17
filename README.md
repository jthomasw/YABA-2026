# YABA — Yet Another Budgeting App

A household budgeting app: income and expenses, recurring schedules, priority-ordered
monthly expense buckets funded from income as it arrives, savings funds with an
emergency-fund target sized from your own essential spending, shared budgets with
owner/editor/viewer roles, and receipt upload with OCR that proposes an amount for you
to confirm.

Go, SQLite and server-rendered HTML. No build step for the frontend, no JavaScript
framework, and every page works with scripting switched off.

---

## Requirements

| | |
|---|---|
| Go | 1.25 or later (see `go.mod`) |
| SQLite | none — `modernc.org/sqlite` is pure Go, so there is no cgo and no system library |
| tesseract | optional, for receipt OCR |
| poppler (`pdftoppm`) | optional, for PDF receipts |
| ImageMagick (`convert`) | optional, for HEIC/WebP receipts |

Dependencies are vendored, so a clean checkout builds with no network access.

Without tesseract the app still accepts receipt uploads — it queues them for manual
entry instead of reading them. Nothing fails; the feature degrades. The startup log
says which of the three tools it found.

---

## Quick start

```bash
git clone https://github.com/jthomasw/YABA-2026.git
cd YABA-2026

go build ./...
go test ./...

# A random session key is generated if you do not set one, which is fine
# locally: it just signs you out whenever you restart.
go run . -db yaba.db -uploads uploads -addr :8000 -backup-dir off
```

Then open <http://localhost:8000> and create an account.

Leave `YABA_SECURE_COOKIE` off for local development. A Secure cookie is not sent over
`http://localhost` at all, so with it on you cannot sign in.

---

## Architecture

```
main.go              configuration, wiring, signals, graceful shutdown
  │
  ├── internal/web       HTTP: routing, middleware, sessions, templates, handlers
  │     │                 (templates and static assets are embedded in the binary)
  │     ↓
  ├── internal/store     every SQL statement in the application
  │     ↓
  ├── internal/db        schema, versioned migrations, backup and restore
  │
  ├── internal/money     Cents — integer arithmetic, never float
  ├── internal/insight   derived figures: runway, projections, observations
  ├── internal/ocr       tesseract subprocess, preprocessing, receipt parsing
  ├── internal/worker    background receipt queue
  ├── internal/mail      SMTP, or log-only when unconfigured
  │
  └── cmd/yaba           maintenance CLI (separate binary, deliberately)
```

Things worth knowing before changing any of it:

**Money is `money.Cents`, an `int64` of cents.** No monetary value is ever held in a
float. `Cents.Float()` exists for chart JSON and must not be used for arithmetic.
Amounts are parsed by `money.Parse`, which is strict about what it accepts — see its
tests for exactly how strict, and why.

**Every query is scoped.** `store.Scope{HouseholdID, UserID}` is threaded through every
call, and every statement that reads or writes a row carries `household_id` in its
`WHERE` clause. That is what stops a guessed id reaching another household's data, and
it is checked in the store rather than the handler so there is one place to get it
right.

**The connection pool is one connection** (`db.Open` sets `SetMaxOpenConns(1)`). SQLite
has a single writer; serialising at the pool means a read-modify-write inside one
transaction cannot interleave with another request. It also means a slow query blocks
everything, which is why every catch-up loop is bounded.

**Migrations are append-only and versioned.** Add a new numbered entry to
`migrations()` in `internal/db/db.go`; never edit an existing one, because it has
already run on real databases. A pending migration triggers a verified backup first,
and a failed backup aborts the migration.

**Templates are a registry, not a directory scan.** A new page under
`internal/web/templates/` must also be added to the `pages` list in
`internal/web/web.go`, or rendering it returns `unknown page`.

**Scripts are enhancements.** Anything hidden on load is hidden *by the script*, not by
the server, so the page works without it. Server-rendering `hidden` or
`style="display:none"` on something whose only opener is a `<button type="button">`
makes that feature unreachable — which is a mistake this codebase has made more than
once.

---

## Configuration

Every setting is a flag and an environment variable; the flag wins. See
[`.env.example`](.env.example) for the annotated list, and `./yaba -h` for the flags.

The one that must be set in production:

```bash
YABA_SESSION_KEY=$(openssl rand -base64 32)
```

Unset, a random key is generated per process, so a restart signs everyone out.

`YABA_SMTP_PASS` is read from the environment only, never a flag — a flag value is
visible in the process list to every other user on the machine.

Do not set `YABA_MAIL_DEBUG` in production. With no SMTP configured it writes every
undeliverable message to the log in full, and a password-reset email contains a live
bearer token for the account.

---

## Testing

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l .
```

`gofmt -l` is the one to watch on Windows: the tree is stored with CRLF line endings,
which `gofmt` reports as unformatted. Compare against a copy with LF endings if you
need a clean answer, or rely on CI, which checks out with LF.

The suite is ordinary `go test` — no build tags, no external services, no fixtures to
load. Store and web tests each build a real migrated SQLite database in a temp
directory, so they exercise the actual schema and the actual middleware rather than a
mock.

---

## Deployment

The app is one static binary with its templates and assets embedded, so deployment is
copying a file and restarting a unit.

```bash
GOOS=linux GOARCH=amd64 go build -o yaba .
```

Behind nginx, with the app bound to localhost:

```
server {
    listen 80;
    server_name yaba.example.com;
    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

`X-Forwarded-For` matters: rate limiting is keyed partly on the client address, and
without it every request appears to come from the proxy. The header is believed only
when the immediate peer is loopback — which is why the app binds to `127.0.0.1` above.
A proxy on another host would need that trust rule widened first, deliberately, since a
forwarded header from an arbitrary peer is just a claim.

A systemd unit wants `EnvironmentFile=/etc/yaba.env` holding the values from
`.env.example`, `User=yaba`, and the hardening directives (`ProtectSystem`,
`PrivateTmp`, `NoNewPrivileges`, `ReadWritePaths` limited to the database, uploads and
backup directories). See [`internal/ocr/README.md`](internal/ocr/README.md) for the
tesseract, poppler and ImageMagick packages the EC2 box needs for receipt OCR.

Shutdown is graceful: on `SIGTERM` the server stops accepting connections, lets
in-flight requests finish (up to 20 seconds), stops the background worker and closes
the database. `systemctl restart` during a save no longer cuts the response.

### Health

`GET /healthz` returns `200 ok` when the database answers a query within two seconds,
and `503` when it does not. Public and deliberately uninformative — no version, no
schema number, no error text. Point your load balancer or monitor at it rather than
`/`, which renders a template and touches the session store.

### Logs

One line per request, at `INFO`, with a short request id:

```
INFO  3d4ad9db GET /dashboard 200 14ms
ERROR 3d4ad9db POST /expense: create transaction: database is locked
```

The id is also returned in the `X-Request-Id` header and quoted on the 500 page, so a
user report can be tied to the exact request. Query strings are deliberately omitted —
they carry search terms and reset tokens. No password, token, cookie or email address
is ever logged.

---

## Backup and restore

Backups exist and run by default. `YABA_BACKUP_DIR` (default: platform-dependent, see
`-h`) receives a verified snapshot every `YABA_BACKUP_EVERY` (default 6h), keeping
`YABA_BACKUP_KEEP` (default 14). A snapshot is also taken immediately before any
pending migration, and if that backup fails the migration is not attempted.

Each snapshot is verified after being written — a snapshot with no `schema_migrations`
table is rejected rather than stored as if it were good.

```bash
# take one now, and list what you have
./yaba backup -db /var/lib/yaba/yaba.db -dir /var/backups/yaba
./yaba backup -list -dir /var/backups/yaba

# check a snapshot without touching the live database
./yaba restore -check -from /var/backups/yaba/yaba-2026-09-17T06-00-00.db

# put one back (stop the server first)
systemctl stop yaba
./yaba restore -from /var/backups/yaba/yaba-2026-09-17T06-00-00.db -db /var/lib/yaba/yaba.db
systemctl start yaba
```

Restore moves the database it replaces aside as `*.before-restore-<timestamp>` rather
than deleting it, so a restore from the wrong snapshot is itself recoverable.

Two caveats worth being honest about. Snapshots land on the same disk as the database
unless `YABA_BACKUP_DIR` points elsewhere, so they do not protect against losing that
disk — copy them off the machine. And the backup covers the database; uploaded receipt
images live under `YABA_UPLOADS` and need their own copy.

---

## Maintenance CLI

Kept out of the server binary so a destructive action is not one mistyped character
away from the thing you run every day.

```
yaba reset   [-db path] [-uploads dir] [-keep email] [-yes] [-backup=false]
yaba passwd  [-db path] [-email addr] [-password pw] [-list]
yaba backup  [-db path] [-dir path] [-uploads dir] [-keep n] [-list]
yaba restore [-from snapshot] [-dir path] [-db path] [-force] [-check]
yaba repair  [-db path] [-delete ids] [-yes]
```

```bash
go build -o yaba-cli ./cmd/yaba
./yaba-cli passwd -list -db /var/lib/yaba/yaba.db
```

---

## Troubleshooting

**Everyone was signed out after a restart.** `YABA_SESSION_KEY` is not set, so a random
one was generated. The startup log says so.

**I cannot sign in locally.** `YABA_SECURE_COOKIE` is on and you are on `http://`. The
browser is refusing to send the cookie.

**`render: unknown page "x.html"`.** The template was added but not registered in the
`pages` list in `internal/web/web.go`.

**Receipts upload but are never read.** tesseract was not found. The startup log prints
which tools were located; `YABA_TESSERACT` overrides the search.

**A reset email never arrives.** No SMTP host is configured. Check the startup log — it
says so explicitly, and says which variables to set.

**Rate limiting blocks everyone at once.** The app is behind a proxy that is not sending
`X-Forwarded-For`, so every request shares one apparent address.

**`database is locked`.** Something else has the file open — a second server, or a
`sqlite3` session. The pool is one connection by design; two processes are one too many.

**`gofmt -l .` lists files that look fine.** CRLF line endings. See Testing above.
