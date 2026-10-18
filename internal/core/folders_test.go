package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

func deliverTo(t *testing.T, app *App, u *User, folder, from, subject string) int64 {
	t.Helper()
	raw := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@x.test>\r\nDate: Mon, 02 Jun 2025 10:00:00 +0000\r\n\r\nbody of %s\r\n", from, u.Email, subject, RandomHex(8), subject))
	id, err := app.Deliver(u.ID, raw, DeliverOpts{Folder: folder})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func folderOf(t *testing.T, app *App, id int64) string {
	t.Helper()
	var f string
	if err := app.DB.QueryRow(`SELECT folder FROM messages WHERE id=?`, id).Scan(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFolderNames(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	for _, bad := range []string{"", " ", "/", "a//b", "a/", "/a", "Inbox", "inbox", "SPAM", "Sent/2024", "INBOX/x", "Trash", "All mail", "a%b", "a*b", `a\b`, "a\x01b", `a"b`, strings.Repeat("x", 201), "a/b/c/d/e/f/g/h/i"} {
		if _, err := app.CreateFolder(a.ID, bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	f, err := app.CreateFolder(a.ID, "  Clients /  Acme   Corp ")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "Clients/Acme Corp" || f.Leaf != "Acme Corp" || f.Depth != 1 {
		t.Fatalf("cleaned: %+v", f)
	}
	if _, err := app.folderByName(a.ID, "clients"); err != nil {
		t.Fatal("parent not created")
	}
	if _, err := app.CreateFolder(a.ID, "CLIENTS/acme corp"); err != errFolderTaken {
		t.Fatalf("duplicate (case-insensitive): %v", err)
	}
	if _, err := app.CreateFolder(a.ID, "Clients"); err != errFolderTaken {
		t.Fatalf("existing parent: %v", err)
	}
	list, _ := app.ListFolders(a.ID)
	if len(list) != 2 || list[0].Name != "Clients" || list[1].Name != "Clients/Acme Corp" {
		t.Fatalf("tree order: %+v", list)
	}
}

func TestFolderMoveListArchiveRestore(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	f, _ := app.CreateFolder(a.ID, "Projects")
	m1 := deliverTo(t, app, a, "inbox", "pm@x.test", "Alpha plan")
	m2 := deliverTo(t, app, a, "inbox", "pm@x.test", "Beta plan")
	sent := deliverTo(t, app, a, "sent", a.Email, "My report")

	if _, err := app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m1), threadOf(t, app, sent)}, "all", "move:"+f.Key, nil); err != nil {
		t.Fatal(err)
	}
	if folderOf(t, app, m1) != f.Key || folderOf(t, app, sent) != f.Key || folderOf(t, app, m2) != "inbox" {
		t.Fatal("move to folder")
	}
	// another user's folder key is not a valid target
	other, _ := app.CreateFolder(b.ID, "Mine")
	if _, err := app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, "inbox", "move:"+other.Key, nil); err == nil {
		t.Fatal("moved into another user's folder")
	}
	if _, err := app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, "inbox", "move:sent", nil); err == nil {
		t.Fatal("move to Sent allowed")
	}
	rows, total, err := app.ListThreads(a.ID, f.Key, "", 0, 50)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("folder list: %d %v", total, err)
	}
	if _, total, _ := app.ListThreads(a.ID, "all", "", 0, 50); total != 3 {
		t.Fatalf("All mail count includes custom folders: %d", total)
	}
	if rows, _, _ := app.ListThreads(a.ID, "inbox", "in:"+f.Key+" alpha", 0, 50); len(rows) != 1 {
		t.Fatal("search in folder")
	}
	if c := app.FolderCounts(a.ID); c[f.Key] != 2 { // both unread in this test
		t.Fatalf("unread count for folder: %v", c)
	}
	// Archive from the folder view moves that folder's messages.
	if _, err := app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m1)}, f.Key, "archive", nil); err != nil || folderOf(t, app, m1) != "archive" {
		t.Fatalf("archive from folder: %v %s", err, folderOf(t, app, m1))
	}
	// Sent mail filed in a folder goes back to Sent on "Move to Inbox".
	if _, err := app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, sent)}, f.Key, "inbox", nil); err != nil || folderOf(t, app, sent) != "sent" {
		t.Fatalf("filed sent mail restored to %s", folderOf(t, app, sent))
	}
	// Trash from a folder and restore puts it back in the folder.
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, "inbox", "move:"+f.Key, nil)
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, f.Key, "trash", nil)
	if folderOf(t, app, m2) != "trash" {
		t.Fatal("trash")
	}
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, "trash", "inbox", nil)
	if folderOf(t, app, m2) != f.Key {
		t.Fatalf("restored to %s, want the folder", folderOf(t, app, m2))
	}
	// Move out of Trash straight into a folder clears the trash clock.
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, f.Key, "trash", nil)
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m2)}, "trash", "move:archive", nil)
	var trashedAt int64
	app.DB.QueryRow(`SELECT trashed_at FROM messages WHERE id=?`, m2).Scan(&trashedAt)
	if folderOf(t, app, m2) != "archive" || trashedAt != 0 {
		t.Fatal("move out of trash")
	}
}

func TestFolderRenameDelete(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	acme, _ := app.CreateFolder(a.ID, "Clients/Acme")
	clients, _ := app.folderByName(a.ID, "Clients")
	other, _ := app.CreateFolder(a.ID, "Other")
	m := deliverTo(t, app, a, "inbox", "x@acme.test", "Acme invoice")
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m)}, "inbox", "move:"+acme.Key, nil)
	fl, err := app.CreateFilter(a.ID, Filter{From: "acme.test", Folder: acme.Key})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := app.RenameFolder(a.ID, clients.Key, "Clients/Sub"); err == nil {
		t.Fatal("moved inside itself")
	}
	if _, err := app.RenameFolder(a.ID, clients.Key, "other"); err != errFolderTaken {
		t.Fatalf("rename onto existing: %v", err)
	}
	if _, err := app.RenameFolder(a.ID, clients.Key, "Customers"); err != nil {
		t.Fatal(err)
	}
	if f, _ := app.folderByKey(a.ID, acme.Key); f.Name != "Customers/Acme" {
		t.Fatalf("child not renamed: %q", f.Name)
	}
	if folderOf(t, app, m) != acme.Key {
		t.Fatal("messages must stay in the renamed folder")
	}
	// Move a folder under another one.
	if _, err := app.RenameFolder(a.ID, acme.Key, "Other/Acme"); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteFolder(a.ID, other.Key); err != nil {
		t.Fatal(err)
	}
	if folderOf(t, app, m) != "archive" {
		t.Fatal("mail of a deleted folder must go to Archive")
	}
	if _, err := app.folderByKey(a.ID, acme.Key); err == nil {
		t.Fatal("subfolder not deleted with its parent")
	}
	list, _ := app.ListFilters(a.ID)
	if len(list) != 1 || list[0].ID != fl.ID || list[0].Folder != "" {
		t.Fatalf("filter still points at the deleted folder: %+v", list)
	}
	if err := app.DeleteFolder(a.ID, acme.Key); err != errNoFolder {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestFiltersOnDelivery(t *testing.T) {
	app, _, a, b := newTestApp(t, true)
	news, _ := app.CreateFolder(a.ID, "Newsletters")
	bills, _ := app.CreateFolder(a.ID, "Bills")
	mustFilter := func(f Filter) Filter {
		t.Helper()
		out, err := app.CreateFilter(a.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	mustFilter(Filter{From: "deals@shop.test, offers@shop.test", Folder: news.Key, MarkRead: true})
	mustFilter(Filter{Subject: "invoice", Folder: bills.Key, Star: true})
	mustFilter(Filter{Subject: "invoice", Folder: "archive"}) // second folder action: the first wins
	off := mustFilter(Filter{From: "boss@corp.test", Folder: "trash"})
	off.Enabled = false
	if _, err := app.UpdateFilter(a.ID, off); err != nil {
		t.Fatal(err)
	}
	mustFilter(Filter{Words: "unsubscribe here", MarkRead: true})

	send := func(from, subject, body string) {
		t.Helper()
		msg := fmt.Sprintf("From: %s\r\nTo: asi@skien.test\r\nSubject: %s\r\nMessage-ID: <%s@x.test>\r\nDate: Mon, 02 Jun 2025 10:00:00 +0000\r\n\r\n%s\r\n", from, subject, RandomHex(8), body)
		env := from
		if i := strings.Index(from, "<"); i >= 0 {
			env = strings.Trim(from[i:], "<>")
		}
		if err := sendSMTP(app.Cfg.SMTPAddr, env, []string{"asi@skien.test"}, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	send("Offers <OFFERS@shop.test>", "Big sale", "hello")
	send("acc@vendor.test", "Your INVOICE for May", "pay")
	send("boss@corp.test", "Meeting", "see you")
	send("list@news.test", "Weekly", "text ... Unsubscribe   here ...")
	waitFor(t, "deliveries", func() bool {
		var n int
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, a.ID).Scan(&n)
		return n == 4
	})
	got := storedBySubject(t, app, a.ID)
	if m := got["Big sale"]; m.folder != news.Key || !m.seen {
		t.Fatalf("from + alternatives: %+v", m)
	}
	if m := got["Your INVOICE for May"]; m.folder != bills.Key || !m.flagged || m.seen {
		t.Fatalf("subject: %+v", m)
	}
	if m := got["Meeting"]; m.folder != "inbox" {
		t.Fatalf("disabled filter applied: %+v", m)
	}
	if m := got["Weekly"]; m.folder != "inbox" || !m.seen {
		t.Fatalf("words: %+v", m)
	}
	list, _ := app.ListFilters(a.ID)
	if list[0].Hits != 1 || list[1].Hits != 1 {
		t.Fatalf("hit counters: %+v", list)
	}
	// Filters belong to their owner: Sajin's copy of the same message isn't affected.
	msg := "From: offers@shop.test\r\nTo: sajin@skien.test\r\nSubject: Sale for Sajin\r\nMessage-ID: <s1@x.test>\r\n\r\nx\r\n"
	sendSMTP(app.Cfg.SMTPAddr, "offers@shop.test", []string{"sajin@skien.test"}, []byte(msg))
	waitFor(t, "sajin's copy", func() bool {
		var f string
		app.DB.QueryRow(`SELECT folder FROM messages WHERE user_id=? AND subject='Sale for Sajin'`, b.ID).Scan(&f)
		return f == "inbox"
	})
	// Internal mail is filtered too.
	mustFilter(Filter{From: "sajin@skien.test", Folder: "archive"})
	raw := []byte("From: sajin@skien.test\r\nTo: asi@skien.test\r\nSubject: Internal note\r\nMessage-ID: <int1@skien.test>\r\n\r\nhi\r\n")
	if err := app.Submit(SubmitReq{User: b, EnvFrom: b.Email, Raw: raw, Rcpts: []string{a.Email}, Via: "web"}); err != nil {
		t.Fatal(err)
	}
	if m := storedBySubject(t, app, a.ID)["Internal note"]; m.folder != "archive" {
		t.Fatalf("internal mail: %+v", m)
	}
}

func TestFilterValidationAndApply(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	for _, bad := range []Filter{{}, {From: "x"}, {From: " , ,", MarkRead: true}, {From: "x", Folder: "f:999"}, {From: "x", Folder: "sent"}, {From: strings.Repeat("x", 501), Star: true}} {
		if _, err := app.CreateFilter(a.ID, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	bf, _ := app.CreateFolder(b.ID, "B's")
	if _, err := app.CreateFilter(a.ID, Filter{From: "x", Folder: bf.Key}); err == nil {
		t.Fatal("filter into another user's folder")
	}
	f, _ := app.CreateFolder(a.ID, "Vendors")
	m1 := deliverTo(t, app, a, "inbox", "billing@vendor.test", "May bill")
	m2 := deliverTo(t, app, a, "archive", "billing@vendor.test", "April bill")
	m3 := deliverTo(t, app, a, "trash", "billing@vendor.test", "Old bill")
	m4 := deliverTo(t, app, a, "inbox", "friend@x.test", "Hello")
	fl, err := app.CreateFilter(a.ID, Filter{From: "vendor.test", Folder: f.Key, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := app.ApplyFilter(a.ID, fl.ID)
	if err != nil || n != 2 {
		t.Fatalf("applied to %d (%v), want 2", n, err)
	}
	if folderOf(t, app, m1) != f.Key || folderOf(t, app, m2) != f.Key || folderOf(t, app, m3) != "trash" || folderOf(t, app, m4) != "inbox" {
		t.Fatal("apply to existing mail")
	}
	// Words go through the full-text index and are then checked exactly.
	w, _ := app.CreateFilter(a.ID, Filter{Words: "body of hello", Star: true})
	if n, err := app.ApplyFilter(a.ID, w.ID); err != nil || n != 1 {
		t.Fatalf("words applied to %d (%v)", n, err)
	}
	if _, err := app.ApplyFilter(b.ID, fl.ID); err == nil {
		t.Fatal("applied someone else's filter")
	}
}

func TestSenderLists(t *testing.T) {
	app, _, a, b := newTestApp(t, true)
	for _, bad := range []string{"", "nodot", "@", "x@", "@bad_domain", "a b@c.test"} {
		if _, err := app.SetSender(a.ID, bad, "block"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := app.SetSender(a.ID, a.Email, "block"); err == nil {
		t.Fatal("blocked own address")
	}
	app.SetSender(a.ID, "spam.test", "block")           // domain without @
	app.SetSender(a.ID, "friend@spam.test", "allow")    // an exception inside it
	app.SetSender(a.ID, "<News@Letters.test>", "allow") // normalised
	app.SetSender(a.ID, "@sub.allowed.test", "allow")
	if v := app.senderVerdict(a.ID, "x@spam.test"); v != "block" {
		t.Fatalf("domain: %q", v)
	}
	if v := app.senderVerdict(a.ID, "x@mail.spam.test"); v != "block" {
		t.Fatalf("subdomain: %q", v)
	}
	if k, p := app.senderMatch(a.ID, "x@mail.spam.test"); k != "block" || p != "@spam.test" {
		t.Fatalf("deciding entry: %q %q", k, p)
	}
	if k, p := app.senderMatch(a.ID, "friend@spam.test"); k != "allow" || p != "friend@spam.test" {
		t.Fatalf("deciding entry for the exception: %q %q", k, p)
	}
	if v := app.senderVerdict(a.ID, "x@notspam.test"); v != "" {
		t.Fatalf("suffix that isn't a subdomain: %q", v)
	}
	if v := app.senderVerdict(a.ID, "FRIEND@spam.test"); v != "allow" {
		t.Fatalf("exact address wins over domain: %q", v)
	}
	if v := app.senderVerdict(b.ID, "x@spam.test"); v != "" {
		t.Fatal("lists are per user")
	}
	list, _ := app.ListSenders(a.ID)
	if len(list) != 4 || list[0].Kind != "allow" {
		t.Fatalf("list: %+v", list)
	}

	// Delivery: blocked → Spam (no auto-reply), allowed → Inbox despite a high spam score.
	app.DB.Exec(`UPDATE users SET vacation_enabled=1, vacation_body=? WHERE id=?`, "away", a.ID)
	app.Settings.Set(map[string]string{SetSpamThreshold: "1"})
	// letters.test signs its mail with DKIM (key served without DNS); a forged copy doesn't.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dkimLookupTXT = func(domain string) ([]string, error) {
		if domain == "sel._domainkey.letters.test" {
			return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: domain, IsNotFound: true}
	}
	t.Cleanup(func() { dkimLookupTXT = nil })
	send := func(from, subj string, withMsgID, sign bool) {
		t.Helper()
		id := ""
		if withMsgID {
			id = "Message-ID: <" + RandomHex(6) + "@x.test>\r\n"
		}
		msg := []byte(fmt.Sprintf("From: %s\r\nTo: asi@skien.test\r\nSubject: %s\r\n%s\r\nhi\r\n", from, subj, id))
		if sign {
			var out bytes.Buffer
			if err := dkim.Sign(&out, bytes.NewReader(msg), &dkim.SignOptions{Domain: "letters.test", Selector: "sel", Signer: priv}); err != nil {
				t.Fatal(err)
			}
			msg = out.Bytes()
		}
		if err := sendSMTP(app.Cfg.SMTPAddr, from, []string{"asi@skien.test"}, msg); err != nil {
			t.Fatal(err)
		}
	}
	send("x@spam.test", "Blocked one", true, false)
	send("news@letters.test", "Allowed despite score", false, true) // missing Message-ID scores over 1
	send("news@letters.test", "Forged allowed sender", false, false)
	send("other@else.test", "Scored spam", false, false)
	waitFor(t, "deliveries", func() bool {
		var n int
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, a.ID).Scan(&n)
		return n == 4
	})
	got := storedBySubject(t, app, a.ID)
	if got["Blocked one"].folder != "spam" || got["Allowed despite score"].folder != "inbox" || got["Scored spam"].folder != "spam" ||
		got["Forged allowed sender"].folder != "spam" {
		t.Fatalf("delivery: %+v", got)
	}
	time.Sleep(200 * time.Millisecond)
	var replies int
	app.DB.QueryRow(`SELECT COUNT(*) FROM vacation_log WHERE user_id=? AND sender='x@spam.test'`, a.ID).Scan(&replies)
	if replies != 0 {
		t.Fatal("auto-replied to a blocked sender")
	}
	// A forged copy of an allowed address (domain not verified) stays in Spam; a verified one is
	// rescued.
	m := &Meta{From: Addr{Email: "news@letters.test"}}
	if arr := app.decideArrival(a.ID, m, nil, "spam", false); arr.Folder != "spam" {
		t.Fatal("allow list let unverified mail into the inbox")
	}
	if arr := app.decideArrival(a.ID, m, nil, "spam", true); arr.Folder != "inbox" {
		t.Fatal("allow list didn't rescue verified mail")
	}
	// Block with "move existing".
	old := deliverTo(t, app, a, "inbox", "promo@ads.test", "Promo")
	sentOne := deliverTo(t, app, a, "sent", "promo@ads.test", "edge: sent copy")
	app.SetSender(a.ID, "@ads.test", "block")
	if n, err := app.MoveSenderToSpam(a.ID, "@ads.test"); err != nil || n != 1 {
		t.Fatalf("moved %d (%v)", n, err)
	}
	if folderOf(t, app, old) != "spam" || folderOf(t, app, sentOne) != "sent" {
		t.Fatal("move existing")
	}
	app.RemoveSender(a.ID, "ADS.test")
	if v := app.senderVerdict(a.ID, "promo@ads.test"); v != "" {
		t.Fatal("remove")
	}
}

// Undo of Archive returns every message to where it was archived from, also when Archive was
// pressed in one of your folders and took the conversation's Inbox messages along.
func TestUnarchiveReturnsToOrigin(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	acme, _ := app.CreateFolder(a.ID, "Acme")
	first := deliverTo(t, app, a, "inbox", "x@acme.test", "Deal")
	tid := threadOf(t, app, first)
	reply := []byte(fmt.Sprintf("From: x@acme.test\r\nTo: %s\r\nSubject: Re: Deal\r\nMessage-ID: <r%s@x.test>\r\nIn-Reply-To: %s\r\nDate: Mon, 02 Jun 2025 11:00:00 +0000\r\n\r\nreply\r\n", a.Email, RandomHex(4), msgIDOf(t, app, first)))
	second, err := app.Deliver(a.ID, reply, DeliverOpts{Folder: "inbox"})
	if err != nil || threadOf(t, app, second) != tid {
		t.Fatalf("thread setup: %v", err)
	}
	app.ThreadActionIDs(a.ID, []int64{tid}, "inbox", "move:"+acme.Key, []int64{first})
	// Now: first in Acme, second in Inbox. Archive the conversation from Acme.
	moved, err := app.ThreadActionIDs(a.ID, []int64{tid}, acme.Key, "archive", nil)
	if err != nil || len(moved) != 2 || folderOf(t, app, first) != "archive" || folderOf(t, app, second) != "archive" {
		t.Fatalf("archive from folder: %v %v", moved, err)
	}
	if _, err := app.ThreadActionIDs(a.ID, []int64{tid}, "archive", "unarchive", moved); err != nil {
		t.Fatal(err)
	}
	if folderOf(t, app, first) != acme.Key || folderOf(t, app, second) != "inbox" {
		t.Fatalf("undo: first in %s, second in %s", folderOf(t, app, first), folderOf(t, app, second))
	}
	// From the Inbox, Archive and Undo work as before; a later "Move to Inbox" is plain.
	moved, _ = app.ThreadActionIDs(a.ID, []int64{tid}, "inbox", "archive", nil)
	app.ThreadActionIDs(a.ID, []int64{tid}, "archive", "unarchive", moved)
	if folderOf(t, app, second) != "inbox" {
		t.Fatal("undo of archive from the inbox")
	}
	app.ThreadActionIDs(a.ID, []int64{tid}, acme.Key, "archive", nil)
	app.ThreadActionIDs(a.ID, []int64{tid}, "archive", "inbox", nil)
	if folderOf(t, app, first) != "inbox" {
		t.Fatalf("Move to Inbox went to %s", folderOf(t, app, first))
	}
	if _, err := app.ThreadActionIDs(a.ID, []int64{tid}, "inbox", "unarchive", nil); err == nil {
		t.Fatal("unarchive accepted outside Archive")
	}
}

func msgIDOf(t *testing.T, app *App, id int64) string {
	t.Helper()
	var mid string
	app.DB.QueryRow(`SELECT message_id FROM messages WHERE id=?`, id).Scan(&mid)
	if !strings.HasPrefix(mid, "<") {
		mid = "<" + mid + ">"
	}
	return mid
}
