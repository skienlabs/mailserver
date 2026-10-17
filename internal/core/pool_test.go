package core

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

// ListThreads must not need two pool connections at once: with MaxOpenConns(8), eight
// concurrent list requests otherwise deadlock the whole server. MaxOpenConns(1) makes it deterministic.
func TestListThreadsSingleConn(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), SMTPAddr: "off", SubmissionAddr: "off", SubmissionsAddr: "off", HTTPAddr: "off", Hostname: "mail.test", MaxMessageMB: 25}
	app, err := NewApp(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := app.CreateDomain("skien.test")
	u, _ := app.CreateUser(NewUser{Name: "A", Local: "a", DomainID: d.ID, Password: "password1"})
	for i := 0; i < 3; i++ {
		raw := fmt.Sprintf("From: x <x@example.test>\r\nTo: a@skien.test\r\nSubject: s%d\r\nMessage-ID: <%d@x>\r\n\r\nbody\r\n", i, i)
		if _, err := app.Deliver(u.ID, []byte(raw), DeliverOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	app.DB.SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		_, _, err := app.ListThreads(u.ID, "inbox", "", 0, 50)
		if err == nil {
			_, _, err = app.ListThreads(u.ID, "inbox", "body", 0, 50)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListThreads deadlocked on a single-connection pool")
	}
}

// Running the whole suite with SM_TEST_ONECONN=1 (one pooled connection) turns
// any query issued while another result set or transaction is open into a hang.
func TestSaveAliasSingleConn(t *testing.T) {
	app, d, a, b := newTestApp(t, false)
	app.DB.SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		_, err := app.SaveAlias(0, AliasInput{Local: "team", DomainID: d.ID, Name: "Team", Members: []AliasMember{{UserID: a.ID}, {UserID: b.ID}}})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaveAlias deadlocked on a single-connection pool")
	}
}
