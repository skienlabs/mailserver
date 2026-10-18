package core

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProductName is shown in the UI, banners and generated messages.
const ProductName = "Skien Mails"

// Version is the release number (see CHANGELOG.md). Bump it with every release:
// 1.0.0 → 1.0.1 for fixes and small additions, 1.1.0 for bigger features.
var Version = "1.0.1"

// App ties together storage, SMTP, delivery and the web server.
type App struct {
	Cfg      *Config
	DB       *sql.DB
	Settings *Settings
	Log      *slog.Logger
	Events   *Hub
	TLS      *TLSManager

	domainsMu sync.RWMutex
	domains   map[string]*Domain // lower-case name -> domain

	queueKick chan struct{}
	limiter   *RateLimiter
	imp       *importer
	qd        *queueDispatcher

	setupToken string

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// NewApp opens storage and prepares every subsystem (but starts nothing).
func NewApp(cfg *Config, log *slog.Logger) (*App, error) {
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "mail"), filepath.Join(cfg.DataDir, "queue"),
		filepath.Join(cfg.DataDir, "uploads"), filepath.Join(cfg.DataDir, "avatars"), filepath.Join(cfg.DataDir, "certs"), filepath.Join(cfg.DataDir, "imports")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := NewSettings(db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		Cfg:       cfg,
		DB:        db,
		Settings:  st,
		Log:       log,
		Events:    NewHub(),
		queueKick: make(chan struct{}, 1),
		limiter:   NewRateLimiter(),
		imp:       newImporter(),
		qd:        newQueueDispatcher(),
		ctx:       ctx,
		cancel:    cancel,
	}
	if err := a.LoadDomains(); err != nil {
		return nil, err
	}
	a.TLS = NewTLSManager(a)
	if !a.SetupDone() {
		a.setupToken = cfg.SetupToken
		if a.setupToken == "" {
			a.setupToken = strings.ToUpper(RandomHex(4))
		}
	}
	return a, nil
}

// SetupDone reports whether the organisation has been created.
func (a *App) SetupDone() bool { return a.Settings.Get(SetSetupDone) == "1" }

// Hostname is the public name of this server (EHLO, banner, Message-ID, TLS).
func (a *App) Hostname() string {
	if a.Cfg.Hostname != "" {
		return a.Cfg.Hostname
	}
	if h := a.Settings.Get(SetHostname); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

func (a *App) OrgName() string {
	if n := a.Settings.Get(SetOrgName); n != "" {
		return n
	}
	return ProductName
}

// MaxMessageBytes is the largest message accepted or sent.
func (a *App) MaxMessageBytes() int64 {
	mb := a.Settings.Int(SetMaxMessageMB)
	if mb <= 0 {
		mb = a.Cfg.MaxMessageMB
	}
	return int64(mb) * 1024 * 1024
}

// Go runs fn in a tracked goroutine that is awaited on shutdown.
func (a *App) Go(fn func(ctx context.Context)) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn(a.ctx)
	}()
}

// Start launches the background workers and network listeners.
func (a *App) Start() error {
	if !a.SetupDone() {
		a.Log.Warn("organisation not set up yet — open the web UI to create it", "setup_token", a.setupToken)
		fmt.Fprintf(os.Stderr, "\n  ==============================================\n    %s setup code:  %s\n    Open the web app and enter this code.\n  ==============================================\n\n", ProductName, a.setupToken)
	}
	if a.Cfg.TLSCertFile != "" {
		if _, err := a.TLS.loadFile(); err != nil {
			a.Log.Error("TLS certificate could not be loaded, using a self-signed certificate until it can", "err", err)
		}
	}
	a.recoverImports()
	a.Go(a.queueWorker)
	a.Go(a.janitor)
	if err := a.startSMTP(); err != nil {
		return err
	}
	return a.startHTTP()
}

// Shutdown stops listeners and waits for workers.
func (a *App) Shutdown() {
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.stopHTTP(ctx)
	a.stopSMTP(ctx)
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.DB.Close()
}

// logMail records an entry in the admin activity log.
func (a *App) logMail(direction, sender, rcpt, subject, status, detail, remote string) {
	subject = truncateRunes(strings.ToValidUTF8(subject, ""), 200)
	detail = truncateRunes(strings.ToValidUTF8(detail, ""), 500)
	if _, err := a.DB.Exec(`INSERT INTO mail_log(ts, direction, sender, rcpt, subject, status, detail, remote) VALUES(?,?,?,?,?,?,?,?)`,
		nowMs(), direction, sender, rcpt, subject, status, detail, remote); err != nil {
		a.Log.Error("mail_log insert", "err", err)
	}
}
