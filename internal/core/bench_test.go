package core

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

func BenchmarkDeliver(b *testing.B) {
	cfg := &Config{DataDir: b.TempDir(), SMTPAddr: "off", SubmissionAddr: "off", SubmissionsAddr: "off", HTTPAddr: "off", Hostname: "mail.test", MaxMessageMB: 25}
	app, err := NewApp(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	d, _ := app.CreateDomain("skien.test")
	u, _ := app.CreateUser(NewUser{Name: "A", Local: "a", DomainID: d.ID, Password: "password1"})
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			raw := []byte(fmt.Sprintf("From: x <x@example.test>\r\nTo: a@skien.test\r\nSubject: s %d %d\r\nMessage-ID: <%d.%d@x>\r\n\r\nhello world body\r\n", i, time.Now().UnixNano(), i, time.Now().UnixNano()))
			if _, err := app.Deliver(u.ID, raw, DeliverOpts{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
