package core

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imapbackend "github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
)

// ---------------------------------------------------------------- helpers

func selfSignedLocal(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

type fakeIMAP struct {
	be   *imapbackend.Backend
	port int
}

func startFakeIMAP(t *testing.T, app *App) *fakeIMAP { return startFakeIMAPAt(t, app, "127.0.0.1:0") }

func startFakeIMAPAt(t *testing.T, app *App, addr string) *fakeIMAP {
	t.Helper()
	cert, pool := selfSignedLocal(t)
	be := imapbackend.New()
	s := imapserver.New(be)
	s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	l, err := tls.Listen("tcp", addr, s.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	app.imp.allowLocal = true
	app.imp.rootCAs = pool
	return &fakeIMAP{be: be, port: l.Addr().(*net.TCPAddr).Port}
}

func (f *fakeIMAP) add(t *testing.T, mailbox string, flags []string, date time.Time, raw string) {
	t.Helper()
	u, err := f.be.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := u.GetMailbox(mailbox)
	if err != nil {
		if err := u.CreateMailbox(mailbox); err != nil {
			t.Fatal(err)
		}
		mb, _ = u.GetMailbox(mailbox)
	}
	if err := mb.CreateMessage(flags, date, bytes.NewBufferString(raw)); err != nil {
		t.Fatal(err)
	}
}

func waitImport(t *testing.T, app *App, userID, id int64) ImportJob {
	t.Helper()
	return waitImportFor(t, app, userID, id, 20*time.Second)
}

func waitImportFor(t *testing.T, app *App, userID, id int64, wait time.Duration) ImportJob {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		for _, j := range app.ListImports(userID) {
			if j.ID == id && j.Status != "queued" && j.Status != "running" {
				return j
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("import did not finish")
	return ImportJob{}
}

type storedMsg struct {
	folder   string
	seen     bool
	flagged  bool
	received int64
}

func storedBySubject(t *testing.T, app *App, userID int64) map[string]storedMsg {
	t.Helper()
	rows, err := app.DB.Query(`SELECT subject, folder, seen, flagged, received_at FROM messages WHERE user_id=?`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]storedMsg{}
	for rows.Next() {
		var subj string
		var m storedMsg
		rows.Scan(&subj, &m.folder, &m.seen, &m.flagged, &m.received)
		out[subj] = m
	}
	return out
}

func eml(from, subject, msgid, body string) string {
	return fmt.Sprintf("From: %s\r\nTo: asi@skien.test\r\nSubject: %s\r\nDate: Mon, 02 Jun 2025 10:00:00 +0530\r\nMessage-ID: <%s>\r\n\r\n%s\r\n", from, subject, msgid, body)
}

// ---------------------------------------------------------------- IMAP

func TestIMAPImport(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	f := startFakeIMAP(t, app)
	old := time.Date(2024, 3, 5, 9, 30, 0, 0, time.UTC)
	f.add(t, "INBOX", []string{"\\Seen", "\\Flagged"}, old, eml("ravi@zoho.test", "Starred and read", "m1@zoho.test", "hello"))
	f.add(t, "INBOX", nil, old.Add(time.Hour), eml("ravi@zoho.test", "Unread one", "m2@zoho.test", "hi"))
	f.add(t, "Sent", []string{"\\Seen"}, old.Add(2*time.Hour), strings.Replace(eml("asi@skien.test", "Re: Starred and read", "m3@skien.test", "thanks"), "\r\n\r\n", "\r\nIn-Reply-To: <m1@zoho.test>\r\n\r\n", 1))
	f.add(t, "Projects/Alpha", nil, old.Add(3*time.Hour), eml("pm@zoho.test", "Alpha kickoff", "m4@zoho.test", "plan"))
	f.add(t, "Spam", nil, old, eml("x@spam.test", "Win money", "m5@spam.test", "spam"))
	f.add(t, "Drafts", nil, old, eml("asi@skien.test", "Draft", "m6@skien.test", "draft"))

	src := IMAPSource{Host: "127.0.0.1", Port: f.port, Security: "tls", Username: "username", Password: "password"}
	folders, err := app.IMAPFolders(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"INBOX": "inbox", "Sent": "sent", "Projects/Alpha": "keep", "Spam": "skip", "Drafts": "skip"}
	for _, fo := range folders {
		if w, ok := want[fo.Name]; ok && fo.Target != w {
			t.Fatalf("%s suggested %s, want %s", fo.Name, fo.Target, w)
		}
	}
	if folders[0].Name != "INBOX" || folders[1].Name != "Sent" {
		t.Fatalf("order: %v", folders)
	}

	job, err := app.ImportIMAP(a, src, folders)
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	// INBOX also holds the backend's sample message: 3 + Sent 1 + Alpha 1 = 5
	if done.Status != "done" || done.Imported != 5 || done.Failed != 0 || done.Total != 5 {
		t.Fatalf("job: %+v", done)
	}
	got := storedBySubject(t, app, a.ID)
	check := func(subj, folder string, seen, flagged bool) {
		t.Helper()
		m, ok := got[subj]
		if !ok || m.folder != folder || m.seen != seen || m.flagged != flagged {
			t.Fatalf("%q: %+v (want %s seen=%v flagged=%v)", subj, m, folder, seen, flagged)
		}
	}
	check("Starred and read", "inbox", true, true)
	check("Unread one", "inbox", false, false)
	check("Re: Starred and read", "sent", true, false)
	alpha, err := app.folderByName(a.ID, "Projects/Alpha")
	if err != nil {
		t.Fatalf("folder of the same name not created: %v", err)
	}
	if _, err := app.folderByName(a.ID, "Projects"); err != nil {
		t.Fatal("parent folder not created")
	}
	check("Alpha kickoff", alpha.Key, false, false)
	if _, ok := got["Win money"]; ok {
		t.Fatal("skipped folder imported")
	}
	if got["Starred and read"].received != old.UnixMilli() {
		t.Fatalf("received_at %d, want original %d", got["Starred and read"].received, old.UnixMilli())
	}
	// the reply threads with the original
	var t1, t2 int64
	app.DB.QueryRow(`SELECT thread_id FROM messages WHERE user_id=? AND subject='Starred and read'`, a.ID).Scan(&t1)
	app.DB.QueryRow(`SELECT thread_id FROM messages WHERE user_id=? AND subject='Re: Starred and read'`, a.ID).Scan(&t2)
	if t1 != t2 {
		t.Fatal("imported reply not threaded")
	}
	// searchable
	if rows, _, _ := app.ListThreads(a.ID, "inbox", "kickoff in:anywhere", 0, 10); len(rows) != 1 {
		t.Fatal("imported mail not searchable")
	}

	// Running it again stores nothing new.
	job2, err := app.ImportIMAP(a, src, folders)
	if err != nil {
		t.Fatal(err)
	}
	again := waitImport(t, app, a.ID, job2.ID)
	if again.Status != "done" || again.Imported != 0 || again.Skipped != 5 {
		t.Fatalf("re-run: %+v", again)
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, a.ID).Scan(&n)
	if n != 5 {
		t.Fatalf("%d messages after re-run", n)
	}
}

func TestIMAPImportErrors(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	f := startFakeIMAP(t, app)
	_, err := app.IMAPFolders(t.Context(), IMAPSource{Host: "127.0.0.1", Port: f.port, Username: "username", Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "app-specific password") {
		t.Fatalf("bad password: %v", err)
	}
	for _, s := range []IMAPSource{
		{Host: "", Port: 993, Username: "u", Password: "p"},
		{Host: "bad host", Port: 993, Username: "u", Password: "p"},
		{Host: "imap.test", Port: 993, Username: "", Password: "p"},
		{Host: "imap.test", Port: 993, Security: "plain", Username: "u", Password: "p"},
		{Host: "imap.test", Port: 993, Username: "u\r\nX", Password: "p"},
	} {
		if _, err := app.IMAPFolders(t.Context(), s); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	// Without the test hook, other ports and this machine are refused.
	app.imp.allowLocal = false
	if _, err := app.IMAPFolders(t.Context(), IMAPSource{Host: "127.0.0.1", Port: f.port, Username: "username", Password: "password"}); err == nil {
		t.Fatal("non-standard port accepted")
	}
	if _, err := app.IMAPFolders(t.Context(), IMAPSource{Host: "127.0.0.1", Port: 993, Username: "username", Password: "password"}); err == nil || !strings.Contains(err.Error(), "isn't allowed") {
		t.Fatalf("loopback not refused: %v", err)
	}
	if _, err := app.ImportIMAP(a, IMAPSource{Host: "imap.test", Port: 993, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "skip"}}); err == nil {
		t.Fatal("import with no folders accepted")
	}
}

// ---------------------------------------------------------------- files

func uploadFile(t *testing.T, app *App, userID int64, name string, data []byte) string {
	t.Helper()
	id, err := app.BeginImportUpload(userID, name, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); off += 7 { // small chunks on purpose
		end := min(off+7, len(data))
		if _, err := app.AppendImportUpload(userID, id, int64(off), bytes.NewReader(data[off:end])); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestZipImport(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name, content string) {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	put("Inbox/1.eml", eml("vendor@zoho.test", "Invoice March", "z1@zoho.test", "pay"))
	put("Sent/2.eml", eml("asi@skien.test", "Quote sent", "z2@skien.test", "quote"))
	put("3.eml", eml("asi@old-company.test", "From my old address", "z3@old.test", "x"))
	put("Spam/4.eml", eml("x@spam.test", "Cheap pills", "z4@spam.test", "spam"))
	put("__MACOSX/._1.eml", "junk")
	put("readme.txt", "not mail")
	put("broken.eml", "this is not an email")
	zw.Close()

	id := uploadFile(t, app, a.ID, "zoho-export.zip", buf.Bytes())
	job, err := app.ImportFile(a, FileImportOpts{UploadID: id, Folder: "auto", MarkRead: true, OldEmail: "asi@old-company.test"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "done" || done.Imported != 4 || done.Failed != 1 || done.Total != 5 {
		t.Fatalf("job: %+v", done)
	}
	got := storedBySubject(t, app, a.ID)
	for subj, folder := range map[string]string{"Invoice March": "inbox", "Quote sent": "sent", "From my old address": "sent", "Cheap pills": "spam"} {
		if got[subj].folder != folder || !got[subj].seen {
			t.Fatalf("%q: %+v, want %s and read", subj, got[subj], folder)
		}
	}
	if want := time.Date(2025, 6, 2, 4, 30, 0, 0, time.UTC).UnixMilli(); got["Invoice March"].received != want {
		t.Fatalf("received %d, want Date header %d", got["Invoice March"].received, want)
	}
	if _, err := os.Stat(app.uploadPath(id)); !os.IsNotExist(err) {
		t.Fatal("uploaded file left behind")
	}
	// Same file again: everything skipped.
	id2 := uploadFile(t, app, a.ID, "zoho-export.zip", buf.Bytes())
	job2, _ := app.ImportFile(a, FileImportOpts{UploadID: id2, Folder: "archive"})
	again := waitImport(t, app, a.ID, job2.ID)
	if again.Imported != 0 || again.Skipped != 4 {
		t.Fatalf("re-run: %+v", again)
	}
}

func TestMboxImportGmailLabels(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	mbox := "From 1234@xxx Mon Jun 02 10:00:00 +0000 2025\n" +
		"X-Gmail-Labels: Inbox,Starred,Unread\nFrom: a@gmail.test\nSubject: Labelled inbox\nMessage-ID: <g1@gmail.test>\n\nLine one\n>From the start of a line\n>>From quoted twice\n\n" +
		"From 1235@xxx Mon Jun 02 11:00:00 +0000 2025\n" +
		"X-Gmail-Labels: Sent,Opened\nFrom: asi@skien.test\nSubject: Labelled sent\nMessage-ID: <g2@gmail.test>\n\nbody\n\n" +
		"From 1236@xxx Mon Jun 02 12:00:00 +0000 2025\n" +
		"X-Gmail-Labels: Chat\nFrom: b@gmail.test\nSubject: A chat\n\nhey\n\n" +
		"From 1237@xxx Mon Jun 02 13:00:00 +0000 2025\n" +
		"X-Gmail-Labels: \"Project, Alpha\",Opened\nFrom: c@gmail.test\nSubject: Custom label\nMessage-ID: <g3@gmail.test>\n\nx\n"
	id := uploadFile(t, app, a.ID, "All mail Including Spam and Trash.mbox", []byte(mbox))
	job, err := app.ImportFile(a, FileImportOpts{UploadID: id, Folder: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "done" || done.Imported != 3 || done.Skipped != 1 || done.Total != 4 {
		t.Fatalf("job: %+v", done)
	}
	got := storedBySubject(t, app, a.ID)
	if m := got["Labelled inbox"]; m.folder != "inbox" || m.seen || !m.flagged {
		t.Fatalf("inbox: %+v", m)
	}
	if m := got["Labelled sent"]; m.folder != "sent" || !m.seen {
		t.Fatalf("sent: %+v", m)
	}
	f, err := app.folderByName(a.ID, "Project, Alpha")
	if m := got["Custom label"]; err != nil || m.folder != f.Key || !m.seen {
		t.Fatalf("custom label: %+v (%v)", m, err)
	}
	var blob string
	app.DB.QueryRow(`SELECT blob FROM messages WHERE user_id=? AND subject='Labelled inbox'`, a.ID).Scan(&blob)
	raw, _ := app.readBlob(blob)
	if !strings.Contains(string(raw), "\nFrom the start of a line\n>From quoted twice\n") || strings.HasSuffix(string(raw), "\n\n") {
		t.Fatalf("mbox quoting not undone: %q", raw)
	}
}

func TestReadMboxEdgeCases(t *testing.T) {
	// CRLF line endings, a body line starting with "From" but not after a blank line, and a
	// message far larger than the limit made of one enormous line.
	huge := strings.Repeat("x", 3<<20)
	in := "From a@b Mon Jun 02 10:00:00 2025\r\nSubject: one\r\n\r\nFrom here on, text\r\n\r\n" +
		"From a@b Mon Jun 02 10:00:01 2025\r\nSubject: big\r\n\r\n" + huge + "\r\n\r\n" +
		"From a@b Mon Jun 02 10:00:02 2025\r\nSubject: three\r\n\r\nend"
	var got []string
	tooBig := 0
	err := readMbox(t.Context(), strings.NewReader(in), 1<<20, func(raw []byte, big bool) error {
		if big {
			tooBig++
			return nil
		}
		got = append(got, string(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || tooBig != 1 {
		t.Fatalf("got %d messages, %d too big", len(got), tooBig)
	}
	if got[0] != "Subject: one\r\n\r\nFrom here on, text\r\n" || got[1] != "Subject: three\r\n\r\nend" {
		t.Fatalf("split wrong: %q", got)
	}
	n, _ := countMbox(t.Context(), strings.NewReader(in))
	if n != 3 {
		t.Fatalf("count %d", n)
	}
}

func TestFileImportRejectsNonMail(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	id := uploadFile(t, app, a.ID, "photo.jpg", []byte("\xff\xd8\xff\xe0 not an email at all"))
	job, _ := app.ImportFile(a, FileImportOpts{UploadID: id})
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "failed" || !strings.Contains(done.Message, "isn't an email export") {
		t.Fatalf("job: %+v", done)
	}
	var empty bytes.Buffer
	zw := zip.NewWriter(&empty)
	w, _ := zw.Create("notes.txt")
	w.Write([]byte("hello"))
	zw.Close()
	id = uploadFile(t, app, a.ID, "empty.zip", empty.Bytes())
	job, _ = app.ImportFile(a, FileImportOpts{UploadID: id})
	if done := waitImport(t, app, a.ID, job.ID); done.Status != "failed" || !strings.Contains(done.Message, "No emails were found") {
		t.Fatalf("zip without mail: %+v", done)
	}
}

func TestImportQuotaStops(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.DB.Exec(`UPDATE users SET quota_bytes=600 WHERE id=?`, a.ID)
	var mbox strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&mbox, "From x Mon Jun 02 10:00:00 2025\nFrom: v@zoho.test\nSubject: n%d\nMessage-ID: <q%d@zoho.test>\n\n%s\n\n", i, i, strings.Repeat("y", 100))
	}
	id := uploadFile(t, app, a.ID, "big.mbox", []byte(mbox.String()))
	job, _ := app.ImportFile(a, FileImportOpts{UploadID: id, Folder: "inbox"})
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "failed" || !strings.Contains(done.Message, "mailbox is full") || done.Imported == 0 || done.Imported >= 10 {
		t.Fatalf("job: %+v", done)
	}
	var used, quota int64
	app.DB.QueryRow(`SELECT used_bytes, quota_bytes FROM users WHERE id=?`, a.ID).Scan(&used, &quota)
	if used > quota {
		t.Fatalf("quota exceeded: %d > %d", used, quota)
	}
}

func TestImportUploadRules(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	if _, err := app.BeginImportUpload(a.ID, "x.zip", importUploadMax+1); err == nil {
		t.Fatal("oversize upload accepted")
	}
	id, err := app.BeginImportUpload(a.ID, "x.mbox", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AppendImportUpload(b.ID, id, 0, strings.NewReader("abc")); !os.IsNotExist(err) {
		t.Fatalf("another user wrote to the upload: %v", err)
	}
	if got, err := app.AppendImportUpload(a.ID, id, 5, strings.NewReader("abc")); err != errUploadOffset || got != 0 {
		t.Fatalf("wrong offset: %d %v", got, err)
	}
	if _, err := app.AppendImportUpload(a.ID, id, 0, strings.NewReader("0123456789AB")); err == nil {
		t.Fatal("more than the declared size accepted")
	}
	if got, err := app.AppendImportUpload(a.ID, id, 0, strings.NewReader("01234")); err != nil || got != 5 {
		t.Fatalf("append: %d %v", got, err)
	}
	if _, err := app.ImportFile(a, FileImportOpts{UploadID: id}); err == nil || !strings.Contains(err.Error(), "finished uploading") {
		t.Fatalf("incomplete upload imported: %v", err)
	}
	if _, err := app.ImportFile(b, FileImportOpts{UploadID: id}); err == nil {
		t.Fatal("another user imported the upload")
	}
	app.DiscardImportUpload(a.ID, id)
	if _, err := os.Stat(app.uploadPath(id)); !os.IsNotExist(err) {
		t.Fatal("discarded upload left on disk")
	}

	// HTTP: chunk endpoint enforces the session's ownership and offsets.
	id, _ = app.BeginImportUpload(a.ID, "y.eml", 4)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/import/uploads/"+id+"?offset=2", strings.NewReader("ab"))
	r.SetPathValue("id", id)
	app.handleImportUploadChunk(w, r, a)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"received":0`) {
		t.Fatalf("offset conflict: %d %s", w.Code, w.Body)
	}
}

func TestOneImportPerUser(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	f := startFakeIMAP(t, app)
	for i := 0; i < 300; i++ {
		f.add(t, "INBOX", nil, time.Now(), eml("v@zoho.test", fmt.Sprintf("bulk %d", i), fmt.Sprintf("b%d@zoho.test", i), "x"))
	}
	src := IMAPSource{Host: "127.0.0.1", Port: f.port, Username: "username", Password: "password"}
	job, err := app.ImportIMAP(a, src, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.ImportIMAP(a, src, []IMAPFolder{{Name: "INBOX", Target: "inbox"}}); err != errImportBusy {
		t.Fatalf("second concurrent import: %v", err)
	}
	if !app.CancelImport(a.ID, job.ID) {
		t.Fatal("cancel failed")
	}
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "cancelled" && done.Status != "done" {
		t.Fatalf("after cancel: %+v", done)
	}
}

// ---------------------------------------------------------------- hostile sources (review round)

// scriptedIMAP is a tiny TLS IMAP server that answers with whatever the test wants.
func scriptedIMAP(t *testing.T, app *App, exists string, fetchBody func(w net.Conn, tag string)) int {
	t.Helper()
	cert, pool := selfSignedLocal(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	app.imp.allowLocal = true
	app.imp.rootCAs = pool
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				fmt.Fprint(c, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				buf := make([]byte, 0, 4096)
				tmp := make([]byte, 4096)
				for {
					n, err := c.Read(tmp)
					if err != nil {
						return
					}
					buf = append(buf, tmp[:n]...)
					for {
						i := bytes.Index(buf, []byte("\r\n"))
						if i < 0 {
							break
						}
						line := string(buf[:i])
						buf = buf[i+2:]
						tag, rest, _ := strings.Cut(line, " ")
						cmd := strings.ToUpper(rest)
						switch {
						case strings.HasPrefix(cmd, "LOGIN"):
							fmt.Fprintf(c, "%s OK logged in\r\n", tag)
						case strings.HasPrefix(cmd, "CAPABILITY"):
							fmt.Fprintf(c, "* CAPABILITY IMAP4rev1\r\n%s OK done\r\n", tag)
						case strings.HasPrefix(cmd, "STATUS"):
							fmt.Fprintf(c, "* STATUS INBOX (MESSAGES 1)\r\n%s OK done\r\n", tag)
						case strings.HasPrefix(cmd, "EXAMINE"), strings.HasPrefix(cmd, "SELECT"):
							fmt.Fprintf(c, "* %s EXISTS\r\n* OK [UIDVALIDITY 1]\r\n%s OK [READ-ONLY] done\r\n", exists, tag)
						case strings.HasPrefix(cmd, "FETCH"):
							fmt.Fprintf(c, "* 1 FETCH (UID 1 FLAGS () INTERNALDATE \"01-Jan-2025 10:00:00 +0000\" RFC822.SIZE 100)\r\n%s OK done\r\n", tag)
						case strings.HasPrefix(cmd, "UID FETCH"):
							fetchBody(c, tag)
							return
						case strings.HasPrefix(cmd, "LOGOUT"):
							fmt.Fprintf(c, "* BYE\r\n%s OK done\r\n", tag)
							return
						default:
							fmt.Fprintf(c, "%s OK done\r\n", tag)
						}
					}
				}
			}(c)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestIMAPHostileExistsCount(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	port := scriptedIMAP(t, app, "4294967295", func(w net.Conn, tag string) {})
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "failed" || !strings.Contains(done.Message, "5 million") {
		t.Fatalf("job: %+v", done)
	}
}

func TestIMAPHostileLiteral(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	port := scriptedIMAP(t, app, "1", func(w net.Conn, tag string) {
		fmt.Fprint(w, "* 1 FETCH (UID 1 BODY[] {3000000000}\r\n")
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 2000; i++ { // keep sending until the client hangs up
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	var before, after runtimeMem
	before.read()
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	after.read()
	if done.Status != "failed" || !strings.Contains(done.Message, "larger than the import limit") {
		t.Fatalf("job: %+v", done)
	}
	if grew := int64(after.peak) - int64(before.peak); grew > 200<<20 {
		t.Fatalf("heap grew by %d MB", grew>>20)
	}
}

func TestIMAPReconnectsAfterDrops(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	app, _, a, _ := newTestApp(t, false)
	f := startFakeIMAP(t, app)
	for i := 0; i < 120; i++ {
		f.add(t, "INBOX", nil, time.Now(), eml("v@zoho.test", fmt.Sprintf("flaky %d", i), fmt.Sprintf("fl%d@zoho.test", i), strings.Repeat("body ", 400)))
	}
	// A proxy in front of the server: the 1st connection is cut part-way, the 2nd is refused,
	// later ones work.
	pl, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { pl.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			n := conns.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				if n == 2 {
					return
				}
				up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", f.port))
				if err != nil {
					return
				}
				defer up.Close()
				go func() { io.Copy(up, c) }()
				if n == 1 {
					io.CopyN(c, up, 60<<10)
					return
				}
				io.Copy(c, up)
			}(c)
		}
	}()
	src := IMAPSource{Host: "127.0.0.1", Port: pl.Addr().(*net.TCPAddr).Port, Username: "username", Password: "password"}
	if _, err := app.ImportIMAP(a, src, []IMAPFolder{{Name: "INBOX", Target: "inbox"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	var done ImportJob
	for time.Now().Before(deadline) {
		done = app.ListImports(a.ID)[0]
		if done.Status != "running" && done.Status != "queued" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if done.Status != "done" || done.Imported != 121 {
		t.Fatalf("job after drops: %+v (connections %d)", done, conns.Load())
	}
}

func TestUploadBusyAndDiscard(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	id, _ := app.BeginImportUpload(a.ID, "big.mbox", 100)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { app.AppendImportUpload(a.ID, id, 0, pr); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if got, err := app.AppendImportUpload(a.ID, id, 0, strings.NewReader("x")); err != errUploadBusy || got != 0 {
		t.Fatalf("busy: %d %v", got, err)
	}
	app.DiscardImportUpload(a.ID, id) // while busy: marked, removed when the write ends
	pw.Write([]byte("hello"))
	pw.Close()
	<-done
	if _, err := os.Stat(app.uploadPath(id)); !os.IsNotExist(err) {
		t.Fatal("discarded busy upload left on disk")
	}
	for i := 0; i < 3; i++ { // the slot is free again
		if _, err := app.BeginImportUpload(a.ID, "x.eml", 10); err != nil {
			t.Fatalf("upload %d refused: %v", i, err)
		}
	}
}

func TestQueuedFileImportCleansUpOnCancel(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.imp.slots <- struct{}{} // both server-wide slots busy
	app.imp.slots <- struct{}{}
	id := uploadFile(t, app, a.ID, "q.eml", []byte(eml("v@zoho.test", "queued", "q1@zoho.test", "x")))
	job, err := app.ImportFile(a, FileImportOpts{UploadID: id})
	if err != nil {
		t.Fatal(err)
	}
	if j := app.ListImports(a.ID)[0]; j.Status != "queued" {
		t.Fatalf("status %s", j.Status)
	}
	app.CancelImport(a.ID, job.ID)
	if j := waitImport(t, app, a.ID, job.ID); j.Status != "cancelled" {
		t.Fatalf("status %s", j.Status)
	}
	if _, err := os.Stat(app.uploadPath(id)); !os.IsNotExist(err) {
		t.Fatal("file of a cancelled queued import left on disk")
	}
	<-app.imp.slots
	<-app.imp.slots
}

func TestZipDirectoryLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, entries int) string {
		p := dir + "/" + name
		f, _ := os.Create(p)
		zw := zip.NewWriter(f)
		for i := 0; i < entries; i++ {
			w, _ := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%d.eml", i), Method: zip.Store})
			w.Write([]byte("x"))
		}
		zw.Close()
		f.Close()
		return p
	}
	open := func(p string, maxEntries int, maxDir int64) error {
		zr, c, err := openZipLimited(p, maxEntries, maxDir)
		if err == nil {
			if len(zr.File) == 0 {
				t.Fatalf("%s: no files", p)
			}
			c.Close()
		}
		return err
	}
	small := write("small.zip", 10)
	if err := open(small, 100, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := open(small, 5, 1<<20); err != errZipTooManyFiles {
		t.Fatalf("entry limit: %v", err)
	}
	big := write("zip64.zip", 70000) // > 65535 entries: zip64 end records
	if err := open(big, 100000, 64<<20); err != nil {
		t.Fatalf("zip64 within limits: %v", err)
	}
	if err := open(big, 60000, 64<<20); err != errZipTooManyFiles {
		t.Fatalf("zip64 entry limit: %v", err)
	}
	if err := open(big, 100000, 1<<20); err != errZipTooManyFiles { // directory ~4 MB
		t.Fatalf("directory size limit: %v", err)
	}
	// A prefix before the archive (self-extracting style) still opens.
	raw, _ := os.ReadFile(small)
	pre := dir + "/prefixed.zip"
	os.WriteFile(pre, append(bytes.Repeat([]byte{'#'}, 5000), raw...), 0o600)
	if err := open(pre, 100, 1<<20); err != nil {
		t.Fatalf("prefixed zip: %v", err)
	}
	junk := dir + "/junk.zip"
	os.WriteFile(junk, []byte("PK\x03\x04 not really"), 0o600)
	if err := open(junk, 100, 1<<20); err == nil {
		t.Fatal("junk accepted")
	}
}

func TestMboxScanStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	endless := io.MultiReader(strings.NewReader("From a Mon Jun 02 10:00:00 2025\n"), infiniteReader{})
	if _, err := countMbox(ctx, endless); err == nil {
		t.Fatal("countMbox ignored cancellation")
	}
	if err := readMbox(ctx, endless, 1<<20, func([]byte, bool) error { return nil }); err == nil {
		t.Fatal("readMbox ignored cancellation")
	}
}

type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type runtimeMem struct{ peak uint64 }

func (m *runtimeMem) read() {
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	m.peak = s.HeapSys
}
