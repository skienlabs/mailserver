package core

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportAndReimport(t *testing.T) {
	app, _, a, b := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	acme, _ := app.CreateFolder(a.ID, "Clients/Acme: Ltd") // ':' isn't allowed in Windows file names
	m1 := deliverTo(t, app, a, "inbox", "x@one.test", "Hello inbox")
	deliverTo(t, app, a, "sent", a.Email, "Sent: report/q3")
	m3 := deliverTo(t, app, a, "inbox", "ceo@acme.test", "Acme contract")
	app.ThreadActionIDs(a.ID, []int64{threadOf(t, app, m3)}, "inbox", "move:"+acme.Key, nil)
	deliverTo(t, app, a, "spam", "s@spam.test", "Spammy")
	app.SetFlag(a.ID, m1, true)
	deliverTo(t, app, b, "inbox", "x@two.test", "Not Asi's") // another user's mail never leaks in

	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	if code := c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	var folders []exportFolder
	c.do("GET", "/api/export/folders", nil, &folders)
	if len(folders) != 7 || folders[3].Name != "Clients" || folders[4].Name != "Clients/Acme: Ltd" || folders[4].Messages != 1 || folders[5].Key != "spam" || folders[5].Default || folders[6].Key != "trash" {
		t.Fatalf("folders: %+v", folders)
	}
	get := func(q string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/export"+q, nil)
		resp, err := c.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}
	resp, body := get("")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "asi mail") {
		t.Fatalf("export: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var csvData string
	for _, f := range zr.File {
		names[f.Name] = true
		if strings.HasSuffix(f.Name, "messages.csv") {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			csvData = string(b)
		}
	}
	has := func(prefix string) bool {
		for n := range names {
			if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".eml") {
				return true
			}
		}
		return false
	}
	if !has(exportTop+"/Inbox/") || !has(exportTop+"/Sent/") || !has(exportTop+"/Clients/Acme_ Ltd/") || has(exportTop+"/Spam/") || !names[exportTop+"/README.txt"] {
		t.Fatalf("zip contents: %v", names)
	}
	for n := range names {
		if strings.Contains(n, "Not Asi") || strings.Contains(n, "report/q3") {
			t.Fatalf("bad entry %q", n)
		}
	}
	recs, _ := csv.NewReader(strings.NewReader(csvData)).ReadAll()
	if len(recs) != 4 { // header + 3 messages
		t.Fatalf("csv: %v", recs)
	}
	// Choosing folders explicitly (Spam only).
	_, body = get("?folders=spam")
	zr2, _ := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	n := 0
	for _, f := range zr2.File {
		if strings.HasSuffix(f.Name, ".eml") {
			n++
			if !strings.HasPrefix(f.Name, exportTop+"/Spam/") {
				t.Fatalf("unexpected %s", f.Name)
			}
		}
	}
	if n != 1 {
		t.Fatalf("spam-only export had %d messages", n)
	}
	if resp, _ := get("?folders=f:99999"); resp.StatusCode != 400 {
		t.Fatalf("unknown folder: %d", resp.StatusCode)
	}
	// One export per person at a time.
	if err := exportBegin(a.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(""); resp.StatusCode != 429 {
		t.Fatalf("parallel export: %d", resp.StatusCode)
	}
	exportEnd(a.ID)

	// The export imports back with the same folders.
	id := uploadFile(t, app, b.ID, "asi mail.zip", body0(t, get))
	job, err := app.ImportFile(b, FileImportOpts{UploadID: id, Folder: "auto", OldEmail: a.Email})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, b.ID, job.ID)
	if done.Status != "done" || done.Imported != 3 {
		t.Fatalf("re-import: %+v", done)
	}
	got := storedBySubject(t, app, b.ID)
	f, err := app.folderByName(b.ID, "Clients/Acme_ Ltd")
	if err != nil || got["Acme contract"].folder != f.Key || got["Hello inbox"].folder != "inbox" || got["Sent: report/q3"].folder != "sent" {
		t.Fatalf("re-imported folders: %+v (%v)", got, err)
	}
}

func body0(t *testing.T, get func(string) (*http.Response, []byte)) []byte {
	_, b := get("")
	return b
}

// Drafts saved by mail apps are part of the export (they don't show in the web app).
func TestExportIncludesMailAppDrafts(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	deliverTo(t, app, a, "inbox", "x@one.test", "Hello inbox")
	if err := app.imapSaveDraft(a.ID, []byte("From: asi@skien.test\r\nSubject: Half written\r\n\r\nwip\r\n"), imapFlags{seen: true, draft: true}, nowMs()); err != nil {
		t.Fatal(err)
	}
	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil)
	var folders []exportFolder
	c.do("GET", "/api/export/folders", nil, &folders)
	found := false
	for _, f := range folders {
		if f.Key == exportAppDrafts && f.Messages == 1 && f.Default {
			found = true
		}
	}
	if !found {
		t.Fatalf("mail-app drafts not offered: %+v", folders)
	}
	resp, err := c.c.Get(srv.URL + "/api/export")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var draft, csvData string
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.HasPrefix(f.Name, exportTop+"/Drafts (mail apps)/") && strings.HasSuffix(f.Name, ".eml") {
			draft = string(b)
		}
		if strings.HasSuffix(f.Name, "messages.csv") {
			csvData = string(b)
		}
	}
	if !strings.Contains(draft, "Half written") || !strings.Contains(csvData, "Half written") {
		t.Fatalf("draft missing: %q / %q", draft, csvData)
	}
}
