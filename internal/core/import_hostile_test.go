package core

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the second review of the import code.

// rawIMAP is a TLS server that sends greeting, then answers each command line with
// reply(n, tag, command) for the n-th connection. An empty reply means the default answer
// ("" with keep=false: hang up without answering); "HOLD" stalls until the test ends.
func rawIMAP(t *testing.T, app *App, greeting string, reply func(n int, tag, cmd string) (string, bool)) int {
	t.Helper()
	rawCertOnce.Do(func() { rawCert, rawPool = selfSignedLocal(t) }) // one CA, so servers can coexist
	cert, pool := rawCert, rawPool
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold); l.Close() })
	app.imp.allowLocal = true
	app.imp.rootCAs = pool
	go func() {
		n := 0
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n++
			go func(c net.Conn, n int) {
				defer c.Close()
				io.WriteString(c, greeting)
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
					cmd := strings.ToUpper(rest)
					out, keep := "", true
					if reply != nil {
						out, keep = reply(n, tag, cmd)
					}
					if out == "" && !keep {
						return // hang up without answering
					}
					if out == "" {
						switch {
						case strings.HasPrefix(cmd, "CAPABILITY"):
							out = "* CAPABILITY IMAP4rev1\r\n" + tag + " OK done\r\n"
						case strings.HasPrefix(cmd, "LOGOUT"):
							out = "* BYE\r\n" + tag + " OK done\r\n"
						default:
							out = tag + " OK done\r\n"
						}
					}
					if out == "HOLD" {
						<-hold
						return
					}
					io.WriteString(c, out)
					if !keep {
						return
					}
				}
			}(c, n)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

var (
	rawCertOnce sync.Once
	rawCert     tls.Certificate
	rawPool     *x509.CertPool
)

func heapNow() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A "{40}" at the end of status text used to desynchronise the old literal scanner, after which
// a huge literal got through. Now the library itself refuses literals above the limit.
func TestIMAPFakeLiteralMarkerInStatusText(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	port := scriptedIMAP(t, app, "1", func(w net.Conn, tag string) {
		fmt.Fprint(w, "* OK hello {40}\r\n* 1 FETCH (UID 1 BODY[] {3000000000}\r\nFrom: x@y\r\n")
		time.Sleep(time.Minute) // then go quiet
	})
	before := heapNow()
	start := time.Now()
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImport(t, app, a.ID, job.ID)
	if done.Status != "failed" || !strings.Contains(done.Message, "larger than the import limit") {
		t.Fatalf("job: %+v", done)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("took %v to stop (should stop as soon as the literal is announced)", el)
	}
	if grew := int64(heapNow()) - int64(before); grew > 100<<20 {
		t.Fatalf("heap grew by %d MB", grew>>20)
	}
}

// Before the library's limit is set (greeting, capabilities), no literal is accepted at all.
func TestIMAPLiteralBeforeSignInRefused(t *testing.T) {
	src := func(port int) IMAPSource {
		return IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}
	}
	for name, tc := range map[string]struct {
		greeting string
		reply    func(n int, tag, cmd string) (string, bool)
	}{
		"greeting": {greeting: "* OK [CAPABILITY IMAP4rev1] {3000000000}\r\nxxxx"},
		"capability": {greeting: "* OK ready\r\n", reply: func(n int, tag, cmd string) (string, bool) {
			if strings.HasPrefix(cmd, "CAPABILITY") {
				return "* CAPABILITY {3000000000}\r\nIMAP4rev1", true
			}
			return "", true
		}},
	} {
		t.Run(name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t, false)
			port := rawIMAP(t, app, tc.greeting, tc.reply)
			before := heapNow()
			_, err := app.IMAPFolders(t.Context(), src(port))
			if err == nil || !strings.Contains(err.Error(), "unexpected greeting") {
				t.Fatalf("err = %v", err)
			}
			if grew := int64(heapNow()) - int64(before); grew > 100<<20 {
				t.Fatalf("heap grew by %d MB", grew>>20)
			}
		})
	}
	// An ordinary greeting without capabilities still works (the client asks for them).
	app, _, _, _ := newTestApp(t, false)
	port := rawIMAP(t, app, "* OK ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LIST") {
			return "* LIST () \"/\" INBOX\r\n" + tag + " OK done\r\n", true
		}
		return "", true
	})
	folders, err := app.IMAPFolders(t.Context(), src(port))
	if err != nil || len(folders) != 1 || folders[0].Name != "INBOX" {
		t.Fatalf("plain greeting: %v %+v", err, folders)
	}
}

// A server that streams endless data for a command is cut off by the byte budget.
func TestIMAPEndlessResponseCutOff(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	port := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LIST") {
			var b strings.Builder
			for i := 0; i < 1000000; i++ { // ~80 MB of folder names (budget is 32 MB)
				fmt.Fprintf(&b, "* LIST () \"/\" \"folder-%08d-padding-padding-padding-padding-padding-pad\"\r\n", i)
			}
			return b.String() + tag + " OK done\r\n", true
		}
		return "", true
	})
	_, err := app.IMAPFolders(t.Context(), IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "far more data") {
		t.Fatalf("err = %v", err)
	}
}

// Wrong password vs. a connection that drops during sign-in.
func TestIMAPLoginDropIsNotWrongPassword(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	no := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LOGIN") {
			return tag + " NO [AUTHENTICATIONFAILED] Invalid credentials\r\n", true
		}
		return "", true
	})
	noClose := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LOGIN") {
			return tag + " NO bad password, goodbye\r\n", false // some servers hang up after a failure
		}
		return "", true
	})
	drop := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LOGIN") {
			return "* BYE going away\r\n", false
		}
		return "", true
	})
	cut := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		if strings.HasPrefix(cmd, "LOGIN") {
			return "* OK", false // half a line, then gone
		}
		return "", true
	})
	dial := func(port int) error {
		ic, err := app.imapDial(t.Context(), IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"})
		ic.close()
		return err
	}
	for name, port := range map[string]int{"no": no, "no+close": noClose} {
		if err := dial(port); !errors.Is(err, errIMAPAuth) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, port := range map[string]int{"bye": drop, "cut": cut} {
		if err := dial(port); err == nil || errors.Is(err, errIMAPAuth) {
			t.Fatalf("%s: dropped connection reported as %v", name, err)
		}
	}
}

// During an import, a drop while signing in again is retried rather than ending the import
// with "wrong password".
func TestIMAPImportRetriesDropDuringLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	app, _, a, _ := newTestApp(t, false)
	port := rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		switch {
		case strings.HasPrefix(cmd, "LOGIN") && n == 2:
			return "", false // close without answering
		case strings.HasPrefix(cmd, "STATUS"):
			return "* STATUS INBOX (MESSAGES 1)\r\n" + tag + " OK done\r\n", true
		case strings.HasPrefix(cmd, "EXAMINE"), strings.HasPrefix(cmd, "SELECT"):
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] x\r\n" + tag + " OK [READ-ONLY] done\r\n", true
		case strings.HasPrefix(cmd, "FETCH"):
			return "* 1 FETCH (UID 1 FLAGS () INTERNALDATE \"01-Jan-2025 10:00:00 +0000\" RFC822.SIZE 60)\r\n" + tag + " OK done\r\n", true
		case strings.HasPrefix(cmd, "UID FETCH") && n == 1:
			return "", false // the first connection drops mid-import
		case strings.HasPrefix(cmd, "UID FETCH"):
			body := "From: a@zoho.test\r\nSubject: retried\r\nMessage-ID: <rt1@zoho.test>\r\n\r\nhi\r\n"
			return fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(body), body, tag), true
		}
		return "", true
	})
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitImportFor(t, app, a.ID, job.ID, 60*time.Second)
	if done.Status != "done" || done.Imported != 1 {
		t.Fatalf("job: %+v", done)
	}
}

// An end record claiming no entries and a tiny directory, in front of a huge real directory.
func TestZipForgedEndRecord(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n := 65536 * 3 // the forged count (0) matches modulo 65536
	for i := 0; i < n; i++ {
		zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%07d.eml", i), Method: zip.Store})
	}
	zw.Close()
	data := buf.Bytes()
	z64 := data[len(data)-22-20-56:]
	if binary.LittleEndian.Uint32(z64) != 0x06064b50 {
		t.Fatal("no zip64 record")
	}
	cdOff := binary.LittleEndian.Uint64(z64[48:])
	out := append([]byte(nil), data[:len(data)-22-20-56]...)
	e := make([]byte, 22)
	binary.LittleEndian.PutUint32(e, 0x06054b50)
	binary.LittleEndian.PutUint32(e[12:], 46)
	binary.LittleEndian.PutUint32(e[16:], uint32(cdOff))
	out = append(out, e...)
	p := filepath.Join(t.TempDir(), "forged.zip")
	os.WriteFile(p, out, 0o600)
	if zr, err := zip.OpenReader(p); err == nil { // the attack works on archive/zip itself
		if len(zr.File) != n {
			t.Fatalf("archive/zip loaded %d entries; test no longer models the attack", len(zr.File))
		}
		zr.Close()
	}
	if _, _, err := openZipLimited(p, 100_000, 64<<20); err != errZipTooManyFiles {
		t.Fatalf("entry cap: %v", err)
	}
	if _, _, err := openZipLimited(p, 10_000_000, 1<<20); err != errZipTooManyFiles {
		t.Fatalf("directory bytes cap: %v", err)
	}
}

// A slow but steady upload must not be cut off; a stalled one must be.
func TestUploadChunkIdleDeadline(t *testing.T) {
	old := uploadIdleTimeout
	uploadIdleTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { uploadIdleTimeout = old })
	app, _, a, _ := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	if code := c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	put := func(body io.Reader, size int) (int, string) {
		var up struct{ ID string }
		c.do("POST", "/api/import/uploads", map[string]any{"name": "x.mbox", "size": size}, &up)
		req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/api/import/uploads/%s?offset=0", srv.URL, up.ID), body)
		req.Header.Set("X-SM", "1")
		req.ContentLength = int64(size)
		resp, err := c.c.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// 5 s total at 4 KB per 250 ms — several times the idle timeout, but never idle that long.
	if code, body := put(&trickle{left: 80 << 10, every: 250 * time.Millisecond, n: 4 << 10}, 80<<10); code != 200 || !strings.Contains(body, `"received":81920`) {
		t.Fatalf("slow upload: %d %s", code, body)
	}
	// Stalls after 1 KB: the server gives up after the idle timeout.
	start := time.Now()
	code, _ := put(&trickle{left: 2 << 10, every: 10 * time.Second, n: 1 << 10}, 2<<10)
	if code == 200 || time.Since(start) > 9*time.Second {
		t.Fatalf("stalled upload: %d after %v", code, time.Since(start))
	}
}

type trickle struct {
	left, n int
	every   time.Duration
	started bool
}

func (s *trickle) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	if s.started {
		time.Sleep(s.every)
	}
	s.started = true
	n := min(len(p), s.n, s.left)
	for i := range n {
		p[i] = 'x'
	}
	s.left -= n
	return n, nil
}

// ---------------------------------------------------------------- third review round

// peakHeap samples heap use until stop is called and returns growth over the starting point.
func peakHeap() (stop func() int64) {
	base := heapNow()
	var peak atomic.Uint64
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return func() int64 { close(done); <-finished; return int64(peak.Load()) - int64(base) }
}

// indexIMAP answers like a mailbox with one folder; index is the reply to the index FETCH and
// body (if set) the reply to UID FETCH.
func indexIMAP(t *testing.T, app *App, exists int, index func(tag string) string, body func(tag string) string) int {
	return rawIMAP(t, app, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(n int, tag, cmd string) (string, bool) {
		switch {
		case strings.HasPrefix(cmd, "STATUS"):
			return fmt.Sprintf("* STATUS INBOX (MESSAGES %d)\r\n%s OK done\r\n", exists, tag), true
		case strings.HasPrefix(cmd, "EXAMINE"), strings.HasPrefix(cmd, "SELECT"):
			return fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY 1] x\r\n%s OK [READ-ONLY] done\r\n", exists, tag), true
		case strings.HasPrefix(cmd, "FETCH"):
			return index(tag), true
		case strings.HasPrefix(cmd, "UID FETCH") && body != nil:
			return body(tag), true
		}
		return "", true
	})
}

func runImport(t *testing.T, app *App, a *User, port int, wait time.Duration) ImportJob {
	t.Helper()
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: port, Username: "u", Password: "p"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	return waitImportFor(t, app, a.ID, job.ID, wait)
}

// Tiny fields cost the parser ~8× their size in memory; the patched library caps fields per
// response (the old code peaked at gigabytes).
func TestIMAPFieldFloodRefused(t *testing.T) {
	for name, flood := range map[string]string{
		"flags":    strings.Repeat(`\a `, 10<<20),     // 30 MB
		"lists":    strings.Repeat(`() `, 10<<20),     // empty lists
		"literals": strings.Repeat("{0}\r\n ", 5<<20), // empty literals, with line breaks
	} {
		t.Run(name, func(t *testing.T) {
			app, _, a, _ := newTestApp(t, false)
			port := indexIMAP(t, app, 4999999, func(tag string) string {
				return "* 1 FETCH (UID 1 FLAGS (" + flood + `\b) RFC822.SIZE 100)` + "\r\n" + tag + " OK done\r\n"
			}, nil)
			stop := peakHeap()
			done := runImport(t, app, a, port, 60*time.Second)
			grew := stop()
			if done.Status != "failed" || !strings.Contains(done.Message, "malformed") {
				t.Fatalf("job: %+v", done)
			}
			if grew > 150<<20 {
				t.Fatalf("heap peaked %d MB above start", grew>>20)
			}
		})
	}
}

// Deeply nested lists used to recurse without limit (stack exhaustion crashes the process).
func TestIMAPDeepNestingRefused(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	port := indexIMAP(t, app, 1, func(tag string) string {
		return "* 1 FETCH (UID 1 FLAGS " + strings.Repeat("(", 3_000_000) + "\r\n" + tag + " OK done\r\n"
	}, nil)
	done := runImport(t, app, a, port, 60*time.Second)
	if done.Status != "failed" || !strings.Contains(done.Message, "malformed") {
		t.Fatalf("job: %+v", done)
	}
}

// Atoms used to be built by string concatenation (quadratic): a 900 KB atom took minutes.
func TestIMAPLongAtoms(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	body := "From: a@zoho.test\r\nSubject: long flag\r\nMessage-ID: <la1@zoho.test>\r\n\r\nhi\r\n"
	port := indexIMAP(t, app, 1, func(tag string) string {
		return "* 1 FETCH (UID 1 FLAGS (\\Seen $" + strings.Repeat("k", 900<<10) + ") RFC822.SIZE 60)\r\n" + tag + " OK done\r\n"
	}, func(tag string) string {
		return fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(body), body, tag)
	})
	start := time.Now()
	done := runImport(t, app, a, port, 60*time.Second)
	if done.Status != "done" || done.Imported != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("job after %v: %+v", time.Since(start), done)
	}
	app2, _, a2, _ := newTestApp(t, false)
	port = indexIMAP(t, app2, 1, func(tag string) string {
		return "* 1 FETCH (UID 1 FLAGS ($" + strings.Repeat("k", 3<<20) + ") RFC822.SIZE 60)\r\n" + tag + " OK done\r\n"
	}, nil)
	if done := runImport(t, app2, a2, port, 60*time.Second); done.Status != "failed" || !strings.Contains(done.Message, "malformed") {
		t.Fatalf("3 MB atom: %+v", done)
	}
}

// A server that under-reports a size (Exchange gives estimates) and then sends a message a
// little over the limit: that message counts as failed; the import carries on.
func TestIMAPUnderReportedSizeIsNotFatal(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	limit := app.importMsgLimit()
	big := "From: a@zoho.test\r\nSubject: big\r\nMessage-ID: <big1@zoho.test>\r\n\r\n" + strings.Repeat("x", int(limit))
	small := "From: a@zoho.test\r\nSubject: small\r\nMessage-ID: <small1@zoho.test>\r\n\r\nhi\r\n"
	port := indexIMAP(t, app, 2, func(tag string) string {
		return fmt.Sprintf("* 1 FETCH (UID 1 FLAGS () RFC822.SIZE %d)\r\n* 2 FETCH (UID 2 FLAGS () RFC822.SIZE 60)\r\n%s OK done\r\n", limit-1000, tag)
	}, func(tag string) string {
		return fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n* 2 FETCH (UID 2 BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(big), big, len(small), small, tag)
	})
	done := runImport(t, app, a, port, 60*time.Second)
	if done.Status != "done" || done.Imported != 1 || done.Failed != 1 {
		t.Fatalf("job: %+v", done)
	}
}

// Bodies for UIDs that weren't asked for (or repeated) are dropped, not collected.
func TestIMAPUnrequestedBodiesIgnored(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	msg := func(n int) string {
		return fmt.Sprintf("From: a@zoho.test\r\nSubject: m%d\r\nMessage-ID: <u%d@zoho.test>\r\n\r\nhi\r\n", n, n)
	}
	port := indexIMAP(t, app, 1, func(tag string) string {
		return "* 1 FETCH (UID 7 FLAGS () RFC822.SIZE 60)\r\n" + tag + " OK done\r\n"
	}, func(tag string) string {
		var b strings.Builder
		for _, uid := range []int{7, 7, 8, 9} {
			fmt.Fprintf(&b, "* 1 FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", uid, len(msg(uid)), msg(uid))
		}
		return b.String() + tag + " OK done\r\n"
	})
	done := runImport(t, app, a, port, 30*time.Second)
	if done.Status != "done" || done.Imported != 1 {
		t.Fatalf("job: %+v", done)
	}
}
