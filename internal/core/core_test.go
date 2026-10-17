package core

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/smtp"
	"reflect"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type client struct {
	t   *testing.T
	c   *http.Client
	url string
}

func newClient(t *testing.T, url string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, c: &http.Client{Jar: jar}, url: url}
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	req.Header.Set("X-SM", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		resetValue(out)
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 400 {
		c.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// sendSMTP delivers like a remote MTA would (STARTTLS without certificate checks).
func sendSMTP(addr, from string, rcpts []string, msg []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("mx.example.org"); err != nil {
		return err
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	w.Write(msg)
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// resetValue zeroes a decode target so values from earlier responses don't leak through.
func resetValue(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv.Elem().Set(reflect.Zero(rv.Elem().Type()))
	}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type threadList struct {
	Items []ThreadRow `json:"items"`
	Total int         `json:"total"`
}

func TestEndToEnd(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), SMTPAddr: freePort(t), SubmissionAddr: freePort(t), SubmissionsAddr: "off",
		HTTPAddr: "off", Hostname: "mail.skienlab.test", MaxMessageMB: 25, TrustProxy: true}
	app, err := NewApp(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown()
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	admin := newClient(t, srv.URL)
	var st map[string]any
	admin.do("GET", "/api/setup", nil, &st)
	if st["needs_setup"] != true {
		t.Fatal("expected setup needed")
	}
	// CSRF header enforced
	resp, _ := http.Post(srv.URL+"/api/setup", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 403 {
		t.Fatalf("missing X-SM header should be rejected, got %d", resp.StatusCode)
	}
	if code := admin.do("POST", "/api/setup", map[string]any{"token": "WRONG", "org_name": "Skienlab", "domain": "skien.test",
		"hostname": "mail.skienlab.test", "name": "Asi", "local": "asi", "password": "supersecret1"}, nil); code != 403 {
		t.Fatalf("wrong token accepted: %d", code)
	}
	if code := admin.do("POST", "/api/setup", map[string]any{"token": app.setupToken, "org_name": "Skienlab", "domain": "skien.test",
		"hostname": "mail.skienlab.test", "name": "Asi", "local": "asi", "password": "supersecret1"}, nil); code != 200 {
		t.Fatalf("setup failed: %d", code)
	}
	var me map[string]any
	if code := admin.do("GET", "/api/me", nil, &me); code != 200 {
		t.Fatal("me failed")
	}
	domains := me["org"].(map[string]any)["domains"].([]any)
	domID := int64(domains[0].(map[string]any)["id"].(float64))

	// Create an employee with a generated password
	var created struct {
		User     User   `json:"user"`
		Password string `json:"password"`
	}
	if code := admin.do("POST", "/api/admin/users", map[string]any{"name": "Sajin", "local": "sajin", "domain_id": domID}, &created); code != 200 {
		t.Fatal("create user failed")
	}
	if created.Password == "" || created.User.Email != "sajin@skien.test" {
		t.Fatalf("unexpected create response %+v", created)
	}
	// Duplicate address
	if code := admin.do("POST", "/api/admin/users", map[string]any{"name": "X", "local": "sajin", "domain_id": domID}, nil); code != 400 {
		t.Fatal("duplicate user accepted")
	}
	// Group
	if code := admin.do("POST", "/api/admin/aliases", map[string]any{"local": "team", "domain_id": domID, "name": "Team",
		"members": []map[string]any{{"user_id": 1, "can_send": true}, {"user_id": created.User.ID, "can_send": false}}}, nil); code != 200 {
		t.Fatal("create group failed")
	}

	sajin := newClient(t, srv.URL)
	if code := sajin.do("POST", "/api/auth/login", map[string]any{"email": "sajin", "password": "bad"}, nil); code != 401 {
		t.Fatal("bad password accepted")
	}
	if code := sajin.do("POST", "/api/auth/login", map[string]any{"email": "sajin", "password": created.Password}, nil); code != 200 {
		t.Fatal("login failed")
	}
	if code := sajin.do("GET", "/api/threads?folder=inbox", nil, nil); code != 403 {
		t.Fatal("password change should be required first")
	}
	if code := sajin.do("POST", "/api/profile/password", map[string]any{"current": created.Password, "new": "anothersecret9"}, nil); code != 200 {
		t.Fatal("password change failed")
	}

	// Admin sends to Sajin, the team group and an external domain that doesn't exist
	if code := admin.do("POST", "/api/send", map[string]any{
		"to": []Addr{{Name: "Sajin", Email: "sajin@skien.test"}, {Email: "team@skien.test"}}, "cc": []Addr{{Email: "someone@nonexistent-domain.invalid"}},
		"subject": "Welcome aboard", "html": "<p>Hello <b>Sajin</b><script>alert(1)</script></p>"}, nil); code != 200 {
		t.Fatal("send failed")
	}
	// Unknown local recipient is refused
	if code := admin.do("POST", "/api/send", map[string]any{"to": []Addr{{Email: "nobody@skien.test"}}, "subject": "x", "html": "x"}, nil); code != 400 {
		t.Fatal("send to unknown local user should fail")
	}
	var inbox threadList
	sajin.do("GET", "/api/threads?folder=inbox", nil, &inbox)
	if inbox.Total != 1 || inbox.Items[0].Subject != "Welcome aboard" || !inbox.Items[0].Unread {
		t.Fatalf("sajin inbox unexpected: %+v", inbox)
	}
	var sent threadList
	admin.do("GET", "/api/threads?folder=sent", nil, &sent)
	if sent.Total != 1 {
		t.Fatalf("admin sent unexpected: %+v", sent)
	}
	// The group delivered a copy to the admin too
	var adminInbox threadList
	admin.do("GET", "/api/threads?folder=inbox", nil, &adminInbox)
	if adminInbox.Total != 1 {
		t.Fatalf("group delivery to admin missing: %+v", adminInbox)
	}

	// Open the thread: HTML sanitised, marked read
	var tv struct {
		Messages []MessageView `json:"messages"`
	}
	sajin.do("GET", fmt.Sprintf("/api/threads/%d?context=inbox", inbox.Items[0].ThreadID), nil, &tv)
	if len(tv.Messages) != 1 || strings.Contains(tv.Messages[0].HTML, "<script") || !strings.Contains(tv.Messages[0].HTML, "<b>Sajin</b>") {
		t.Fatalf("thread view unexpected: %+v", tv.Messages)
	}
	if len(tv.Messages[0].Bcc) != 0 {
		t.Fatal("recipient must not see bcc")
	}
	sajin.do("GET", "/api/threads?folder=inbox", nil, &inbox)
	if inbox.Items[0].Unread {
		t.Fatal("thread should be read after opening")
	}

	// Sajin replies: should join the same conversation for both
	if code := sajin.do("POST", "/api/send", map[string]any{"to": []Addr{{Email: "asi@skien.test"}}, "subject": "Re: Welcome aboard",
		"html": "<p>Thanks!</p>", "reply_to_id": tv.Messages[0].ID}, nil); code != 200 {
		t.Fatal("reply failed")
	}
	admin.do("GET", "/api/threads?folder=inbox", nil, &adminInbox)
	if adminInbox.Total != 1 || adminInbox.Items[0].Count < 2 {
		t.Fatalf("reply should thread with group copy in admin inbox: %+v", adminInbox)
	}
	admin.do("GET", fmt.Sprintf("/api/threads/%d?context=inbox", adminInbox.Items[0].ThreadID), nil, &tv)
	if len(tv.Messages) < 3 { // group copy + sent original + reply (+ bounce, timing dependent)
		t.Fatalf("expected 3 messages in conversation, got %d", len(tv.Messages))
	}

	// Inbound SMTP from an outside sender
	msg := "From: Ravi <ravi@example.org>\r\nTo: sajin@skien.test\r\nSubject: Quarterly invoice\r\nMessage-ID: <abc123@example.org>\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XYZ\r\n\r\n" +
		"--XYZ\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nPlease find the invoice attached. Caf=E9\r\n" +
		"--XYZ\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--XYZ--\r\n"
	if err := sendSMTP(cfg.SMTPAddr, "ravi@example.org", []string{"sajin+billing@skien.test"}, []byte(msg)); err != nil {
		t.Fatalf("inbound smtp: %v", err)
	}
	// Relaying is refused on the MX port
	if err := sendSMTP(cfg.SMTPAddr, "ravi@example.org", []string{"x@gmail.com"}, []byte(msg)); err == nil {
		t.Fatal("open relay!")
	}
	// Spoofing a local From without authentication is refused
	spoof := strings.Replace(msg, "From: Ravi <ravi@example.org>", "From: Asi <asi@skien.test>", 1)
	if err := sendSMTP(cfg.SMTPAddr, "asi@skien.test", []string{"sajin@skien.test"}, []byte(spoof)); err == nil {
		t.Fatal("spoofed local sender accepted")
	}
	sajin.do("GET", "/api/threads?folder=inbox", nil, &inbox)
	if inbox.Total != 2 || inbox.Items[0].Subject != "Quarterly invoice" || !inbox.Items[0].HasAttach {
		t.Fatalf("inbound message missing: %+v", inbox.Items)
	}
	sajin.do("GET", fmt.Sprintf("/api/threads/%d?context=inbox", inbox.Items[0].ThreadID), nil, &tv)
	if !strings.Contains(tv.Messages[0].Text, "Café") || len(tv.Messages[0].Attachments) != 1 || tv.Messages[0].Attachments[0].Filename != "invoice.pdf" {
		t.Fatalf("parse unexpected: %q %+v", tv.Messages[0].Text, tv.Messages[0].Attachments)
	}
	ar, _ := sajin.c.Get(srv.URL + fmt.Sprintf("/api/messages/%d/attachments/0", tv.Messages[0].ID))
	body, _ := io.ReadAll(ar.Body)
	ar.Body.Close()
	if string(body) != "%PDF-1.4\n" || ar.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("attachment download unexpected: %q %s", body, ar.Header.Get("Content-Type"))
	}

	// Search
	var res threadList
	sajin.do("GET", "/api/threads?q=invoice", nil, &res)
	if res.Total != 1 {
		t.Fatalf("search failed: %+v", res)
	}
	sajin.do("GET", "/api/threads?q=from:ravi%20has:attachment", nil, &res)
	if res.Total != 1 {
		t.Fatalf("operator search failed: %+v", res)
	}

	// Archive, trash, restore, delete forever
	tid := inbox.Items[0].ThreadID
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "inbox", "action": "archive"}, nil)
	sajin.do("GET", "/api/threads?folder=archive", nil, &res)
	if res.Total != 1 {
		t.Fatal("archive failed")
	}
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "archive", "action": "trash"}, nil)
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "trash", "action": "inbox"}, nil)
	sajin.do("GET", "/api/threads?folder=archive", nil, &res)
	if res.Total != 1 {
		t.Fatal("restore from trash should return to Archive (where it was deleted from)")
	}
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "archive", "action": "inbox"}, nil)
	sajin.do("GET", "/api/threads?folder=inbox", nil, &res)
	if res.Total != 2 {
		t.Fatal("move to inbox failed")
	}
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "inbox", "action": "trash"}, nil)
	sajin.do("POST", "/api/threads/action", map[string]any{"threads": []int64{tid}, "view": "trash", "action": "delete"}, nil)
	sajin.do("GET", "/api/threads?folder=trash", nil, &res)
	if res.Total != 0 {
		t.Fatal("delete forever failed")
	}

	// Submission port: auth required, then send as self
	c, err := smtp.Dial(cfg.SubmissionAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sajin@skien.test"); err == nil {
		t.Fatal("submission without auth accepted")
	}
	c.Close()
	c, err = smtp.Dial(cfg.SubmissionAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.Hello("laptop")
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Auth(smtp.PlainAuth("", "sajin@skien.test", "wrongpass", "127.0.0.1")); err == nil {
		t.Fatal("bad submission password accepted")
	}
	c.Close()
	c, _ = smtp.Dial(cfg.SubmissionAddr)
	c.Hello("laptop")
	c.StartTLS(&tls.Config{InsecureSkipVerify: true})
	if err := c.Auth(smtp.PlainAuth("", "sajin@skien.test", "anothersecret9", "127.0.0.1")); err != nil {
		t.Fatalf("submission auth: %v", err)
	}
	if err := c.Mail("asi@skien.test"); err == nil {
		t.Fatal("sending as someone else accepted")
	}
	c.Reset()
	if err := c.Mail("sajin@skien.test"); err != nil {
		t.Fatal(err)
	}
	c.Rcpt("asi@skien.test")
	w, _ := c.Data()
	io.WriteString(w, "From: Sajin <sajin@skien.test>\r\nTo: asi@skien.test\r\nBcc: secret@skien.test\r\nSubject: From my phone\r\nMessage-ID: <phone1@x>\r\n\r\nSent via SMTP\r\n")
	if err := w.Close(); err != nil {
		t.Fatalf("submission data: %v", err)
	}
	c.Quit()
	admin.do("GET", "/api/threads?q=subject:phone", nil, &res)
	if res.Total != 1 {
		t.Fatalf("submitted message not delivered: %+v", res)
	}
	sajin.do("GET", "/api/threads?folder=sent&q=", nil, &res)
	admin.do("GET", fmt.Sprintf("/api/threads/%d?context=inbox", 0), nil, nil)

	// The external recipient bounces back to the admin (domain doesn't exist)
	waitFor(t, "bounce", func() bool {
		admin.do("GET", "/api/threads?q=subject:Delivery", nil, &res)
		return res.Total == 1
	})

	// Out-of-office: Sajin is away, Asi gets exactly one automatic reply
	if code := sajin.do("PUT", "/api/vacation", map[string]any{"enabled": true, "subject": "Away", "body": "I'm on leave until Monday."}, nil); code != 200 {
		t.Fatal("vacation save failed")
	}
	for i := 0; i < 2; i++ {
		admin.do("POST", "/api/send", map[string]any{"to": []Addr{{Email: "sajin@skien.test"}}, "subject": fmt.Sprintf("Question %d", i), "html": "<p>?</p>"}, nil)
	}
	waitFor(t, "auto-reply", func() bool {
		admin.do("GET", "/api/threads?q=subject:Away", nil, &res)
		return res.Total == 1
	})
	time.Sleep(300 * time.Millisecond)
	admin.do("GET", "/api/threads?q=subject:Away", nil, &res)
	if res.Total != 1 {
		t.Fatalf("expected exactly one auto-reply, got %d", res.Total)
	}
	sajin.do("PUT", "/api/vacation", map[string]any{"enabled": false}, nil)

	// Drafts
	var dr map[string]any
	sajin.do("PUT", "/api/drafts", map[string]any{"subject": "Draft one", "html": "<p>hi</p>", "to": []Addr{{Email: "asi@skien.test"}}}, &dr)
	var drafts []map[string]any
	sajin.do("GET", "/api/drafts", nil, &drafts)
	if len(drafts) != 1 {
		t.Fatal("draft not saved")
	}

	// Non-admin can't reach admin API
	if code := sajin.do("GET", "/api/admin/users", nil, nil); code != 403 {
		t.Fatal("admin api exposed to user")
	}
	// Disable user -> session revoked
	admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", created.User.ID), map[string]any{"name": "Sajin", "role": "user", "status": "disabled"}, nil)
	if code := sajin.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatal("disabled user still signed in")
	}
	// The last admin can't demote themself
	if code := admin.do("PUT", "/api/admin/users/1", map[string]any{"name": "Asi", "role": "user", "status": "active"}, nil); code != 400 {
		t.Fatal("last admin demoted")
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	raw, mid, err := BuildMessage(Compose{From: Addr{Name: "Asi K", Email: "asi@skien.test"}, To: []Addr{{Name: "Ünïcode Name", Email: "x@y.test"}},
		Subject: "Hello — world ✓", HTML: "<p>Hi <i>there</i></p>", Files: []OutgoingPart{{Filename: "résumé.txt", Type: "text/plain", Data: []byte("cv")}}}, "mail.test")
	if err != nil {
		t.Fatal(err)
	}
	m := ParseMeta(raw)
	if m.Subject != "Hello — world ✓" || m.MessageID != mid || m.To[0].Name != "Ünïcode Name" || !m.HasAttach || !strings.Contains(m.Text, "Hi there") {
		t.Fatalf("round trip mismatch: %+v", m)
	}
	b := ParseBody(raw)
	if len(b.Attachments) != 1 || b.Attachments[0].Filename != "résumé.txt" {
		t.Fatalf("attachment mismatch %+v", b.Attachments)
	}
	if got := string(stripHeader([]byte("From: a\r\nBcc: x,\r\n y\r\nTo: b\r\n\r\nbody"), "Bcc")); got != "From: a\r\nTo: b\r\n\r\nbody" {
		t.Fatalf("stripHeader: %q", got)
	}
}
