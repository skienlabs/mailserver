package core

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUIHarness serves the web app with a fake IMAP account for browser tests of the import
// screen. It only runs when SM_UI_HARNESS is set to the port to listen on, e.g.
//
//	SM_UI_HARNESS=8095 go test ./internal/core -run TestUIHarness -timeout 0
func TestUIHarness(t *testing.T) {
	port := os.Getenv("SM_UI_HARNESS")
	if port == "" {
		t.Skip("set SM_UI_HARNESS to run")
	}
	app, _, a, _ := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1", SetOrgName: "Skienlab"})
	f := startFakeIMAPAt(t, app, "127.0.0.1:993") // the UI only offers the standard ports
	base := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 120; i++ {
		f.add(t, "INBOX", []string{"\\Seen"}, base.Add(time.Duration(i)*time.Hour), eml("client@zoho.test", fmt.Sprintf("Zoho message %d", i), fmt.Sprintf("h%d@zoho.test", i), "hello from zoho"))
	}
	f.add(t, "Sent", []string{"\\Seen"}, base, eml("asi@skien.test", "Old sent mail", "s1@skien.test", "sent"))
	f.add(t, "Clients/Acme", nil, base, eml("acme@acme.test", "Acme contract", "c1@acme.test", "contract"))
	f.add(t, "Spam", nil, base, eml("x@spam.test", "Spam", "sp@spam.test", "spam"))
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("HARNESS READY http://localhost:%s imap=127.0.0.1:%d user=%s password=password-asi\n", port, f.port, a.Email)
	srv := &http.Server{Addr: ":" + port, Handler: h}
	go srv.ListenAndServe()
	time.Sleep(10 * time.Minute)
	srv.Close()
}

// TestImportSpeed measures IMAP import throughput (SM_IMPORT_BENCH=<messages>).
func TestImportSpeed(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("SM_IMPORT_BENCH"))
	if n == 0 {
		t.Skip("set SM_IMPORT_BENCH to run")
	}
	app, _, a, _ := newTestApp(t, false)
	f := startFakeIMAP(t, app)
	body := strings.Repeat("Some ordinary email text for the import benchmark.\r\n", 150) // ~8 KB
	for i := 0; i < n; i++ {
		f.add(t, "INBOX", []string{"\\Seen"}, time.Now(), eml("v@zoho.test", fmt.Sprintf("bench %d", i), fmt.Sprintf("bench%d@zoho.test", i), body))
	}
	start := time.Now()
	job, err := app.ImportIMAP(a, IMAPSource{Host: "127.0.0.1", Port: f.port, Username: "username", Password: "password"}, []IMAPFolder{{Name: "INBOX", Target: "inbox"}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		j := app.ListImports(a.ID)[0]
		if j.ID == job.ID && j.Status != "running" && j.Status != "queued" {
			el := time.Since(start)
			t.Logf("imported %d in %v: %.0f msg/s (%s)", j.Imported, el.Round(time.Millisecond), float64(j.Imported)/el.Seconds(), j.Status)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestQueueUIHarness serves the web app with a busy delivery queue for browser tests of the
// admin queue screen (SM_QUEUE_HARNESS=<port>).
func TestQueueUIHarness(t *testing.T) {
	port := os.Getenv("SM_QUEUE_HARNESS")
	if port == "" {
		t.Skip("set SM_QUEUE_HARNESS to run")
	}
	app, a, f := queueApp(t)
	app.Settings.Set(map[string]string{SetSetupDone: "1", SetOrgName: "Skienlab"})
	f.delay = func(domain string) time.Duration {
		if domain == "slow.test" {
			return 90 * time.Second
		}
		return 20 * time.Millisecond
	}
	f.fail = func(rcpt string) error {
		switch {
		case strings.HasSuffix(rcpt, "@busy.test"):
			return tempErr("mx.busy.test said: 451 4.7.1 Greylisted, try again later")
		case strings.HasSuffix(rcpt, "@bounce.test"):
			return &deliveryError{perm: true, msg: "mx.bounce.test said: 550 5.1.1 The email account that you tried to reach does not exist"}
		}
		return nil
	}
	for i := 0; i < 12; i++ {
		submitRemote(t, app, a, []string{fmt.Sprintf("client%d@gmail.test", i)}, fmt.Sprintf("Invoice %d for October", i))
	}
	submitRemote(t, app, a, []string{"ops@busy.test", "team@gmail.test"}, "Weekly status report")
	submitRemote(t, app, a, []string{"nobody@bounce.test"}, "Proposal draft")
	for i := 0; i < 3; i++ {
		submitRemote(t, app, a, []string{fmt.Sprintf("u%d@slow.test", i)}, fmt.Sprintf("Large attachment %d", i))
	}
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("QUEUE HARNESS READY http://localhost:%s user=%s password=password-asi\n", port, a.Email)
	srv := &http.Server{Addr: ":" + port, Handler: h}
	go srv.ListenAndServe()
	time.Sleep(10 * time.Minute)
	srv.Close()
}
