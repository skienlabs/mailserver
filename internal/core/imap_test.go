package core

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// startIMAPTest serves IMAP (TLS and STARTTLS) for the test app; returns both addresses.
func startIMAPTest(t *testing.T, app *App) (tlsAddr, plainAddr string) {
	t.Helper()
	app.Cfg.IMAPSAddr, app.Cfg.IMAPAddr = freePort(t), freePort(t)
	app.startIMAP()
	t.Cleanup(app.stopIMAP)
	return app.Cfg.IMAPSAddr, app.Cfg.IMAPAddr
}

func imapDialT(t *testing.T, addr, user, pass string) *imapclient.Client {
	t.Helper()
	var c *imapclient.Client
	var err error
	for i := 0; i < 50; i++ { // the listener starts in the background
		c, err = imapclient.DialTLS(addr, &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 20 * time.Second
	if user != "" {
		if err := c.Login(user, pass); err != nil {
			t.Fatalf("login: %v", err)
		}
	}
	t.Cleanup(func() { c.Logout(); c.Terminate() })
	return c
}

// imapWire is a bare protocol session, for checking exact reply lines (response codes the
// client library hides).
type imapWire struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	n    int
}

func imapWireT(t *testing.T, addr, user, pass string) *imapWire {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	w := &imapWire{t: t, conn: conn, r: bufio.NewReader(conn)}
	w.line() // greeting
	if got := w.cmd(fmt.Sprintf("LOGIN %q %q", user, pass)); !strings.Contains(got, " OK") {
		t.Fatalf("wire login: %s", got)
	}
	return w
}

func (w *imapWire) line() string {
	w.t.Helper()
	l, err := w.r.ReadString('\n')
	if err != nil {
		w.t.Fatalf("wire read: %v", err)
	}
	return strings.TrimRight(l, "\r\n")
}

// cmd sends one command (an optional literal goes after the server's "+") and returns the
// tagged reply line.
func (w *imapWire) cmd(c string, literal ...string) string {
	w.t.Helper()
	w.n++
	tag := fmt.Sprintf("w%d", w.n)
	if len(literal) > 0 {
		fmt.Fprintf(w.conn, "%s %s {%d}\r\n", tag, c, len(literal[0]))
		for {
			l := w.line()
			if strings.HasPrefix(l, "+") {
				break
			}
			if strings.HasPrefix(l, tag+" ") {
				return l
			}
		}
		io.WriteString(w.conn, literal[0]+"\r\n")
	} else {
		fmt.Fprintf(w.conn, "%s %s\r\n", tag, c)
	}
	for {
		if l := w.line(); strings.HasPrefix(l, tag+" ") {
			return l
		}
	}
}

// imapCount is how many messages the server says this session's folder holds.
func imapCount(t *testing.T, c *imapclient.Client) int {
	t.Helper()
	ids, err := c.Search(imap.NewSearchCriteria())
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

func fetchAll(t *testing.T, c *imapclient.Client, set string, items ...imap.FetchItem) []*imap.Message {
	t.Helper()
	seq, err := imap.ParseSeqSet(set)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *imap.Message, 100)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seq, items, ch) }()
	var out []*imap.Message
	for m := range ch {
		out = append(out, m)
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch %s: %v", set, err)
	}
	return out
}

func uidFetch(t *testing.T, c *imapclient.Client, set string, items ...imap.FetchItem) []*imap.Message {
	t.Helper()
	seq, _ := imap.ParseSeqSet(set)
	ch := make(chan *imap.Message, 100)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(seq, items, ch) }()
	var out []*imap.Message
	for m := range ch {
		out = append(out, m)
	}
	if err := <-done; err != nil {
		t.Fatalf("uid fetch %s: %v", set, err)
	}
	return out
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}

func TestIMAPSignInAndList(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	tlsAddr, plainAddr := startIMAPTest(t, app)
	app.CreateFolder(a.ID, "Clients/Acme")

	c := imapDialT(t, tlsAddr, "", "")
	if err := c.Login(a.Email, "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := c.Login(strings.ToUpper(a.Email), "password-asi"); err != nil {
		t.Fatalf("login (case-insensitive address): %v", err)
	}
	caps, _ := c.Capability()
	if !caps["SPECIAL-USE"] || !caps["IDLE"] || !caps["MOVE"] {
		t.Fatalf("capabilities: %v", caps)
	}
	ch := make(chan *imap.MailboxInfo, 50)
	if err := c.List("", "*", ch); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for mi := range ch {
		got[mi.Name] = mi.Attributes
		if mi.Delimiter != "/" {
			t.Fatalf("delimiter %q", mi.Delimiter)
		}
	}
	for name, attr := range map[string]string{"INBOX": "", "Sent": imap.SentAttr, "Drafts": imap.DraftsAttr, "Archive": imap.ArchiveAttr, "Spam": imap.JunkAttr, "Trash": imap.TrashAttr, "Clients": imap.HasChildrenAttr, "Clients/Acme": imap.HasNoChildrenAttr} {
		attrs, ok := got[name]
		if !ok || (attr != "" && !hasFlag(attrs, attr)) {
			t.Fatalf("%s: %v (list %v)", name, attrs, got)
		}
	}

	// Port 143: no plain-text sign-in before STARTTLS.
	pc, err := imapclient.Dial(plainAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Terminate()
	if err := pc.Login(a.Email, "password-asi"); err == nil {
		t.Fatal("plain-text login allowed")
	}
	if err := pc.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if err := pc.Login(a.Email, "password-asi"); err != nil {
		t.Fatalf("login after STARTTLS: %v", err)
	}

	// Accounts that may not sign in.
	app.DB.Exec(`UPDATE users SET must_change_password=1 WHERE id=?`, b.ID)
	c2 := imapDialT(t, tlsAddr, "", "")
	if err := c2.Login(b.Email, "password-sajin"); err == nil || !strings.Contains(err.Error(), "web app") {
		t.Fatalf("must-change-password account: %v", err)
	}
	app.DB.Exec(`UPDATE users SET must_change_password=0, status='disabled' WHERE id=?`, b.ID)
	c3 := imapDialT(t, tlsAddr, "", "")
	if err := c3.Login(b.Email, "password-sajin"); err == nil {
		t.Fatal("disabled account signed in")
	}
}

func TestIMAPFetchAndFlags(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	var ids []int64
	for i := 1; i <= 3; i++ {
		ids = append(ids, deliverTo(t, app, a, "inbox", fmt.Sprintf("p%d@x.test", i), fmt.Sprintf("Subject %d", i)))
	}
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	st, err := c.Select("INBOX", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Messages != 3 || st.UidNext != 4 || st.UidValidity == 0 || st.UnseenSeqNum != 1 {
		t.Fatalf("select: %+v", st)
	}
	msgs := fetchAll(t, c, "1:*", imap.FetchUid, imap.FetchFlags, imap.FetchRFC822Size, imap.FetchInternalDate, imap.FetchEnvelope)
	if len(msgs) != 3 || msgs[0].Envelope.Subject != "Subject 1" || msgs[2].Uid != 3 {
		t.Fatalf("fetch: %+v", msgs)
	}
	var blob string
	app.DB.QueryRow(`SELECT blob FROM messages WHERE id=?`, ids[0]).Scan(&blob)
	fi, _ := os.Stat(app.blobPath(blob))
	if int64(msgs[0].Size) != fi.Size() {
		t.Fatalf("RFC822.SIZE %d, file %d", msgs[0].Size, fi.Size())
	}
	// PEEK leaves it unread; BODY[] marks it read (and says so).
	peek, _ := imap.ParseBodySectionName("BODY.PEEK[]")
	m := fetchAll(t, c, "1", peek.FetchItem())[0]
	raw, _ := io.ReadAll(m.GetBody(peek))
	if !bytes.Contains(raw, []byte("Subject: Subject 1")) || int64(len(raw)) != fi.Size() {
		t.Fatal("BODY.PEEK[] content")
	}
	var seen bool
	app.DB.QueryRow(`SELECT seen FROM messages WHERE id=?`, ids[0]).Scan(&seen)
	if seen {
		t.Fatal("PEEK marked the message read")
	}
	full, _ := imap.ParseBodySectionName("BODY[]")
	m = fetchAll(t, c, "1", full.FetchItem())[0]
	if !hasFlag(m.Flags, imap.SeenFlag) {
		t.Fatalf("implicit \\Seen not reported: %v", m.Flags)
	}
	app.DB.QueryRow(`SELECT seen FROM messages WHERE id=?`, ids[0]).Scan(&seen)
	if !seen {
		t.Fatal("BODY[] didn't mark the message read for the web app")
	}
	hdr, _ := imap.ParseBodySectionName("BODY.PEEK[HEADER.FIELDS (SUBJECT)]")
	m = fetchAll(t, c, "2", hdr.FetchItem())[0]
	h, _ := io.ReadAll(m.GetBody(hdr))
	if strings.TrimSpace(string(h)) != "Subject: Subject 2" {
		t.Fatalf("header fields: %q", h)
	}
	// UID n:* past the end still returns the last message (RFC 3501).
	if got := uidFetch(t, c, "50:*", imap.FetchUid); len(got) != 1 || got[0].Uid != 3 {
		t.Fatalf("UID 50:* -> %+v", got)
	}
	// STORE: flags reach the web app; keywords are kept.
	seq, _ := imap.ParseSeqSet("2")
	if err := c.Store(seq, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.FlaggedFlag, "$Forwarded"}, nil); err != nil {
		t.Fatal(err)
	}
	var flagged bool
	app.DB.QueryRow(`SELECT flagged FROM messages WHERE id=?`, ids[1]).Scan(&flagged)
	if !flagged {
		t.Fatal("\\Flagged not stored")
	}
	m = fetchAll(t, c, "2", imap.FetchFlags)[0]
	if !hasFlag(m.Flags, "$Forwarded") || !hasFlag(m.Flags, imap.FlaggedFlag) {
		t.Fatalf("flags after store: %v", m.Flags)
	}
	// A change made in the web app shows up on NOOP.
	updates := make(chan imapclient.Update, 20)
	c.Updates = updates
	app.ThreadAction(a.ID, []int64{threadOf(t, app, ids[2])}, "inbox", "read")
	if err := c.Noop(); err != nil {
		t.Fatal(err)
	}
	var sawFlags bool
	for len(updates) > 0 {
		if u, ok := (<-updates).(*imapclient.MessageUpdate); ok && u.Message.SeqNum == 3 && hasFlag(u.Message.Flags, imap.SeenFlag) {
			sawFlags = true
		}
	}
	if !sawFlags {
		t.Fatal("web change not reported on NOOP")
	}
	// SEARCH: flags and text.
	crit := imap.NewSearchCriteria()
	crit.WithFlags = []string{imap.FlaggedFlag}
	if res, err := c.Search(crit); err != nil || len(res) != 1 || res[0] != 2 {
		t.Fatalf("search flagged: %v %v", res, err)
	}
	crit = imap.NewSearchCriteria()
	crit.Text = []string{"Subject 3"}
	if res, err := c.UidSearch(crit); err != nil || len(res) != 1 || res[0] != 3 {
		t.Fatalf("search text: %v %v", res, err)
	}
}

func TestIMAPNewMailNoopAndIdle(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	deliverTo(t, app, a, "inbox", "x@x.test", "First")
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	deliverTo(t, app, a, "inbox", "x@x.test", "Second")
	c.Noop()
	if c.Mailbox().Messages != 2 {
		t.Fatalf("EXISTS after NOOP: %d", c.Mailbox().Messages)
	}
	// IDLE wakes on new mail (well before the 30 s poll).
	updates := make(chan imapclient.Update, 10)
	c.Updates = updates
	stop := make(chan struct{})
	idleDone := make(chan error, 1)
	go func() { idleDone <- c.Idle(stop, nil) }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	deliverTo(t, app, a, "inbox", "x@x.test", "Third")
	select {
	case u := <-updates:
		if mu, ok := u.(*imapclient.MailboxUpdate); !ok || mu.Mailbox.Messages != 3 {
			t.Fatalf("idle update: %#v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IDLE didn't report new mail")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("IDLE took %v", el)
	}
	close(stop)
	if err := <-idleDone; err != nil {
		t.Fatalf("idle: %v", err)
	}
	if got := fetchAll(t, c, "3", imap.FetchEnvelope); got[0].Envelope.Subject != "Third" {
		t.Fatal("fetch after idle")
	}
}

func TestIMAPMoveCopyExpunge(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	acme, _ := app.CreateFolder(a.ID, "Clients/Acme")
	var ids []int64
	for i := 1; i <= 4; i++ {
		ids = append(ids, deliverTo(t, app, a, "inbox", "x@x.test", fmt.Sprintf("M%d", i)))
	}
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	c.Select("INBOX", false)
	updates := make(chan imapclient.Update, 50)
	c.Updates = updates

	// MOVE 1 to Archive: it moves for the web app too, and the app gets EXPUNGE 1.
	seq, _ := imap.ParseSeqSet("1")
	if err := c.Move(seq, "Archive"); err != nil {
		t.Fatal(err)
	}
	if folderOf(t, app, ids[0]) != "archive" {
		t.Fatal("MOVE didn't move")
	}
	// The client library doesn't count EXPUNGE itself; check the server sent "* 1 EXPUNGE".
	sawExpunge := false
	for len(updates) > 0 {
		if u, ok := (<-updates).(*imapclient.ExpungeUpdate); ok && u.SeqNum == 1 {
			sawExpunge = true
		}
	}
	if !sawExpunge {
		t.Fatal("no EXPUNGE after MOVE")
	}
	if got := fetchAll(t, c, "1", imap.FetchEnvelope); got[0].Envelope.Subject != "M2" {
		t.Fatalf("numbers after MOVE: %s", got[0].Envelope.Subject)
	}
	// COPY to a custom folder makes a second message (never dropped as a duplicate).
	if err := c.Copy(seq, "Clients/Acme"); err != nil {
		t.Fatal(err)
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder=?`, a.ID, acme.Key).Scan(&n)
	if n != 1 || folderOf(t, app, ids[1]) != "inbox" {
		t.Fatalf("copy: %d in folder", n)
	}
	if err := c.Copy(seq, "No/Such"); err == nil {
		t.Fatal("copy to a missing folder accepted")
	}
	wire := imapWireT(t, tlsAddr, a.Email, "password-asi")
	wire.cmd("SELECT INBOX")
	if got := wire.cmd("COPY 1 No/Such"); !strings.Contains(got, "NO [TRYCREATE]") {
		t.Fatalf("copy to missing folder: %s", got)
	}
	// \Deleted + EXPUNGE in the inbox moves to Trash (recoverable).
	if err := c.Store(seq, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); err != nil {
		t.Fatal(err)
	}
	exp := make(chan uint32, 10)
	if err := c.Expunge(exp); err != nil {
		t.Fatal(err)
	}
	if got := <-exp; got != 1 {
		t.Fatalf("expunged seq %d", got)
	}
	if folderOf(t, app, ids[1]) != "trash" {
		t.Fatal("expunge outside Trash must move to Trash")
	}
	// In Trash it's deleted for good, file and all.
	if _, err := c.Select("Trash", false); err != nil {
		t.Fatal(err)
	}
	var blob string
	app.DB.QueryRow(`SELECT blob FROM messages WHERE id=?`, ids[1]).Scan(&blob)
	all, _ := imap.ParseSeqSet("1:*")
	c.Store(all, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil)
	if err := c.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, ids[1]).Scan(&n)
	if _, err := os.Stat(app.blobPath(blob)); n != 0 || !os.IsNotExist(err) {
		t.Fatal("expunge in Trash must delete for good")
	}
	// EXAMINE is read-only: CLOSE doesn't expunge, FETCH doesn't mark read.
	c.Select("INBOX", true)
	two, _ := imap.ParseSeqSet("1")
	full, _ := imap.ParseBodySectionName("BODY[]")
	fetchAll(t, c, "1", full.FetchItem())
	var seen bool
	app.DB.QueryRow(`SELECT seen FROM messages WHERE id=?`, ids[2]).Scan(&seen)
	if seen {
		t.Fatal("FETCH in EXAMINE marked read")
	}
	if err := c.Store(two, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); err == nil {
		t.Fatal("STORE allowed after EXAMINE")
	}

	// Two connections: a move made by the other one (or the web) arrives as EXPUNGE on NOOP.
	c.Select("INBOX", false)
	c2 := imapDialT(t, tlsAddr, a.Email, "password-asi")
	c2.Select("INBOX", false)
	if err := c2.Move(two, "Archive"); err != nil {
		t.Fatal(err)
	}
	for len(updates) > 0 {
		<-updates
	}
	c.Noop()
	sawExpunge = false
	for len(updates) > 0 {
		if u, ok := (<-updates).(*imapclient.ExpungeUpdate); ok && u.SeqNum == 1 {
			sawExpunge = true
		}
	}
	if n := imapCount(t, c); !sawExpunge || n != 1 {
		t.Fatalf("other connection's move: expunge=%v messages=%d", sawExpunge, n)
	}
	if got := fetchAll(t, c, "1", imap.FetchEnvelope); got[0].Envelope.Subject != "M4" {
		t.Fatalf("after another session's move: %s", got[0].Envelope.Subject)
	}
	// The web app moving a message to a folder: the app sees it leave and arrive.
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, ids[3])}, "inbox", "move:"+acme.Key, nil)
	c.Noop()
	if n := imapCount(t, c); n != 0 {
		t.Fatalf("web move not reported: %d left", n)
	}
	st, err := c.Status("Clients/Acme", []imap.StatusItem{imap.StatusMessages, imap.StatusUidNext})
	if err != nil || st.Messages != 2 || st.UidNext != 3 {
		t.Fatalf("status of the folder: %+v %v", st, err)
	}
}

func TestIMAPAppendSentDrafts(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	msg := func(id, subj string) *bytes.Buffer {
		return bytes.NewBufferString(fmt.Sprintf("From: %s\r\nTo: x@x.test\r\nSubject: %s\r\nMessage-ID: <%s>\r\nDate: Mon, 02 Jun 2025 10:00:00 +0000\r\n\r\nbody\r\n", a.Email, subj, id))
	}
	// A copy of mail sent through our SMTP is already in Sent: the app's upload is dropped.
	sentRaw := msg("sent1@skien.test", "Already sent").Bytes()
	app.Deliver(a.ID, sentRaw, DeliverOpts{Folder: "sent", Seen: true})
	if err := c.Append("Sent", nil, time.Time{}, bytes.NewReader(sentRaw)); err != nil {
		t.Fatal(err)
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND message_id='sent1@skien.test'`, a.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("sent copy stored %d times", n)
	}
	// Other uploads are stored (with their date and flags).
	when := time.Date(2024, 1, 2, 3, 4, 0, 0, time.UTC)
	if err := c.Append("INBOX", []string{imap.FlaggedFlag}, when, msg("in1@x.test", "Uploaded")); err != nil {
		t.Fatal(err)
	}
	got := storedBySubject(t, app, a.ID)["Uploaded"]
	if got.folder != "inbox" || !got.flagged || got.received != when.UnixMilli() {
		t.Fatalf("append: %+v", got)
	}
	// Selected mailbox learns about its own append right away.
	c.Select("INBOX", false)
	if err := c.Append("INBOX", nil, time.Time{}, msg("in2@x.test", "Second upload")); err != nil {
		t.Fatal(err)
	}
	if c.Mailbox().Messages != 2 {
		t.Fatalf("EXISTS after APPEND: %d", c.Mailbox().Messages)
	}
	// Drafts from mail apps: their own folder, not in the web app's lists.
	var usedBefore int64
	app.DB.QueryRow(`SELECT used_bytes FROM users WHERE id=?`, a.ID).Scan(&usedBefore)
	if err := c.Append("Drafts", []string{imap.DraftFlag}, time.Time{}, msg("d1@skien.test", "Half written")); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedBySubject(t, app, a.ID)["Half written"]; ok {
		t.Fatal("app draft leaked into the mailbox")
	}
	st, err := c.Select("Drafts", false)
	if err != nil || st.Messages != 1 {
		t.Fatalf("drafts: %+v %v", st, err)
	}
	m := fetchAll(t, c, "1", imap.FetchFlags, imap.FetchEnvelope)[0]
	if m.Envelope.Subject != "Half written" || !hasFlag(m.Flags, imap.DraftFlag) {
		t.Fatalf("draft fetch: %+v", m)
	}
	one, _ := imap.ParseSeqSet("1")
	c.Store(one, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil)
	if err := c.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	var usedAfter int64
	app.DB.QueryRow(`SELECT COUNT(*) FROM imap_drafts WHERE user_id=?`, a.ID).Scan(&n)
	app.DB.QueryRow(`SELECT used_bytes FROM users WHERE id=?`, a.ID).Scan(&usedAfter)
	if n != 0 || usedAfter != usedBefore {
		t.Fatalf("draft delete: %d left, used %d -> %d", n, usedBefore, usedAfter)
	}
	// Limits: size and quota.
	big := bytes.NewBufferString("Subject: big\r\n\r\n" + strings.Repeat("x", int(app.MaxMessageBytes())))
	if err := c.Append("INBOX", nil, time.Time{}, big); err == nil {
		t.Fatal("oversized append accepted")
	}
	app.DB.Exec(`UPDATE users SET quota_bytes=used_bytes+10 WHERE id=?`, a.ID)
	if err := c.Append("INBOX", nil, time.Time{}, msg("q@x.test", "Over quota")); err == nil || !strings.Contains(err.Error(), "mailbox is full") {
		t.Fatalf("over quota: %v", err)
	}
	wire := imapWireT(t, tlsAddr, a.Email, "password-asi")
	if got := wire.cmd("APPEND INBOX", "Subject: q\r\n\r\nhello"); !strings.Contains(got, "NO [OVERQUOTA] Your mailbox is full") {
		t.Fatalf("over quota on the wire: %s", got)
	}
}

func TestIMAPManageFolders(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	if err := c.Create("Projects/2026/"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.folderByName(a.ID, "Projects/2026"); err != nil {
		t.Fatal("CREATE didn't make the folder")
	}
	if err := c.Create("sent"); err == nil {
		t.Fatal("created a folder named like a built-in one")
	}
	id := deliverTo(t, app, a, "inbox", "x@x.test", "Plan")
	c.Select("INBOX", false)
	one, _ := imap.ParseSeqSet("1")
	if err := c.Move(one, "Projects/2026"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("Projects", "Work"); err != nil {
		t.Fatal(err)
	}
	f, err := app.folderByName(a.ID, "Work/2026")
	if err != nil || folderOf(t, app, id) != f.Key {
		t.Fatal("RENAME didn't carry the subfolder and its mail")
	}
	if err := c.Rename("INBOX", "Old"); err == nil {
		t.Fatal("renamed INBOX")
	}
	if err := c.Delete("Trash"); err == nil {
		t.Fatal("deleted Trash")
	}
	if err := c.Delete("Work"); err != nil {
		t.Fatal(err)
	}
	if folderOf(t, app, id) != "archive" {
		t.Fatal("DELETE must move the mail to Archive")
	}
}

func TestIMAPSessionEndsAndLimits(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	old := imapAliveEvery
	imapAliveEvery = 0
	oldMax := imapMaxConnsPerIP
	imapMaxConnsPerIP = 3 // set before the server starts: the listener reads it
	t.Cleanup(func() { imapAliveEvery, imapMaxConnsPerIP = old, oldMax })
	tlsAddr, _ := startIMAPTest(t, app)
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	app.DB.Exec(`UPDATE users SET password_changed_at=? WHERE id=?`, nowMs()+1000, a.ID)
	if err := c.Noop(); err == nil {
		if _, err := c.Select("INBOX", false); err == nil {
			t.Fatal("session survived a password change")
		}
	}

	// Before sign-in: big literals are refused and the connection is cut.
	conn, err := tls.Dial("tcp", tlsAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	r.ReadString('\n') // greeting
	fmt.Fprintf(conn, "a1 LOGIN {200000}\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := r.ReadString('\n')
	if strings.HasPrefix(line, "+") {
		t.Fatalf("server asked for a 200 KB literal before sign-in: %q", line)
	}

	// Per-address connection limit (3, set above).
	var conns []net.Conn
	refused := false
	for i := 0; i < 6; i++ {
		cc, err := tls.Dial("tcp", tlsAddr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			refused = true
			break
		}
		conns = append(conns, cc)
		cc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := bufio.NewReader(cc).ReadString('\n'); err != nil {
			refused = true
			break
		}
	}
	for _, cc := range conns {
		cc.Close()
	}
	if !refused {
		t.Fatal("no per-address connection limit")
	}
}

// Stopping right after starting (before the serve goroutines run) must not hang shutdown.
func TestIMAPStartStopQuickly(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	for i := 0; i < 20; i++ {
		app.Cfg.IMAPSAddr, app.Cfg.IMAPAddr = freePort(t), freePort(t)
		app.startIMAP()
		done := make(chan struct{})
		go func() { app.stopIMAP(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stopIMAP hung")
		}
	}
}

// Keywords keep the case the app used, and SEARCH finds them whatever the case, on both the
// quick path (flags only) and the full path (with a body criterion).
func TestIMAPKeywordCase(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	deliverTo(t, app, a, "inbox", "x@x.test", "Keyword one")
	deliverTo(t, app, a, "inbox", "x@x.test", "Keyword two")
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	c.Select("INBOX", false)
	one, _ := imap.ParseSeqSet("1")
	if err := c.Store(one, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{"$MailFlagBit0", "NonJunk"}, nil); err != nil {
		t.Fatal(err)
	}
	// The client library lowercases flags it reads, so look at the server's own reply.
	wire := imapWireT(t, tlsAddr, a.Email, "password-asi")
	wire.cmd("SELECT INBOX")
	wireFlags := func() string {
		wire.n++
		tag := fmt.Sprintf("w%d", wire.n)
		fmt.Fprintf(wire.conn, "%s FETCH 1 (FLAGS)\r\n", tag)
		var res string
		for {
			l := wire.line()
			if strings.HasPrefix(l, "* 1 FETCH") {
				res = l
			}
			if strings.HasPrefix(l, tag+" ") {
				return res
			}
		}
	}
	if got := wireFlags(); !strings.Contains(got, "$MailFlagBit0") || !strings.Contains(got, "NonJunk") {
		t.Fatalf("keyword case lost: %s", got)
	}
	// Same keyword in another case: no duplicate, original spelling kept.
	c.Store(one, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{"$mailflagbit0"}, nil)
	if got := fetchAll(t, c, "1", imap.FetchFlags)[0].Flags; len(got) != 2 {
		t.Fatalf("duplicate keyword: %v", got)
	}
	if got := wireFlags(); !strings.Contains(got, "$MailFlagBit0") {
		t.Fatalf("spelling changed: %s", got)
	}
	for _, q := range []string{"KEYWORD $MAILFLAGBIT0", "KEYWORD $MailFlagBit0 BODY body", "UNKEYWORD nonjunk BODY body"} {
		wire.n++
		tag := fmt.Sprintf("w%d", wire.n)
		fmt.Fprintf(wire.conn, "%s SEARCH %s\r\n", tag, q)
		var res string
		for {
			l := wire.line()
			if strings.HasPrefix(l, "* SEARCH") {
				res = strings.TrimSpace(strings.TrimPrefix(l, "* SEARCH"))
			}
			if strings.HasPrefix(l, tag+" ") {
				break
			}
		}
		want := "1"
		if strings.HasPrefix(q, "UNKEYWORD") {
			want = "2"
		}
		if res != want {
			t.Fatalf("SEARCH %s = %q, want %q", q, res, want)
		}
	}
	// Removing in another case works.
	c.Store(one, imap.FormatFlagsOp(imap.RemoveFlags, true), []interface{}{"nonjunk"}, nil)
	if got := fetchAll(t, c, "1", imap.FetchFlags)[0].Flags; hasFlag(got, "NonJunk") || len(got) != 1 {
		t.Fatalf("remove in other case: %v", got)
	}
}

// The Email apps settings page shows IMAP only when it's really listening.
func TestMailAppsInfo(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	app.Cfg.SubmissionAddr, app.Cfg.SubmissionsAddr = ":587", "off"
	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	if code := c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	var info map[string]string
	c.do("GET", "/api/mail-apps", nil, &info)
	if info["imap_ssl"] != "" || info["imap_starttls"] != "" || info["smtp_starttls"] != "587" || info["smtp_ssl"] != "" ||
		info["host"] != "mail.skien.test" || info["username"] != a.Email || info["tls_mode"] != "self-signed" {
		t.Fatalf("IMAP off: %v", info)
	}
	tlsAddr, plainAddr := startIMAPTest(t, app)
	_, p1, _ := net.SplitHostPort(tlsAddr)
	_, p2, _ := net.SplitHostPort(plainAddr)
	info = nil
	c.do("GET", "/api/mail-apps", nil, &info)
	if info["imap_ssl"] != p1 || info["imap_starttls"] != p2 {
		t.Fatalf("IMAP on: %v (want %s, %s)", info, p1, p2)
	}
	app.stopIMAP()
	info = nil
	c.do("GET", "/api/mail-apps", nil, &info)
	if info["imap_ssl"] != "" {
		t.Fatalf("after stop: %v", info)
	}
	// Signed out: refused.
	anon := newClient(t, srv.URL)
	if code := anon.do("GET", "/api/mail-apps", nil, nil); code != 401 {
		t.Fatalf("signed out: %d", code)
	}
}

// A session waiting in IDLE is told BYE and disconnected when the password changes, instead of
// idling on with an account it may no longer use.
func TestIMAPIdleEndsWhenSessionEnds(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	oldAlive, oldPoll := imapAliveEvery, imapIdlePoll
	imapAliveEvery, imapIdlePoll = 0, 100*time.Millisecond // set before the server starts
	t.Cleanup(func() { imapAliveEvery, imapIdlePoll = oldAlive, oldPoll })
	tlsAddr, _ := startIMAPTest(t, app)
	w := imapWireT(t, tlsAddr, a.Email, "password-asi")
	w.cmd("SELECT INBOX")
	fmt.Fprintf(w.conn, "i1 IDLE\r\n")
	if l := w.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("idle: %s", l)
	}
	// Still fine while nothing changes.
	time.Sleep(300 * time.Millisecond)
	app.DB.Exec(`UPDATE users SET password_changed_at=? WHERE id=?`, nowMs()+1000, a.ID)
	w.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var lines []string
	for {
		l, err := w.r.ReadString('\n')
		if l != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
		if err != nil {
			break // connection closed by the server
		}
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "* BYE") {
		t.Fatalf("expected BYE then disconnect, got %q", lines)
	}
}

// Review fixes (1.1.0): pre-sign-in limits, stale passwords, stale views, CLOSE, EXAMINE, memory.

// A client that never signs in is cut off after the pre-sign-in time even if it keeps sending
// NOOP, and only a few such connections are allowed per address (a signed-in one frees a slot).
func TestIMAPPreAuthLimits(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	oldT, oldN := imapPreAuthTimeout, imapMaxPreAuthPerIP
	imapPreAuthTimeout, imapMaxPreAuthPerIP = 700*time.Millisecond, 3 // set before the server starts
	t.Cleanup(func() { imapPreAuthTimeout, imapMaxPreAuthPerIP = oldT, oldN })
	tlsAddr, _ := startIMAPTest(t, app)
	dial := func() (net.Conn, *bufio.Reader, error) {
		c, err := tls.Dial("tcp", tlsAddr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return nil, nil, err
		}
		r := bufio.NewReader(c)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := r.ReadString('\n'); err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, r, nil
	}
	c, r, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; ; i++ {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprintf(c, "n%d NOOP\r\n", i)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("unauthenticated connection kept alive by NOOP")
		}
	}
	c.Close()
	time.Sleep(100 * time.Millisecond)
	// Three waiting connections from one address; the fourth is refused.
	var open []net.Conn
	for i := 0; i < 3; i++ {
		c, _, err := dial()
		if err != nil {
			t.Fatalf("connection %d refused: %v", i, err)
		}
		open = append(open, c)
	}
	if c, _, err := dial(); err == nil {
		c.Close()
		t.Fatal("fourth unauthenticated connection accepted")
	}
	// Signing one in frees its slot.
	w := &imapWire{t: t, conn: open[0], r: bufio.NewReader(open[0])}
	open[0].SetDeadline(time.Now().Add(5 * time.Second))
	if got := w.cmd(fmt.Sprintf("LOGIN %q %q", a.Email, "password-asi")); !strings.Contains(got, "OK") {
		t.Fatalf("login: %s", got)
	}
	c4, _, err := dial()
	if err != nil {
		t.Fatalf("slot not freed by sign-in: %v", err)
	}
	c4.Close()
	for _, c := range open {
		c.Close()
	}
}

// A phone retrying an old password over and over counts once: it can't lock the address out
// of the web app, and the right password still works from there at once.
func TestIMAPStalePasswordRetries(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	for i := 0; i < 60; i++ {
		c, err := imapclient.DialTLS(tlsAddr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Login(a.Email, "old-password"); err == nil {
			t.Fatal("wrong password accepted")
		}
		c.Terminate()
	}
	if !app.authBegin("127.0.0.1", a.Email) {
		t.Fatal("mail-app retries locked out web sign-in from the same address")
	}
	var logged int
	app.DB.QueryRow(`SELECT COUNT(*) FROM mail_log WHERE direction='auth' AND sender=?`, a.Email).Scan(&logged)
	if logged != 1 {
		t.Fatalf("the same failed sign-in was logged %d times", logged)
	}
	c := imapDialT(t, tlsAddr, a.Email, "password-asi") // fatal if refused
	c.Logout()
	// After a password change the old text is checked afresh (not remembered as "failed").
	if err := app.SetPassword(a.ID, "old-password", false); err != nil {
		t.Fatal(err)
	}
	imapDialT(t, tlsAddr, a.Email, "old-password")
}

// A mail app's stale view can't touch a newer message that got a deleted message's row id,
// and CLOSE only removes what is marked \Deleted at that moment.
func TestIMAPStaleViewAndClose(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	deliverTo(t, app, a, "inbox", "x@x.test", "OLD1")
	idX := deliverTo(t, app, a, "inbox", "x@x.test", "SPAMMY")
	w := imapWireT(t, tlsAddr, a.Email, "password-asi")
	w.cmd("SELECT INBOX") // view: uid 1 = OLD1, uid 2 = SPAMMY
	app.DB.Exec(`UPDATE messages SET folder='trash' WHERE id=?`, idX)
	if err := app.deleteMessages(`SELECT id, size, blob FROM messages WHERE id=?`, idX); err != nil {
		t.Fatal(err)
	}
	idNew := deliverTo(t, app, a, "inbox", "boss@x.test", "IMPORTANT NEW")
	if idNew != idX {
		t.Logf("row id not reused (%d vs %d); the check still applies", idNew, idX)
	}
	w.n++
	fmt.Fprintf(w.conn, "w%d UID FETCH 2 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])\r\n", w.n)
	for {
		l := w.line()
		if strings.Contains(l, "IMPORTANT NEW") {
			t.Fatal("stale UID returned the new message")
		}
		if strings.HasPrefix(l, fmt.Sprintf("w%d ", w.n)) {
			break
		}
	}
	w.cmd("UID STORE 2 +FLAGS (\\Flagged)")
	w.cmd("UID MOVE 2 Archive")
	var folder string
	var flagged bool
	app.DB.QueryRow(`SELECT folder, flagged FROM messages WHERE id=?`, idNew).Scan(&folder, &flagged)
	if folder != "inbox" || flagged {
		t.Fatalf("stale UID changed the new message: %s flagged=%v", folder, flagged)
	}

	// CLOSE after a \Deleted search and an un-delete removes nothing.
	t1 := deliverTo(t, app, a, "trash", "x@x.test", "T1")
	deliverTo(t, app, a, "trash", "x@x.test", "T2")
	w.cmd("SELECT Trash")
	w.cmd("STORE 1 +FLAGS (\\Deleted)")
	w.cmd("SEARCH DELETED")
	w.cmd("STORE 1 -FLAGS (\\Deleted)")
	w.cmd("CLOSE")
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, t1).Scan(&n)
	if n != 1 {
		t.Fatal("CLOSE deleted a message that was no longer marked \\Deleted")
	}
	// EXPUNGE reports exactly what it removed.
	w.cmd("SELECT Trash")
	w.cmd("STORE 2 +FLAGS (\\Deleted)")
	w.n++
	fmt.Fprintf(w.conn, "w%d EXPUNGE\r\n", w.n)
	var exp []string
	for {
		l := w.line()
		if strings.HasSuffix(l, " EXPUNGE") {
			exp = append(exp, l)
		}
		if strings.HasPrefix(l, fmt.Sprintf("w%d ", w.n)) {
			break
		}
	}
	if len(exp) != 1 || exp[0] != "* 2 EXPUNGE" {
		t.Fatalf("expunge reports: %q", exp)
	}
	// Nothing leaves a folder opened with EXAMINE.
	w.cmd("EXAMINE INBOX")
	if got := w.cmd("MOVE 1 Archive"); !strings.Contains(got, "NO") {
		t.Fatalf("MOVE after EXAMINE: %s", got)
	}
}

// Message uploads held in memory are capped across connections: a literal over the budget is
// refused before the server asks for it, and accepted again once memory is free.
func TestIMAPLiteralMemoryBudget(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	old := imapLiteralMemoryTest
	imapLiteralMemoryTest = 300 << 10
	t.Cleanup(func() { imapLiteralMemoryTest = old })
	tlsAddr, _ := startIMAPTest(t, app)
	w1 := imapWireT(t, tlsAddr, a.Email, "password-asi")
	w2 := imapWireT(t, tlsAddr, a.Email, "password-asi")
	size := 200 << 10
	// The first upload is granted and held (its data not sent yet).
	fmt.Fprintf(w1.conn, "u1 APPEND INBOX {%d}\r\n", size)
	if l := w1.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("first upload not granted: %s", l)
	}
	// The second doesn't fit and is refused without being asked for.
	fmt.Fprintf(w2.conn, "u2 APPEND INBOX {%d}\r\n", size)
	if l := w2.line(); !strings.HasPrefix(l, "u2 BAD [LIMIT]") {
		t.Fatalf("second upload: %s", l)
	}
	body := "Subject: big\r\n\r\n" + strings.Repeat("x", size-len("Subject: big\r\n\r\n"))
	io.WriteString(w1.conn, body+"\r\n")
	for {
		l := w1.line()
		if strings.HasPrefix(l, "u1 ") {
			if !strings.Contains(l, "OK") {
				t.Fatalf("first upload: %s", l)
			}
			break
		}
	}
	// Memory released: the second connection can upload now.
	if got := w2.cmd("APPEND INBOX", body); !strings.Contains(got, "OK") {
		t.Fatalf("upload after release: %s", got)
	}
	// Over the whole budget without waiting for permission (LITERAL+): disconnected, since its
	// data is already on the way.
	w3 := imapWireT(t, tlsAddr, a.Email, "password-asi")
	huge := 400 << 10
	fmt.Fprintf(w3.conn, "u3 APPEND INBOX {%d+}\r\n", huge)
	w3.conn.Write([]byte(strings.Repeat("y", 64<<10)))
	w3.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		l, err := w3.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("over-budget unasked literal: connection left open")
			}
			break // closed by the server: good
		}
		if strings.HasPrefix(l, "u3 OK") {
			t.Fatal("over-budget unasked literal accepted")
		}
	}
}

// An oversized upload sent without waiting (LITERAL+) ends the connection: its contents must
// never be read as commands.
func TestIMAPOversizedUnaskedLiteralIsNotRunAsCommands(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	tlsAddr, _ := startIMAPTest(t, app)
	deliverTo(t, app, a, "inbox", "x@x.test", "Keep me")
	w := imapWireT(t, tlsAddr, a.Email, "password-asi")
	evil := "evil1 SELECT INBOX\r\nevil2 STORE 1 +FLAGS (\\Deleted)\r\nevil3 EXPUNGE\r\n"
	fmt.Fprintf(w.conn, "a1 APPEND INBOX {%d+}\r\n%s", app.MaxMessageBytes()+(4<<20), evil)
	w.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		l, err := w.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("connection left open after an oversized unasked literal")
			}
			break
		}
		if strings.HasPrefix(l, "evil") {
			t.Fatalf("literal content ran as a command: %q", l)
		}
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder='inbox'`, a.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("inbox changed: %d", n)
	}
}

// One account can't take the whole upload budget, and a stalled upload gives its share back.
func TestIMAPLiteralBudgetPerAccountAndStall(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	o1, o2, o3 := imapLiteralMemoryTest, imapLiteralPerUserTest, imapLiteralGraceTest
	imapLiteralMemoryTest, imapLiteralPerUserTest, imapLiteralGraceTest = 1<<20, 300<<10, 500*time.Millisecond
	t.Cleanup(func() { imapLiteralMemoryTest, imapLiteralPerUserTest, imapLiteralGraceTest = o1, o2, o3 })
	tlsAddr, _ := startIMAPTest(t, app)
	a1 := imapWireT(t, tlsAddr, a.Email, "password-asi")
	a2 := imapWireT(t, tlsAddr, a.Email, "password-asi")
	b1 := imapWireT(t, tlsAddr, b.Email, "password-sajin")
	size := 200 << 10
	fmt.Fprintf(a1.conn, "s1 APPEND INBOX {%d}\r\n", size)
	if l := a1.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("first upload: %s", l)
	}
	// a1 now stalls. The same account can't start a second big upload...
	fmt.Fprintf(a2.conn, "s2 APPEND INBOX {%d}\r\n", size)
	if l := a2.line(); !strings.HasPrefix(l, "s2 BAD [LIMIT]") {
		t.Fatalf("second upload of the same account: %s", l)
	}
	// Nothing stray follows the refusal: the next reply is the next command's.
	fmt.Fprintf(a2.conn, "n1 NOOP\r\n")
	if l := a2.line(); !strings.HasPrefix(l, "n1 OK") {
		t.Fatalf("after a refused upload: %q", l)
	}
	// ...but another account can.
	fmt.Fprintf(b1.conn, "s3 APPEND INBOX {%d}\r\n", size)
	if l := b1.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("other account's upload: %s", l)
	}
	io.WriteString(b1.conn, "Subject: b\r\n\r\n"+strings.Repeat("b", size-len("Subject: b\r\n\r\n"))+"\r\n")
	for {
		if l := b1.line(); strings.HasPrefix(l, "s3 ") {
			if !strings.Contains(l, "OK") {
				t.Fatalf("other account's upload: %s", l)
			}
			break
		}
	}
	// The stalled upload is cut off after its time (0.5 s + 200 KB at 128 KB/s) and its share
	// comes back.
	a1.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := a1.r.ReadString('\n'); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled upload not cut off: %v", err)
	}
	waitFor(t, "budget released", func() bool {
		fmt.Fprintf(a2.conn, "s4 APPEND INBOX {%d}\r\n", size)
		l := a2.line()
		if strings.HasPrefix(l, "+") {
			io.WriteString(a2.conn, "Subject: a\r\n\r\n"+strings.Repeat("a", size-len("Subject: a\r\n\r\n"))+"\r\n")
			for !strings.HasPrefix(l, "s4 ") {
				l = a2.line()
			}
			return strings.Contains(l, "OK")
		}
		return false
	})
}

// Slow mail-app commands are logged with the account and the command (IDLE never is).
func TestIMAPSlowCommandLog(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	var buf syncBuffer
	app.Log = slog.New(slog.NewTextHandler(&buf, nil))
	old := imapSlowCommand
	imapSlowCommand = 0
	t.Cleanup(func() { imapSlowCommand = old })
	tlsAddr, _ := startIMAPTest(t, app)
	c := imapDialT(t, tlsAddr, a.Email, "password-asi")
	c.Select("INBOX", false)
	c.Noop()
	out := buf.String()
	if !strings.Contains(out, `msg="slow mail-app command" user=`+a.Email+` command=SELECT`) || !strings.Contains(out, "command=NOOP") {
		t.Fatalf("log: %s", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
