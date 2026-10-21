package core

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/smtp"
	"testing"
	"time"
)

// A mail app sends through our SMTP and uploads its own copy to Sent, in either order, with or
// without a Message-ID: Sent ends up with exactly one copy of each message.
func TestOneSentCopyPerMessage(t *testing.T) {
	app, _, _, b := newTestApp(t, true)
	tlsAddr, _ := startIMAPTest(t, app)
	send := func(msg string) {
		t.Helper()
		c, err := smtp.Dial(app.Cfg.SubmissionAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Hello("phone")
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
			t.Fatal(err)
		}
		if err := c.Auth(smtp.PlainAuth("", b.Email, "password-sajin", "127.0.0.1")); err != nil {
			t.Fatal(err)
		}
		if err := c.Mail(b.Email); err != nil {
			t.Fatal(err)
		}
		c.Rcpt("asi@skien.test")
		w, err := c.Data()
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, msg)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ic := imapDialT(t, tlsAddr, b.Email, "password-sajin")
	upload := func(msg string) {
		t.Helper()
		if err := ic.Append("Sent", []string{"\\Seen"}, time.Now(), bytes.NewBufferString(msg)); err != nil {
			t.Fatal(err)
		}
	}
	sentCount := func(subject string) int {
		var n int
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder='sent' AND subject=?`, b.ID, subject).Scan(&n)
		return n
	}
	hdr := "From: sajin@skien.test\r\nTo: asi@skien.test\r\nDate: Wed, 21 Oct 2026 20:01:00 +0530\r\n"
	cases := []struct {
		name, msg   string
		uploadFirst bool
	}{
		{"send then upload, with Message-ID", hdr + "Subject: A\r\nMessage-ID: <a1@phone>\r\n\r\nHello A\r\n", false},
		{"send then upload, no Message-ID", hdr + "Subject: B\r\n\r\nHello B\r\n", false},
		{"upload then send, with Message-ID", hdr + "Subject: C\r\nMessage-ID: <c1@phone>\r\n\r\nHello C\r\n", true},
		{"upload then send, no Message-ID", hdr + "Subject: D\r\n\r\nHello D\r\n", true},
	}
	for _, c := range cases {
		if c.uploadFirst {
			upload(c.msg)
			send(c.msg)
		} else {
			send(c.msg)
			upload(c.msg)
		}
		subj := c.msg[len(hdr)+9 : len(hdr)+10]
		if n := sentCount(subj); n != 1 {
			t.Errorf("%s: %d copies in Sent", c.name, n)
		}
	}
	// The upload-first copy without a Message-ID got the one our SMTP added, so replies thread.
	var mid string
	app.DB.QueryRow(`SELECT message_id FROM messages WHERE user_id=? AND folder='sent' AND subject='D'`, b.ID).Scan(&mid)
	if mid == "" {
		t.Error("upload-first copy has no Message-ID")
	}
	// Two different messages with the same subject are both kept.
	send(hdr + "Subject: Same\r\n\r\nFirst body\r\n")
	upload(hdr + "Subject: Same\r\n\r\nFirst body\r\n")
	send(hdr + "Subject: Same\r\n\r\nSecond body\r\n")
	upload(hdr + "Subject: Same\r\n\r\nSecond body\r\n")
	if n := sentCount("Same"); n != 2 {
		t.Errorf("same subject, different messages: %d copies", n)
	}
	// Recipients got each message once.
	var got int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages m JOIN users u ON u.id=m.user_id WHERE u.email='asi@skien.test' AND m.folder='inbox'`).Scan(&got)
	if got != 6 {
		t.Errorf("recipient has %d messages, want 6", got)
	}
}

func TestMessageBody(t *testing.T) {
	a := messageBody([]byte("Received: x\r\nMessage-ID: <1@x>\r\nSubject: s\r\n\r\nline1\r\nline2\r\n"))
	b := messageBody([]byte("Subject: s\n\nline1\nline2\n"))
	if !bytes.Equal(a, b) || string(a) != "line1\nline2" {
		t.Fatalf("%q vs %q", a, b)
	}
	if messageBody([]byte("no body here")) != nil {
		t.Fatal("headers only")
	}
}
