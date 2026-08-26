// Package mail sends the two emails this application has to send: an invitation
// to a shared budget, and a password reset link.
//
// Built on net/smtp from the standard library, so it adds no dependency. That is
// the whole reason for choosing SMTP over a provider's HTTP API: it works with a
// Gmail app password, with Resend, with Postmark, with a company relay, and with
// a local test server, and the code does not change between them.
//
// Two decisions shape everything here.
//
// The first: with no credentials configured, mail is written to the log instead
// of failing. A development machine has no mail relay, and an application that
// refuses to invite anybody until SMTP is set up would be unusable locally. The
// log line contains the whole message, including the reset link, so the flow can
// be walked end to end without a mail server. It also says clearly that nothing
// was sent, because a silent no-op here would be indistinguishable from working.
//
// The second: links must be absolute. A reset link is clicked from an email
// client, where a relative path means nothing, so the base URL has to be
// configured rather than guessed from the request -- guessing from the Host
// header would let anyone who can reach the server mint a reset link pointing at
// a host of their choosing.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config is the SMTP configuration, all of it optional.
type Config struct {
	Host string // e.g. smtp.gmail.com
	Port int    // 587 for STARTTLS, 465 for implicit TLS
	User string
	Pass string
	From string // e.g. "YABA <you@example.com>"

	// BaseURL is the externally reachable root, used to build links.
	BaseURL string
}

// Message is one outgoing email, in plain text.
//
// Plain text only, deliberately. An HTML email needs a text alternative anyway,
// doubles the templating, and is the part of an email most likely to be mangled
// or flagged as spam. Nothing being sent here benefits from formatting.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends messages, or logs them when it cannot send.
type Mailer struct {
	cfg     Config
	enabled bool
}

// New builds a Mailer. It never fails: missing configuration means log-only.
func New(cfg Config) *Mailer {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:8000"
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" && cfg.User != "" {
		cfg.From = cfg.User
	}

	m := &Mailer{cfg: cfg}
	m.enabled = cfg.Host != "" && cfg.From != ""
	if !m.enabled {
		log.Printf("mail: no SMTP host configured — invitations and reset links " +
			"will be written to this log instead of being sent")
		log.Printf("mail: set YABA_SMTP_HOST, YABA_SMTP_USER, YABA_SMTP_PASS and " +
			"YABA_SMTP_FROM to send them")
	}
	return m
}

// Enabled reports whether mail actually leaves the machine. Handlers use it to
// tell the truth on screen rather than claiming an email was sent.
func (m *Mailer) Enabled() bool { return m.enabled }

// BaseURL is the root used for links, exposed so callers can show it.
func (m *Mailer) BaseURL() string { return m.cfg.BaseURL }

// Send delivers one message, or logs it.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	msg.To = strings.TrimSpace(msg.To)
	if msg.To == "" {
		return fmt.Errorf("mail: no recipient")
	}
	// Header injection: a newline in an address or subject would let the rest of
	// the header block be rewritten, adding recipients or replacing the sender.
	// Rejected rather than stripped, because a legitimate address never contains
	// one and quietly altering it would hide the attempt.
	for _, field := range []string{msg.To, msg.Subject, m.cfg.From} {
		if strings.ContainsAny(field, "\r\n") {
			return fmt.Errorf("mail: header field contains a line break")
		}
	}

	if !m.enabled {
		log.Printf("mail: NOT SENT (no SMTP configured)\n"+
			"  to:      %s\n  subject: %s\n%s",
			msg.To, msg.Subject, indent(msg.Body))
		return nil
	}

	raw := m.compose(msg)
	addr := net.JoinHostPort(m.cfg.Host, fmt.Sprint(m.cfg.Port))

	// A dial that hangs would hold a request open for as long as the network
	// allows, so everything below is bounded.
	deadline := 20 * time.Second
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < deadline {
			deadline = remaining
		}
	}

	done := make(chan error, 1)
	go func() { done <- m.deliver(addr, msg.To, raw, deadline) }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mail: send to %s: %w", msg.To, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(deadline):
		return fmt.Errorf("mail: sending to %s timed out after %s", msg.To, deadline)
	}
}

// deliver does the SMTP conversation.
//
// Port 465 is implicit TLS -- the connection is encrypted before any SMTP is
// spoken -- while 587 negotiates STARTTLS mid-conversation. smtp.SendMail
// handles the second and cannot do the first, so both are covered explicitly.
func (m *Mailer) deliver(addr, to string, raw []byte, timeout time.Duration) error {
	auth := smtp.PlainAuth("", m.cfg.User, m.cfg.Pass, m.cfg.Host)
	if m.cfg.User == "" {
		auth = nil // an unauthenticated relay
	}

	if m.cfg.Port != 465 {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		return m.converse(conn, auth, to, raw)
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr,
		&tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	return m.converse(conn, auth, to, raw)
}

func (m *Mailer) converse(conn net.Conn, auth smtp.Auth, to string, raw []byte) error {
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	// On 587 the connection starts in the clear, so upgrade before authenticating
	// -- otherwise the password crosses the network in plaintext.
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{
			ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12,
		}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(envelopeAddress(m.cfg.From)); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// compose builds the RFC 5322 message.
func (m *Mailer) compose(msg Message) []byte {
	var b strings.Builder
	h := func(k, v string) {
		// CRLF, not LF: some servers reject bare newlines in the header block.
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	h("From", m.cfg.From)
	h("To", msg.To)
	h("Subject", msg.Subject)
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", messageID(m.cfg.Host))
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="utf-8"`)
	// An automated message should not trigger out-of-office replies, and should
	// not be filed as a conversation the recipient is expected to answer.
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(normaliseNewlines(msg.Body), "\n", "\r\n"))
	return []byte(b.String())
}

// envelopeAddress strips a display name, since MAIL FROM takes a bare address.
func envelopeAddress(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		if j := strings.Index(from[i:], ">"); j > 0 {
			return from[i+1 : i+j]
		}
	}
	return strings.TrimSpace(from)
}

func messageID(host string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), host)
	}
	return fmt.Sprintf("<%s@%s>", base64.RawURLEncoding.EncodeToString(buf), host)
}

func normaliseNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(normaliseNewlines(s), "\n") {
		b.WriteString("  | ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ── the two messages ──────────────────────────────────────────────────────────

// Invitation tells someone they have been added to a shared budget.
//
// It does not carry an accept link with a token in it. The invitation is keyed to
// the recipient's email address and is accepted after signing in, so a link that
// granted access on click would be a bearer credential sitting in an inbox --
// forwardable, and readable by anyone who ever sees that mailbox. Signing in
// first means the person who joins is the person who owns the address.
func (m *Mailer) Invitation(ctx context.Context, to, household, invitedBy, role string,
	expires time.Duration) error {

	who := invitedBy
	if strings.TrimSpace(who) == "" {
		who = "Someone"
	}
	body := fmt.Sprintf(`%s has invited you to share a budget on YABA.

  Budget:     %s
  Your role:  %s

Sign in with this address to accept:

  %s

This invitation expires in %s. After that, ask them to send it again.

If you weren't expecting this, you can ignore it — nothing has been shared with
you unless you accept, and your own budget stays private either way.
`, who, household, roleSentence(role), m.cfg.BaseURL, humanDuration(expires))

	return m.Send(ctx, Message{
		To:      to,
		Subject: fmt.Sprintf("%s invited you to the %q budget on YABA", who, household),
		Body:    body,
	})
}

// PasswordReset sends a single-use link.
func (m *Mailer) PasswordReset(ctx context.Context, to, token string, ttl time.Duration) error {
	body := fmt.Sprintf(`Someone asked to reset the password for your YABA account.

Open this link to choose a new one:

  %s/reset?token=%s

The link works once and expires in %s.

If it wasn't you, you can ignore this email — your password has not changed, and
whoever asked cannot see it. Nobody can use this link without this email.
`, m.cfg.BaseURL, token, humanDuration(ttl))

	return m.Send(ctx, Message{
		To:      to,
		Subject: "Reset your YABA password",
		Body:    body,
	})
}

func roleSentence(role string) string {
	switch role {
	case "editor":
		return "Editor — can add and change entries, but not move savings"
	case "viewer":
		return "Viewer — can see everything, changes nothing"
	default:
		return role
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 24*time.Hour:
		return "24 hours"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= time.Hour:
		return "1 hour"
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}
