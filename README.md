# YABA — Yet Another Budgeting App

A household budgeting app: income and expenses, recurring schedules, priority-ordered
monthly expense buckets funded from income as it arrives, savings funds with an
emergency-fund target sized from your own essential spending, shared budgets with
owner/editor/viewer roles, and receipt upload read by the Gemini API, which proposes
an amount for you to confirm.

Go, SQLite and server-rendered HTML. No build step for the frontend, no JavaScript
framework, and every page works with scripting switched off.

---

## Requirements

| | |
|---|---|
| Go | 1.25 or later (see `go.mod`) |
| SQLite | none — `modernc.org/sqlite` is pure Go, so there is no cgo and no system library |
| a Gemini API key | optional, for reading receipts (`YABA_GEMINI_KEY`) |

Dependencies are vendored, so a clean checkout builds with no network access.

Without `YABA_GEMINI_KEY` set, the app still accepts receipt uploads — it queues them
for manual entry instead of reading them. Nothing fails; the feature degrades. The
startup log says whether a key was found.

Setting the key means every receipt photo is sent to Google's Gemini API to be read.
Nothing else the app stores leaves the machine it runs on — see the About page, which
says so.

---

## Quick start

```bash
git clone https://github.com/jthomasw/YABA-2026.git
cd YABA-2026

go build -o yaba-server .        # the web server
go build -o yaba ./cmd/yaba      # the maintenance CLI
go test ./...

# Settings live in .env, which the server reads at startup (copy .env.example).
# For receipt reading, put your Gemini key in it: YABA_GEMINI_KEY=...
cp .env.example .env

# A random session key is generated if you do not set one, which is fine
# locally: it just signs you out whenever you restart.
YABA_SECURE_COOKIE=0 ./yaba-server -db yaba.db -uploads uploads -addr :8000 -backup-dir off
```

Then open <http://localhost:8000> and create an account.

The session cookie is marked `Secure` **by default**, and a browser will not send a
Secure cookie back over plain `http://` -- so without the `YABA_SECURE_COOKIE=0` above
you could not stay signed in locally. Turn it off only for plain-HTTP testing like
this. Any deployment reachable from a network must serve HTTPS and leave it on; the
server logs a warning at startup if `YABA_BASE_URL` is `https://` while it is off, and
a note if it is on while `YABA_BASE_URL` is not `https://`.

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
  ├── internal/ocr       receipt file-type sniffing (JPEG/PNG/HEIC/PDF/...)
  ├── internal/worker    background receipt queue; reads receipts via the Gemini API
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
everything, which is why every catch-up loop is bounded -- and why scheduled backups
read through a separate read-only handle (`db.OpenReadOnly`) instead.

**Bearer tokens are stored hashed.** The `sessions` and `password_resets` tables hold
`store.TokenHash` of each token, never the token, so a copy of the database or a backup
cannot be used to sign in or reset a password.

**Where the code lives.** `internal/web` and `internal/store` are split by feature:
`auth.go`, `transactions.go`, `entry.go` (Add Income / Add Expense), `funds.go`,
`buckets.go`, `receipts.go`, `household.go`, `password.go` and so on in `web`; the same
names plus `recurring.go`, `sessions.go`, `ratelimit.go` and `receipt_jobs.go` in
`store`.

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

Most settings are both a flag and an environment variable, and the flag wins. Secrets
are the exception: `YABA_SESSION_KEY`, `YABA_SMTP_PASS` and `YABA_GEMINI_KEY` are read
from the environment only, never a flag, because a flag value is visible in the process
list to every other user on the machine. See [`.env.example`](.env.example) for the
annotated list, and `./yaba-server -h` for the flags.

The one that must be set in production:

```bash
YABA_SESSION_KEY=$(openssl rand -base64 32)
```

Unset, a random key is generated per process, so a restart signs everyone out.

`YABA_SECURE_COOKIE` defaults to on and should stay on behind HTTPS; see Quick start
for the one case where it is turned off.

### Receipt reading (Gemini)

| Variable | |
|---|---|
| `YABA_GEMINI_KEY` | An API key from <https://aistudio.google.com/apikey>. Paste it bare: no quotes, no spaces around `=`. Unset, uploads are queued for manual entry. |
| `YABA_GEMINI_MODEL` | Optional. An API **model id**, such as the default `gemini-3.1-flash-lite` (`DefaultGeminiModel` in `internal/worker/gemini_processor.go`) -- not a display name like "Gemini Flash". The server refuses to start with an invalid model id rather than failing every upload later. |

Receipt reading needs outbound HTTPS to `generativelanguage.googleapis.com`. There is
nothing to install locally; the old tesseract pipeline is gone.

Each receipt is read for its merchant, date, total, **every line item** and **every
printed tax line**. The confirmation form lists the items, then adds the tax lines (and
tip) as their own rows, plus an "Other" or "Discount / adjustment" row if some prices
could not be read, so the breakdown always adds up to the total paid and can be saved
as it stands. Added rows are shown in italics with a dashed border.

Without a key, the upload page says that automatic reading is off, and a receipt is
saved for you to enter by hand. The same message appears if Google rejects the key; the
server log then names the reason.

### Email

The mail server must offer encryption: implicit TLS on port 465, or STARTTLS on 587.
A server that offers neither is refused rather than sent a password-reset link in
plain text. The one exception is a relay on the same machine (`localhost`,
`127.0.0.1`), such as a local Postfix or a development mail catcher.

Invitations are limited, because each one is an email sent through your relay to an
address the sender chose: 20 per account per day (new invitations and resends
together), and 3 per receiving address per day. Sign-ups are limited to 5 per network
address per hour. The limits are `store.InviteLimit`, `store.InviteRecipientLimit` and
`store.SignupLimit`.

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

There are two binaries, each one static with everything embedded:

| Binary | Built from | Installed as | What it is |
|---|---|---|---|
| `yaba-server` | `go build -o yaba-server .` | `/usr/local/bin/yaba-server` | the web server systemd runs |
| `yaba` | `go build -o yaba ./cmd/yaba` | `/usr/local/bin/yaba` | the maintenance CLI (`backup`, `restore`, ...) |

The production unit runs `ExecStart=/usr/local/bin/yaba-server` with `User=yaba`,
`WorkingDirectory=/var/lib/yaba` and `EnvironmentFile=/etc/yaba.env`. A deploy:

```bash
# On the server: update the source (or scp a tarball / prebuilt binaries over).
cd ~/YABA-2026 && git pull

GOOS=linux GOARCH=amd64 go build -o yaba-server .
GOOS=linux GOARCH=amd64 go build -o yaba ./cmd/yaba

# install(1) writes a new file and renames it into place. A plain
# `cp yaba-server /usr/local/bin/` over the running binary fails with
# "Text file busy"; if you have no install(1), cp to a temp name beside it and mv.
sudo install -m 0755 yaba-server /usr/local/bin/yaba-server
sudo install -m 0755 yaba        /usr/local/bin/yaba

sudo systemctl restart yaba
sudo journalctl -u yaba -n 50 --no-pager     # look for the startup lines, no "fatal:"
curl -fsS http://127.0.0.1:8000/healthz      # prints "ok"
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
`PrivateTmp`, `NoNewPrivileges`, `ReadWritePaths` limited to `/var/lib/yaba` and the
backup directory). Receipt reading needs outbound HTTPS to
`generativelanguage.googleapis.com` and `YABA_GEMINI_KEY` set in that file — nothing
else the app does needs network access.

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

Backups exist and run by default. The server writes a verified snapshot to
`YABA_BACKUP_DIR` every `YABA_BACKUP_EVERY` (default 24h), keeping `YABA_BACKUP_KEEP`
(default 14). The schedule follows the newest snapshot on disk, not process start: a
server restarted more often than the interval still backs up, about a minute after
start whenever the newest snapshot is overdue. A snapshot is also taken immediately
before any pending migration, and if that backup fails the migration is not attempted.

With `YABA_BACKUP_DIR` unset, snapshots go to a `backups` directory **beside the
database** (`/var/lib/yaba/backups` for `/var/lib/yaba/yaba.db`), and the server and the
CLI apply the same rule, so they always agree on where to look. Production sets
`YABA_BACKUP_DIR=/var/backups/yaba`; `-dir` / `-backup-dir` override either.

Versions before this one defaulted to the service account's cache directory
(`~yaba/.cache/YABA/backups`). If an install relied on that default, the server logs
a notice at startup naming both directories; move the old snapshots across, or set
`YABA_BACKUP_DIR` to the old path, to keep using them.

Each snapshot is two files: `yaba-<UTC stamp>.db`, and `yaba-<UTC stamp>-uploads.zip`
holding every receipt under `YABA_UPLOADS` (omitted when there are none). Both are
verified or written before the snapshot counts, and retention removes them together.
When no receipt has been added or removed since the previous backup, the new archive
is a hard link to the previous one: every snapshot still has its own archive to
restore from, but unchanged receipts take no extra disk space.

Snapshots are read through a separate read-only connection, so a backup no longer
holds up requests while it runs.

Each database snapshot is checked after being written: `integrity_check`,
`foreign_key_check`, a recorded schema version and the app's own invariants. It is
also compared with the previous snapshot: if users, households, memberships,
transactions or funds went from some rows to none, it looks like a truncated backup and
is refused. If that emptying was real -- the last household deliberately deleted --
the refusal clears itself once the same state has been seen on attempts at least an
hour apart (a failed backup is retried hourly), or at once with
`yaba backup -accept-empty`. Setting `YABA_BACKUP_ACCEPT_EMPTY=1` does the same for the
server; remove it afterwards, or the check stays off. Either way, the last snapshot
that still held those rows is kept as `pre-empty-yaba-<stamp>.db`, outside retention.

### Using the CLI on the server

The CLI reads `YABA_DB`, `YABA_UPLOADS`, `YABA_BACKUP_DIR` and `YABA_BACKUP_KEEP` as
its defaults, exactly as the server does, so give it the server's values and run it as
the service user (files it creates must belong to `yaba`):

```bash
yaba() {
  sudo -u yaba env $(sudo grep -E '^YABA_(DB|UPLOADS|BACKUP_DIR|BACKUP_KEEP)=' /etc/yaba.env) \
    /usr/local/bin/yaba "$@"
}
```

Only those four lines are passed because `env $(cat /etc/yaba.env)` splits on spaces,
and lines such as `YABA_SMTP_FROM=YABA <you@example.com>` or comments would break it.
Every subcommand except `restore` refuses a database path that does not exist, instead
of quietly creating an empty one in whatever directory you happen to be in.

```bash
# take one now (safe while the server runs), and list what you have
yaba backup
yaba backup -list

# check a snapshot without touching the live database
yaba restore -check                       # the newest
yaba restore -check -from /var/backups/yaba/yaba-20260917-060000Z.db

# put one back: the server must be stopped first
sudo systemctl stop yaba
yaba restore                              # the newest; or -from <snapshot>
sudo -u yaba unzip -o /var/backups/yaba/yaba-20260917-060000Z-uploads.zip -d /var/lib/yaba/uploads
sudo systemctl start yaba
```

`restore`, `reset` and `repair` refuse to run while the server holds the database: the
server keeps an OS-level lock on `yaba.db.lock` for as long as it runs (released
automatically if it dies), and they take the same lock. `backup` and `passwd` work
beside a running server, because SQLite's own locking makes them safe.

Restore never deletes the database it replaces. It first folds the write-ahead log into
it (`PRAGMA wal_checkpoint(TRUNCATE)`), then moves it aside as
`yaba.db.before-restore-<stamp>`; if the checkpoint is not possible, the `-wal` and
`-shm` are moved with it under the same name, so commits not yet checkpointed are kept
either way and SQLite still finds them if you open that file. A restore from the wrong
snapshot is therefore itself recoverable.

Receipts are not unpacked automatically, which is why the `unzip` step above is
separate: run it only if the receipts on disk are also lost or wrong.

One caveat worth being honest about: snapshots on the same disk as the database do not
protect against losing that disk. Copy `YABA_BACKUP_DIR` off the machine (an S3 sync
from cron, for example).

---

## Maintenance CLI

Kept out of the server binary so a destructive action is not one mistyped character
away from the thing you run every day.

```
yaba reset   [-db path] [-uploads dir] [-dir path] [-keep email] [-yes] [-backup=false]
yaba passwd  [-db path] [-email addr] [-list]
yaba backup  [-db path] [-dir path] [-uploads dir] [-keep n] [-list] [-accept-empty]
yaba restore [-from snapshot] [-dir path] [-db path] [-check]
yaba repair  [-db path] [-dir path] [-uploads dir] [-delete ids] [-yes]
```

```bash
go build -o yaba ./cmd/yaba
./yaba passwd -list -db /var/lib/yaba/yaba.db
./yaba passwd -email you@example.com -db /var/lib/yaba/yaba.db   # prompts twice, without echo
```

`passwd` has no `-password` flag, because a password on the command line is left in
the process list and the shell history. It prompts at a terminal without echoing, or
reads two lines (the password, then the confirmation) from piped input.

`reset` and `repair` show what they would change, ask for a typed confirmation, and
take a verified snapshot before changing anything; a failed snapshot stops them.

---

## Troubleshooting

**Everyone was signed out after a restart.** `YABA_SESSION_KEY` is not set, so a random
one was generated. The startup log says so.

**I cannot sign in locally.** `YABA_SECURE_COOKIE` is on (the default) and you are on
`http://`. The browser is refusing to send the cookie. Set `YABA_SECURE_COOKIE=0` for
local plain-HTTP testing only.

**The server will not start: invalid Gemini model.** `YABA_GEMINI_MODEL` holds a display
name or a typo. Use an API model id such as `gemini-3.1-flash-lite`, or unset it to get
the default.

**`yaba restore`: the database is in use by another process.** The server is running.
`sudo systemctl stop yaba`, restore, then start it.

**`yaba ...`: no database at yaba.db.** The CLI was run without the server's
environment, so it looked for `yaba.db` in the current directory. Pass `-db` or use the
`yaba()` wrapper in Backup and restore.

**`BACKUP FAILED: ... snapshot looks truncated`.** A guarded table went empty. If that
was deliberate, it is accepted automatically about an hour later, or at once with
`yaba backup -accept-empty`.

**`render: unknown page "x.html"`.** The template was added but not registered in the
`pages` list in `internal/web/web.go`.

**Receipts upload but are never read** ("Automatic reading isn't available on this
server"). `YABA_GEMINI_KEY` is not set, or Google rejected it. Put the key in `.env` and
restart the server; the startup log says `receipts: reading receipts with the Gemini API`
when it is picked up, and a `WARNING` when it is not. A rejected key is logged as
`Gemini rejected the request` with Google's own message.

**A reset email never arrives.** No SMTP host is configured. Check the startup log — it
says so explicitly, and says which variables to set. If one is configured and the log
says `does not offer STARTTLS; refusing to send unencrypted`, use port 465 or a server
that supports STARTTLS.

**I cannot reach the server from another machine.** It listens on `127.0.0.1:8000` by
default, so only the same machine (or a reverse proxy on it) can connect. Set
`YABA_ADDR=:8000` to listen on every interface.

**Rate limiting blocks everyone at once.** The app is behind a proxy that is not sending
`X-Forwarded-For`, so every request shares one apparent address.

**`database is locked`.** Something else has the file open — a second server, or a
`sqlite3` session. The pool is one connection by design; two processes are one too many.

**`gofmt -l .` lists files that look fine.** CRLF line endings. See Testing above.
