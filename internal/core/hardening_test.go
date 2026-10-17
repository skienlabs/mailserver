package core

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func newTestApp(t *testing.T, withSMTP bool) (*App, *Domain, *User, *User) {
	t.Helper()
	cfg := &Config{DataDir: t.TempDir(), SMTPAddr: "off", SubmissionAddr: "off", SubmissionsAddr: "off", HTTPAddr: "off", Hostname: "mail.skien.test", MaxMessageMB: 25}
	if withSMTP {
		cfg.SMTPAddr, cfg.SubmissionAddr = freePort(t), freePort(t)
	}
	app, err := NewApp(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SM_TEST_ONECONN") == "1" {
		app.DB.SetMaxOpenConns(1)
	}
	if withSMTP {
		if err := app.startSMTP(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Shutdown)
	}
	d, _ := app.CreateDomain("skien.test")
	a, err := app.CreateUser(NewUser{Name: "Asi", Local: "asi", DomainID: d.ID, Password: "password-asi", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := app.CreateUser(NewUser{Name: "Sajin", Local: "sajin", DomainID: d.ID, Password: "password-sajin"})
	return app, d, a, b
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".eml") {
			n++
		}
		return nil
	})
	return n
}

// Finding 1: deeply nested multipart must be cheap to handle and refused at SMTP.
func TestMIMEBombIsCheapAndRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: x@example.test\r\nSubject: bomb\r\n")
	depth := 5000
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nhi\r\n")
	raw := []byte(b.String())
	if !MIMETooComplex(raw) {
		t.Fatal("bomb not detected")
	}
	start := time.Now()
	ParseMeta(raw)
	body := ParseBody(raw)
	FindAttachment(raw, 0, "")
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("parsing a MIME bomb took %v", el)
	}
	if !strings.Contains(body.Text, "complex structure") {
		t.Fatalf("expected notice, got %q", body.Text)
	}
	// Normal mail with a few levels is not affected.
	normal := []byte("From: x@example.test\r\nContent-Type: multipart/mixed; boundary=a\r\n\r\n--a\r\nContent-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n--a--\r\n")
	if MIMETooComplex(normal) || !strings.Contains(ParseBody(normal).Text, "hello") {
		t.Fatal("normal message misclassified")
	}

	app, _, _, _ := newTestApp(t, true)
	err := sendSMTP(app.Cfg.SMTPAddr, "x@example.test", []string{"asi@skien.test"}, raw)
	if err == nil || !strings.Contains(err.Error(), "552") {
		t.Fatalf("expected 552 for MIME bomb, got %v", err)
	}
}

// Finding 2: a repeated Message-ID must not leave orphan files or skew quota.
func TestDuplicateDeliveryLeavesNoFiles(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	raw := []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: dup\r\nMessage-ID: <same@x>\r\n\r\nbody\r\n")
	for i := 0; i < 5; i++ {
		if _, err := app.Deliver(a.ID, raw, DeliverOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countFiles(t, filepath.Join(app.Cfg.DataDir, "mail")); n != 1 {
		t.Fatalf("expected 1 stored file, got %d", n)
	}
	u, _ := app.UserByID(a.ID)
	if u.UsedBytes != int64(len(raw)) {
		t.Fatalf("used_bytes %d, want %d", u.UsedBytes, len(raw))
	}
}

// Finding 3: Trash keeps the full retention window even for old mail.
func TestTrashRetentionCountsFromDeletion(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	raw := []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: old\r\nMessage-ID: <old@x>\r\n\r\nbody\r\n")
	id, _ := app.Deliver(a.ID, raw, DeliverOpts{})
	old := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	app.DB.Exec(`UPDATE messages SET received_at=?, date=? WHERE id=?`, old, old, id)
	var tid int64
	app.DB.QueryRow(`SELECT thread_id FROM messages WHERE id=?`, id).Scan(&tid)
	if err := app.ThreadAction(a.ID, []int64{tid}, "inbox", "trash"); err != nil {
		t.Fatal(err)
	}
	app.cleanup()
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, id).Scan(&n)
	if n != 1 {
		t.Fatal("a message trashed just now was purged because it was received long ago")
	}
	// After the retention window it is purged.
	app.DB.Exec(`UPDATE messages SET trashed_at=? WHERE id=?`, time.Now().Add(-31*24*time.Hour).UnixMilli(), id)
	app.cleanup()
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, id).Scan(&n)
	if n != 0 {
		t.Fatal("expired trash not purged")
	}
}

// Finding 4: wildcard Message-IDs or unrelated subjects can't join a conversation.
func TestThreadInjectionBlocked(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	legit := []byte("From: vendor@partner.test\r\nTo: asi@skien.test\r\nSubject: Invoice 42\r\nMessage-ID: <inv42@partner.test>\r\nReferences: <a@partner.test> <b@partner.test>\r\n\r\nInvoice attached\r\n")
	id1, _ := app.Deliver(a.ID, legit, DeliverOpts{})
	var t1 int64
	app.DB.QueryRow(`SELECT thread_id FROM messages WHERE id=?`, id1).Scan(&t1)

	attacks := []string{
		"From: evil@bad.test\r\nTo: asi@skien.test\r\nSubject: Updated bank details\r\nMessage-ID: <%>\r\n\r\npay here\r\n",
		"From: evil@bad.test\r\nTo: asi@skien.test\r\nSubject: Updated bank details\r\nMessage-ID: <%@partner.test>\r\n\r\npay here\r\n",
		"From: evil@bad.test\r\nTo: asi@skien.test\r\nSubject: Updated bank details\r\nMessage-ID: <x@bad.test>\r\nIn-Reply-To: <inv42@partner.test>\r\n\r\npay here\r\n",
	}
	for i, raw := range attacks {
		id, _ := app.Deliver(a.ID, []byte(raw), DeliverOpts{})
		var tid int64
		app.DB.QueryRow(`SELECT thread_id FROM messages WHERE id=?`, id).Scan(&tid)
		if tid == t1 {
			t.Fatalf("attack %d joined the invoice thread", i)
		}
	}
	// A genuine reply still threads.
	reply := []byte("From: asi@skien.test\r\nTo: vendor@partner.test\r\nSubject: RE: Invoice 42\r\nMessage-ID: <r1@skien.test>\r\nIn-Reply-To: <inv42@partner.test>\r\n\r\nthanks\r\n")
	id2, _ := app.Deliver(a.ID, reply, DeliverOpts{Folder: "sent"})
	var t2 int64
	app.DB.QueryRow(`SELECT thread_id FROM messages WHERE id=?`, id2).Scan(&t2)
	if t2 != t1 {
		t.Fatal("genuine reply did not thread")
	}
}

// Finding 5: submission rejects multiple authors and colleagues in From/Sender.
func TestSubmissionFromSpoofing(t *testing.T) {
	app, _, _, _ := newTestApp(t, true)
	try := func(msg string) error {
		c, err := smtp.Dial(app.Cfg.SubmissionAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Hello("laptop")
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
			t.Fatal(err)
		}
		if err := c.Auth(smtp.PlainAuth("", "sajin@skien.test", "password-sajin", "127.0.0.1")); err != nil {
			t.Fatal(err)
		}
		if err := c.Mail("sajin@skien.test"); err != nil {
			return err
		}
		c.Rcpt("asi@skien.test")
		w, err := c.Data()
		if err != nil {
			return err
		}
		io.WriteString(w, msg)
		return w.Close()
	}
	bad := []string{
		"From: sajin@skien.test, asi@skien.test\r\nTo: asi@skien.test\r\nSubject: x\r\n\r\nx\r\n",
		"From: sajin@skien.test\r\nFrom: asi@skien.test\r\nTo: asi@skien.test\r\nSubject: x\r\n\r\nx\r\n",
		"From: asi@skien.test\r\nTo: asi@skien.test\r\nSubject: x\r\n\r\nx\r\n",
		"From: sajin@skien.test\r\nSender: asi@skien.test\r\nTo: asi@skien.test\r\nSubject: x\r\n\r\nx\r\n",
	}
	for i, m := range bad {
		if err := try(m); err == nil {
			t.Fatalf("spoof %d accepted", i)
		}
	}
	if err := try("From: Sajin <sajin@skien.test>\r\nTo: asi@skien.test\r\nSubject: ok\r\n\r\nfine\r\n"); err != nil {
		t.Fatalf("legit submission refused: %v", err)
	}
}

// Finding 6: failed logins are limited per account across protocols, and IPv6 by /64.
func TestAuthLimits(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	for i := 0; i < authLimitAccountIP; i++ {
		if !app.authBegin("2001:db8:1:2::10", "asi@skien.test") {
			t.Fatalf("attempt %d blocked too early", i)
		}
	}
	// Another address in the same /64 is the same client.
	if app.authBegin("2001:db8:1:2::99", "asi@skien.test") {
		t.Fatal("IPv6 /64 rotation bypassed the limit")
	}
	// The real user on another network can still sign in.
	if !app.authBegin("203.0.113.5", "asi@skien.test") {
		t.Fatal("account locked for everyone by a single attacker")
	}
	// Successful logins are refunded and never lock anyone out.
	for i := 0; i < 50; i++ {
		if !app.authBegin("198.51.100.7", "sajin@skien.test") {
			t.Fatalf("successful login %d was limited", i)
		}
		app.authSucceeded("198.51.100.7", "sajin@skien.test")
	}
	if cap(hashSlots) < 2 {
		t.Fatal("argon2 concurrency cap missing")
	}
}

// Finding 7: a full mailbox is refused during RCPT (no backscatter bounce).
func TestFullMailboxRefusedAtRcpt(t *testing.T) {
	app, _, a, _ := newTestApp(t, true)
	app.DB.Exec(`UPDATE users SET quota_bytes=100, used_bytes=100 WHERE id=?`, a.ID)
	c, err := smtp.Dial(app.Cfg.SMTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Hello("mx.example.test")
	c.Mail("someone@example.test")
	err = c.Rcpt("asi@skien.test")
	if err == nil || !strings.Contains(err.Error(), "452") {
		t.Fatalf("expected 452 at RCPT, got %v", err)
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM queue`).Scan(&n)
	if n != 0 {
		t.Fatal("a bounce was queued")
	}
}

// Finding 9: out-of-office answers only mail addressed to the user personally.
func TestVacationOnlyWhenAddressed(t *testing.T) {
	m := &Meta{To: []Addr{{Email: "team@skien.test"}}}
	if personallyAddressed("asi@skien.test", m) {
		t.Fatal("group mail treated as personal")
	}
	m.Cc = []Addr{{Email: "Asi+work@skien.test"}}
	if !personallyAddressed("asi@skien.test", m) {
		t.Fatal("plus-address in Cc not recognised")
	}
}

// Finding 10: truncation never splits a character.
func TestTruncationIsRuneSafe(t *testing.T) {
	s := strings.Repeat("ഇ", 600) // Malayalam, 3 bytes per character
	out := sanitizeHeaderValue(s)
	if !utf8.ValidString(out) || utf8.RuneCountInString(out) != 500 {
		t.Fatalf("bad truncation: valid=%v runes=%d", utf8.ValidString(out), utf8.RuneCountInString(out))
	}
	if got := truncate("héllo wörld", 4); got != "héll" {
		t.Fatalf("truncate = %q", got)
	}
}
