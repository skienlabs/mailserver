package core

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the second QA round (backend edge cases).

func threadOf(t *testing.T, app *App, id int64) int64 {
	t.Helper()
	var tid int64
	if err := app.DB.QueryRow(`SELECT thread_id FROM messages WHERE id=?`, id).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	return tid
}

func TestMIMEBombWithSpaceBeforeColon(t *testing.T) {
	for _, sep := range []string{"Content-Type :", "Content-Type\t:", "content-TYPE  :"} {
		var b strings.Builder
		b.WriteString("From: x@example.test\r\nSubject: bomb\r\n")
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b, "%s multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", sep, i, i)
		}
		b.WriteString("Content-Type: text/plain\r\n\r\nhi\r\n")
		if !MIMETooComplex([]byte(b.String())) {
			t.Fatalf("bomb using %q not detected", sep)
		}
	}
}

func TestStripBccHeaderOnlyAndSpacedName(t *testing.T) {
	cases := []string{
		"From: a@skien.test\r\nTo: b@x.test\r\nBcc: secret@x.test\r\nSubject: hi\r\n",          // header-only, trailing CRLF
		"From: a@skien.test\r\nTo: b@x.test\r\nSubject: hi\r\nBcc: secret@x.test",              // header-only, no trailing newline
		"From: a@skien.test\r\nBcc : secret@x.test\r\nSubject: hi\r\n\r\nbody\r\n",             // space before colon
		"From: a@skien.test\nBCC:\tsecret@x.test,\n  other@x.test\nSubject: hi\n\nBcc: keep\n", // folded, LF only; body untouched
	}
	for i, c := range cases {
		out := string(stripHeader([]byte(c), "Bcc"))
		if strings.Contains(out, "secret@") || strings.Contains(out, "other@") {
			t.Fatalf("case %d: Bcc leaked: %q", i, out)
		}
		if !strings.Contains(out, "Subject: hi") || !strings.Contains(out, "From: a@skien.test") {
			t.Fatalf("case %d: other headers lost: %q", i, out)
		}
	}
	if out := string(stripHeader([]byte(cases[3]), "Bcc")); !strings.HasSuffix(out, "\n\nBcc: keep\n") {
		t.Fatalf("body changed: %q", out)
	}
	if !hasHeader([]byte("From: a\r\nmessage-id : <x>\r\n\r\nDate: no"), "Message-ID") || hasHeader([]byte("From: a\r\n\r\nDate: body"), "Date") {
		t.Fatal("hasHeader misreads the header block")
	}
}

func TestDeleteUserLeavesNoSearchRows(t *testing.T) {
	app, d, _, _ := newTestApp(t, false)
	for round := 0; round < 5; round++ {
		u, err := app.CreateUser(NewUser{Name: "Tmp", Local: fmt.Sprintf("tmp%d", round), DomainID: d.ID, Password: "password-tmp"})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				raw := fmt.Sprintf("From: x@example.test\r\nTo: %s\r\nSubject: zebra %d\r\nMessage-ID: <z%d.%d@x>\r\n\r\nzebra\r\n", u.Email, i, round, i)
				app.Deliver(u.ID, []byte(raw), DeliverOpts{})
			}(i)
		}
		time.Sleep(time.Millisecond)
		if err := app.DeleteUser(u.ID); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		// Messages delivered after the delete fail (no such user) or are cascaded away;
		// either way the index must match the table.
		var fts, msgs int
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&fts)
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs)
		if fts != msgs {
			t.Fatalf("round %d: %d search rows for %d messages", round, fts, msgs)
		}
	}
}

func TestSearchOddQueries(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.Deliver(a.ID, []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: hello\r\nMessage-ID: <s1@x>\r\n\r\nworld\r\n"), DeliverOpts{})
	for _, q := range []string{"in:anywhere", "in:anywhere is:bogus", "is:bogus", "\x00", "hel\x00lo", `"`, `*`, `"*"`, `-`, `NEAR(`, `a"b`, `^`, `:`, `in:`, `from:`, "​", `''`, `AND OR NOT`} {
		if _, _, err := app.ListThreads(a.ID, "inbox", q, 0, 50); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	rows, _, _ := app.ListThreads(a.ID, "inbox", "in:anywhere", 0, 50)
	if len(rows) != 1 {
		t.Fatalf("in:anywhere returned %d conversations", len(rows))
	}
	if _, _, err := app.ListThreads(a.ID, "bogus", "", 0, 50); err != errUnknownFolder {
		t.Fatalf("unknown folder: %v", err)
	}
}

func TestAdminSettingsValidation(t *testing.T) {
	app, _, admin, _ := newTestApp(t, false)
	try := func(body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		app.handleAdminSetSettings(w, r, admin)
		return w.Code
	}
	bad := []string{
		`{"spam_threshold":"NaN"}`, `{"spam_threshold":"Inf"}`, `{"spam_threshold":"0.5"}`,
		`{"relay_port":"0"}`, `{"relay_port":"99999"}`,
		`{"relay_host":"smtp.relay.test:2525"}`, `{"relay_host":"not a host"}`,
		`{"default_quota_mb":"999999999999"}`, `{"trash_days":"100000"}`,
	}
	for _, b := range bad {
		if c := try(b); c != http.StatusBadRequest {
			t.Fatalf("%s accepted (%d)", b, c)
		}
	}
	for _, b := range []string{`{"spam_threshold":"6.5"}`, `{"relay_host":"smtp-relay.gmail.com","relay_port":"587"}`, `{"relay_host":""}`, `{"relay_host":"192.0.2.10"}`,
		`{"relay_host":"postfix"}`, `{"relay_host":"smtp_relay.internal"}`, `{"relay_host":"[2001:db8::1]"}`} {
		if c := try(b); c != http.StatusOK {
			t.Fatalf("%s rejected (%d)", b, c)
		}
	}
	if quotaBytes(1<<53) <= 0 || quotaBytes(-5) != 0 {
		t.Fatal("quota conversion overflows or accepts negatives")
	}
}

func TestWireAddress(t *testing.T) {
	ok := map[string]string{
		"ola@bølle.no":          "ola@xn--blle-gra.no",
		"a.b+tag@example.org":   "a.b+tag@example.org",
		"x@пример.рф":           "x@xn--e1afmkfd.xn--p1ai",
		"o'brien@example.co.uk": "o'brien@example.co.uk",
	}
	for in, want := range ok {
		got, err := wireAddress(in)
		if err != nil || got != want {
			t.Fatalf("%s → %q, %v (want %s)", in, got, err, want)
		}
	}
	for _, in := range []string{"john doe@example.org", `"x"@example.org`, "carol@skien.test@example.org", ".a@example.org", "a..b@example.org", "a@localhost", "a@[192.0.2.1]", "æ@example.org"} {
		if got, err := wireAddress(in); err == nil {
			t.Fatalf("%q accepted as %q", in, got)
		}
	}
	if _, err := normalizeAddrs([]Addr{{Email: `"john doe"@example.org`}}); err == nil {
		t.Fatal("quoted local part accepted by normalizeAddrs")
	}
}

func TestQuotaHoldsUnderParallelDelivery(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.DB.Exec(`UPDATE users SET quota_bytes=10000 WHERE id=?`, a.ID)
	body := strings.Repeat("x", 900)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw := fmt.Sprintf("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: q%d\r\nMessage-ID: <q%d@x>\r\n\r\n%s\r\n", i, i, body)
			app.Deliver(a.ID, []byte(raw), DeliverOpts{EnforceQuota: true})
		}(i)
	}
	wg.Wait()
	var used, sum int64
	app.DB.QueryRow(`SELECT used_bytes FROM users WHERE id=?`, a.ID).Scan(&used)
	app.DB.QueryRow(`SELECT COALESCE(SUM(size),0) FROM messages WHERE user_id=?`, a.ID).Scan(&sum)
	if used > 10000 || used != sum {
		t.Fatalf("used_bytes %d (sum %d) with a 10000-byte quota", used, sum)
	}
	var msgs int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, a.ID).Scan(&msgs)
	if n := countFiles(t, app.blobPath("")); n != msgs {
		t.Fatalf("%d files for %d messages (refused deliveries must not leave files)", n, msgs)
	}
}

func TestThreadingTagsAndSiblings(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	deliver := func(raw string, folder string) int64 {
		id, err := app.Deliver(a.ID, []byte(raw), DeliverOpts{Folder: folder})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	root := deliver("From: asi@skien.test\r\nTo: v@partner.test\r\nSubject: Budget\r\nMessage-ID: <b0@skien.test>\r\n\r\nplan\r\n", "sent")
	r1 := deliver("From: v@partner.test\r\nTo: asi@skien.test\r\nSubject: [EXTERNAL] Re: Budget\r\nMessage-ID: <b1@partner.test>\r\nIn-Reply-To: <b0@skien.test>\r\nReferences: <b0@skien.test>\r\n\r\nok\r\n", "")
	if threadOf(t, app, r1) != threadOf(t, app, root) {
		t.Fatal("[EXTERNAL] reply did not join its conversation")
	}
	// The newest referenced message has a different subject, an older one matches: still joins.
	other := deliver("From: v@partner.test\r\nTo: asi@skien.test\r\nSubject: Something else\r\nMessage-ID: <o1@partner.test>\r\n\r\nx\r\n", "")
	_ = other
	r2 := deliver("From: v@partner.test\r\nTo: asi@skien.test\r\nSubject: Re: Budget\r\nMessage-ID: <b2@partner.test>\r\nReferences: <b0@skien.test> <o1@partner.test>\r\n\r\nmore\r\n", "")
	if threadOf(t, app, r2) != threadOf(t, app, root) {
		t.Fatal("reply did not join via an older reference")
	}
	// Two replies arriving before their parent end up together.
	s1 := deliver("From: p@partner.test\r\nTo: asi@skien.test\r\nSubject: Re: Lunch\r\nMessage-ID: <l1@partner.test>\r\nIn-Reply-To: <l0@partner.test>\r\n\r\nyes\r\n", "")
	s2 := deliver("From: q@partner.test\r\nTo: asi@skien.test\r\nSubject: Re: Lunch\r\nMessage-ID: <l2@partner.test>\r\nIn-Reply-To: <l0@partner.test>\r\n\r\nme too\r\n", "")
	if threadOf(t, app, s1) != threadOf(t, app, s2) {
		t.Fatal("sibling replies split into two conversations")
	}
	// Injection is still blocked: a matching reference with an unrelated subject doesn't join.
	evil := deliver("From: e@bad.test\r\nTo: asi@skien.test\r\nSubject: [EXTERNAL] New bank details\r\nMessage-ID: <e1@bad.test>\r\nIn-Reply-To: <b0@skien.test>\r\n\r\npay\r\n", "")
	if threadOf(t, app, evil) == threadOf(t, app, root) {
		t.Fatal("unrelated subject joined the conversation")
	}
	if normSubject("[ext] RE: [team] Fwd: Budget") != "budget" || normSubject("[only a tag]") != "[only a tag]" {
		t.Fatalf("normSubject: %q / %q", normSubject("[ext] RE: [team] Fwd: Budget"), normSubject("[only a tag]"))
	}
}

func TestDuplicateAcrossFoldersAndUndoByID(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	raw := []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: dup\r\nMessage-ID: <d1@x>\r\n\r\nbody\r\n")
	id, _ := app.Deliver(a.ID, raw, DeliverOpts{})
	tid := threadOf(t, app, id)
	if err := app.ThreadAction(a.ID, []int64{tid}, "inbox", "archive"); err != nil {
		t.Fatal(err)
	}
	if again, _ := app.Deliver(a.ID, raw, DeliverOpts{}); again != id {
		t.Fatal("a retry after archiving stored a second copy")
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, a.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d copies stored", n)
	}

	// Undo of a delete only restores what that delete moved.
	m1, _ := app.Deliver(a.ID, []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: Plan\r\nMessage-ID: <p1@x>\r\n\r\none\r\n"), DeliverOpts{})
	pt := threadOf(t, app, m1)
	app.ThreadAction(a.ID, []int64{pt}, "inbox", "trash") // deliberately deleted earlier
	m2, _ := app.Deliver(a.ID, []byte("From: x@example.test\r\nTo: asi@skien.test\r\nSubject: Re: Plan\r\nMessage-ID: <p2@x>\r\nIn-Reply-To: <p1@x>\r\n\r\ntwo\r\n"), DeliverOpts{})
	if threadOf(t, app, m2) != pt {
		t.Fatal("reply not threaded")
	}
	moved, err := app.ThreadActionIDs(a.ID, []int64{pt}, "inbox", "trash", nil)
	if err != nil || len(moved) != 1 || moved[0] != m2 {
		t.Fatalf("moved %v, %v", moved, err)
	}
	if _, err := app.ThreadActionIDs(a.ID, []int64{pt}, "trash", "inbox", moved); err != nil {
		t.Fatal(err)
	}
	folder := func(id int64) (f string) {
		app.DB.QueryRow(`SELECT folder FROM messages WHERE id=?`, id).Scan(&f)
		return
	}
	if folder(m2) != "inbox" || folder(m1) != "trash" {
		t.Fatalf("after undo: m1=%s m2=%s (want trash, inbox)", folder(m1), folder(m2))
	}
	// An empty ID list (nothing moved) restores nothing.
	if moved, _ := app.ThreadActionIDs(a.ID, []int64{pt}, "trash", "inbox", []int64{}); len(moved) != 0 || folder(m1) != "trash" {
		t.Fatal("empty ID list restored messages")
	}
}

func TestUploadNameTruncationIsValidUTF8(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	name := strings.Repeat("ø", 150) + ".pdf\r\nX: y"
	res, err := app.saveUpload(a.ID, name, "application/pdf", []byte("%PDF-1.4"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := res["name"].(string)
	if !bytes.Equal([]byte(strings.ToValidUTF8(got, "�")), []byte(got)) || strings.ContainsAny(got, "\r\n") || len(got) > 200 {
		t.Fatalf("bad stored name %q", got)
	}
}

func TestTrashedSentCopyDoesNotBlockIncomingCopy(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	raw := []byte("From: asi@skien.test\r\nTo: list@lists.test\r\nSubject: My post\r\nMessage-ID: <post1@skien.test>\r\n\r\nhello list\r\n")
	sent, _ := app.Deliver(a.ID, raw, DeliverOpts{Folder: "sent"})
	app.ThreadAction(a.ID, []int64{threadOf(t, app, sent)}, "sent", "trash")
	in, err := app.Deliver(a.ID, raw, DeliverOpts{})
	if err != nil || in == sent {
		t.Fatalf("list copy of my own post was dropped (id %d, sent %d, %v)", in, sent, err)
	}
}

func TestUploadNameOnlyControlChars(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	for _, n := range []string{"\x01", "\t", "\r\n", ""} {
		res, err := app.saveUpload(a.ID, n, "", []byte("x"))
		if err != nil || res["name"] != "attachment" {
			t.Fatalf("%q stored as %q (%v)", n, res["name"], err)
		}
	}
}
