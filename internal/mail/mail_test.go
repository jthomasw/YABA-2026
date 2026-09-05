// Tests for the mail package.
//
// The package had none, which is uncomfortable for code that builds a header
// block out of user-supplied strings and hands a password to a socket. The two
// things worth proving are that a header cannot be rewritten by a crafted
// address, and that the password is never sent over a connection that offered
// to encrypt itself.
//
// The SMTP conversation is exercised against a fake server on a local socket
// rather than mocked out, because the interesting bugs live in the order of the
// commands rather than in the code that chooses them.
package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── the fake relay ────────────────────────────────────────────────────────────

// fakeSMTP is a minimal server that records one conversation.
type fakeSMTP struct {
	host string
	port int

	// advertise is appended to the EHLO response, so a test can offer STARTTLS.
	advertise []string

	mu       sync.Mutex
	commands []string
	data     string
	done     chan struct{}
}

func newFakeSMTP(t *testing.T, advertise ...string) *fakeSMTP {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	f := &fakeSMTP{host: "127.0.0.1", port: addr.Port, advertise: advertise, done: make(chan struct{})}

	go func() {
		defer close(f.done)
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		f.serve(conn)
	}()
	t.Cleanup(func() {
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("the fake relay never finished its conversation")
		}
	})
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	say := func(format string, a ...any) {
		fmt.Fprintf(conn, format+"\r\n", a...)
	}

	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)

		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			// The first line of an EHLO reply is the greeting, not an extension:
			// a client that follows RFC 5321 discards it, so advertising
			// STARTTLS there would silently hide it.
			say("250-fake greets you")
			for i, ext := range f.advertise {
				sep := "-"
				if i == len(f.advertise)-1 {
					sep = " "
				}
				say("250%s%s", sep, ext)
			}
			if len(f.advertise) == 0 {
				say("250 HELP")
			}
		case "STARTTLS":
			// Agree, then hang up rather than completing a handshake, so the
			// upgrade fails. A client that carries on in the clear is the bug.
			say("220 go ahead")
			return
		case "MAIL", "RCPT", "RSET", "NOOP":
			say("250 ok")
		case "DATA":
			say("354 send it")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				body.WriteString(l)
			}
			f.mu.Lock()
			f.data = body.String()
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("500 what")
		}
	}
}

func (f *fakeSMTP) record(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, line)
}

func (f *fakeSMTP) transcript() ([]string, string) {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), f.data
}

func (f *fakeSMTP) config() Config {
	return Config{Host: f.host, Port: f.port, From: "YABA <yaba@example.test>",
		BaseURL: "https://budget.example.test"}
}

// ── what a real send looks like on the wire ───────────────────────────────────

// TestSendSpeaksSMTPAndDeliversTheComposedMessage: the envelope carries the bare
// address while the header keeps the display name, and the message that arrives
// is the one compose built.
func TestSendSpeaksSMTPAndDeliversTheComposedMessage(t *testing.T) {
	f := newFakeSMTP(t)
	m := New(f.config())
	if !m.Enabled() {
		t.Fatal("a mailer with a host and a sender should be enabled")
	}

	err := m.Send(context.Background(), Message{
		To:      "someone@example.test",
		Subject: "Reset your YABA password",
		Body:    "line one\nline two\n",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	commands, data := f.transcript()
	joined := strings.Join(commands, "\n")

	// MAIL FROM takes a bare address; a display name here is rejected by real
	// relays, which is the sort of failure that only shows up in production.
	if !strings.Contains(joined, "MAIL FROM:<yaba@example.test>") {
		t.Errorf("envelope sender wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "RCPT TO:<someone@example.test>") {
		t.Errorf("envelope recipient wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "QUIT") {
		t.Errorf("the client did not close the conversation:\n%s", joined)
	}

	// The display name survives in the header even though the envelope dropped it.
	for _, want := range []string{
		"From: YABA <yaba@example.test>",
		"To: someone@example.test",
		"Subject: Reset your YABA password",
		"Auto-Submitted: auto-generated",
		`Content-Type: text/plain; charset="utf-8"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("missing header %q in:\n%s", want, data)
		}
	}
	if !strings.Contains(data, "\r\n\r\n") {
		t.Error("no blank line between headers and body, so the body would be read as headers")
	}
	// Bare newlines in the body would be a protocol violation on a strict relay.
	if strings.Contains(strings.ReplaceAll(data, "\r\n", ""), "\n") {
		t.Errorf("the body still contains bare newlines:\n%q", data)
	}
	if !strings.Contains(data, "line one\r\nline two") {
		t.Errorf("body missing from:\n%s", data)
	}
}

// TestStartTLSIsNotOptional: on port 587 the conversation begins in the clear,
// so the credentials must not be sent unless the upgrade succeeds. A server
// that offers STARTTLS and then fails the handshake must abort the send rather
// than continue in plaintext.
func TestStartTLSIsNotOptional(t *testing.T) {
	f := newFakeSMTP(t, "STARTTLS", "AUTH PLAIN")
	cfg := f.config()
	cfg.User = "yaba@example.test"
	cfg.Pass = "hunter2"
	m := New(cfg)

	err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "hello", Body: "hi",
	})
	if err == nil {
		t.Fatal("the send succeeded even though the connection was never encrypted")
	}

	commands, data := f.transcript()
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "STARTTLS") {
		t.Errorf("the client never tried to upgrade:\n%s", joined)
	}
	if strings.Contains(joined, "AUTH") {
		t.Errorf("credentials were offered before the connection was encrypted:\n%s", joined)
	}
	if strings.Contains(joined, "hunter2") || strings.Contains(data, "hunter2") {
		t.Error("the password crossed the wire in the clear")
	}
	if strings.Contains(joined, "DATA") {
		t.Errorf("the message was sent anyway:\n%s", joined)
	}
}

// TestSendGivesUpOnASilentServer: a relay that accepts the connection and then
// says nothing must not hold the request open indefinitely.
func TestSendGivesUpOnASilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(5 * time.Second) // never greets
	}()

	addr := ln.Addr().(*net.TCPAddr)
	m := New(Config{Host: "127.0.0.1", Port: addr.Port, From: "yaba@example.test"})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := m.Send(ctx, Message{To: "x@example.test", Subject: "s", Body: "b"}); err == nil {
		t.Error("a silent relay was reported as a successful send")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Send took %v; a hung relay would hold a request open", elapsed)
	}
}

// ── header injection ──────────────────────────────────────────────────────────

// TestHeaderInjectionIsRefused. The recipient is a value the user typed, and a
// newline in it would end the To header and let the rest of the block be
// written by hand -- a Bcc to somewhere else, a different Subject, or an early
// blank line that turns the intended headers into body text. Rejected rather
// than stripped, so the attempt is visible instead of quietly half-honoured.
func TestHeaderInjectionIsRefused(t *testing.T) {
	m := New(Config{Host: "smtp.example.test", From: "YABA <yaba@example.test>"})

	cases := []struct {
		name string
		msg  Message
	}{
		{"bcc smuggled into the recipient", Message{
			To:      "someone@example.test\r\nBcc: attacker@evil.test",
			Subject: "hello", Body: "hi"}},
		{"bare newline in the recipient", Message{
			To: "someone@example.test\nBcc: attacker@evil.test", Subject: "hello", Body: "hi"}},
		{"a rewritten subject", Message{
			To:      "someone@example.test",
			Subject: "hello\r\nFrom: bank@example.test", Body: "hi"}},
		{"an early end to the header block", Message{
			To: "someone@example.test", Subject: "hello\r\n\r\nFake body", Body: "hi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.Send(context.Background(), c.msg)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "line break") {
				t.Errorf("error %q does not name the reason", err)
			}
		})
	}

	// An empty recipient is refused too, rather than dialling out for nothing.
	if err := m.Send(context.Background(), Message{To: "   ", Subject: "s"}); err == nil {
		t.Error("an empty recipient was accepted")
	}
}

// ── log-only mode ─────────────────────────────────────────────────────────────

// TestUnconfiguredMailerSucceedsWithoutSending: signing up, inviting someone and
// resetting a password all have to work on a box with no SMTP, so Send reports
// success and writes the message to the log instead.
func TestUnconfiguredMailerSucceedsWithoutSending(t *testing.T) {
	m := New(Config{})
	if m.Enabled() {
		t.Error("a mailer with no host should not claim it can send")
	}
	if err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "hello", Body: "hi"}); err != nil {
		t.Errorf("send with no relay configured: %v", err)
	}
	// The check on the header fields still applies, so a test with a relay
	// configured is not the only thing standing between a crafted address and a
	// rewritten header.
	if err := m.Send(context.Background(), Message{
		To: "a@b.test\r\nBcc: c@d.test", Subject: "hello"}); err == nil {
		t.Error("header injection was accepted when no relay was configured")
	}
}

// ── configuration defaults ────────────────────────────────────────────────────

func TestNewFillsInTheDefaults(t *testing.T) {
	m := New(Config{User: "yaba@example.test", Host: "smtp.example.test",
		BaseURL: "  https://budget.example.test/  "})

	// A trailing slash would produce "https://host//reset?token=..." in every link.
	if got := m.BaseURL(); got != "https://budget.example.test" {
		t.Errorf("BaseURL = %q", got)
	}
	if m.cfg.Port != 587 {
		t.Errorf("port = %d, want the STARTTLS default of 587", m.cfg.Port)
	}
	// A configuration with a user and no explicit sender is complete enough to send.
	if m.cfg.From != "yaba@example.test" || !m.Enabled() {
		t.Errorf("From = %q, enabled = %v; the user should stand in as the sender",
			m.cfg.From, m.Enabled())
	}

	if got := New(Config{}).BaseURL(); got != "http://localhost:8000" {
		t.Errorf("default BaseURL = %q", got)
	}
}

func TestEnvelopeAddressStripsTheDisplayName(t *testing.T) {
	cases := map[string]string{
		"YABA <yaba@example.test>":     "yaba@example.test",
		"  yaba@example.test  ":        "yaba@example.test",
		"<yaba@example.test>":          "yaba@example.test",
		`"Budget, YABA" <y@e.test>`:    "y@e.test",
		"yaba@example.test":            "yaba@example.test",
		"Name <a@b.test> <trailing@c>": "trailing@c",
	}
	for in, want := range cases {
		if got := envelopeAddress(in); got != want {
			t.Errorf("envelopeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── the two messages ──────────────────────────────────────────────────────────

// TestTheTwoMessagesCarryWhatTheyPromise: both are read by somebody deciding
// whether to trust them, so each has to name what is being offered, how long it
// lasts, and what happens if they ignore it.
func TestTheTwoMessagesCarryWhatTheyPromise(t *testing.T) {
	f := newFakeSMTP(t)
	m := New(f.config())

	if err := m.PasswordReset(context.Background(), "someone@example.test",
		"tok3n", 30*time.Minute); err != nil {
		t.Fatalf("password reset: %v", err)
	}
	_, data := f.transcript()

	if !strings.Contains(data, "https://budget.example.test/reset?token=tok3n") {
		t.Errorf("the reset link is wrong or missing:\n%s", data)
	}
	if !strings.Contains(data, "30 minutes") {
		t.Errorf("the message does not say how long the link lasts:\n%s", data)
	}
	if !strings.Contains(data, "has not changed") {
		t.Errorf("the message does not reassure a recipient who did not ask:\n%s", data)
	}
}

func TestInvitationNamesTheBudgetAndTheRole(t *testing.T) {
	f := newFakeSMTP(t)
	m := New(f.config())

	if err := m.Invitation(context.Background(), "someone@example.test",
		"Household", "", "viewer", 48*time.Hour); err != nil {
		t.Fatalf("invitation: %v", err)
	}
	_, data := f.transcript()

	// An invitation from an unnamed inviter must still read as a sentence.
	if !strings.Contains(data, "Someone has invited you") {
		t.Errorf("an anonymous inviter reads badly:\n%s", data)
	}
	if !strings.Contains(data, "Household") {
		t.Errorf("the budget is not named:\n%s", data)
	}
	// The role decides whether accepting is a risk, so it is spelled out rather
	// than left as a word the recipient has to guess the meaning of.
	if !strings.Contains(data, "can see everything, changes nothing") {
		t.Errorf("the role is not explained:\n%s", data)
	}
	if !strings.Contains(data, "2 days") {
		t.Errorf("the expiry is not stated:\n%s", data)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Minute, "30 minutes"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "1 hour"},
		{2 * time.Hour, "2 hours"},
		{24 * time.Hour, "24 hours"},
		{47 * time.Hour, "24 hours"},
		{48 * time.Hour, "2 days"},
		{7 * 24 * time.Hour, "7 days"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
