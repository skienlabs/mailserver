package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"archive/zip"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// Importing mail from another provider (Zoho, Gmail, Yahoo, any IMAP server) or from exported
// files (.zip of .eml as Zoho exports, .mbox as Gmail Takeout / Thunderbird, single .eml).
//
// Imports run in the background, one per user and a few at a time server-wide. Each message is
// stored like received mail (search index, threading, quota) but keeps its original date and
// read/starred state, doesn't notify, and isn't spam-filtered. Running the same import twice is
// safe: messages already in the mailbox (same Message-ID, or byte-identical) are skipped.

const (
	importConcurrency = 2               // imports running at once, server-wide
	importChunkMax    = 8 << 20         // largest upload chunk
	importUploadMax   = 20 << 30        // largest uploaded file (20 GiB)
	importMinMsgLimit = 50 << 20        // imported messages may be larger than the send limit
	importUploadTTL   = 24 * time.Hour  // unfinished uploads are removed after this
	imapCmdTimeout    = 3 * time.Minute // per IMAP command
	importSaveEvery   = 2 * time.Second // progress written to the database
	importSyncEvery   = 5 * time.Second // the user's open mailbox refreshes
)

// ImportJob is one import as shown in Settings.
type ImportJob struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"` // imap | file
	Source     string `json:"source"`
	Status     string `json:"status"` // queued | running | done | failed | cancelled | interrupted
	Total      int    `json:"total"`
	Imported   int    `json:"imported"`
	Skipped    int    `json:"skipped"`
	Failed     int    `json:"failed"`
	Current    string `json:"current"`
	Message    string `json:"message"`
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

type importRun struct {
	mu        sync.Mutex
	job       ImportJob
	userID    int64
	cancel    context.CancelFunc
	cancelled bool // by the user (vs. server shutdown)
	lastSave  time.Time
	lastSync  time.Time
}

type importer struct {
	mu      sync.Mutex
	runs    map[int64]*importRun // job id -> run
	slots   chan struct{}
	uploads map[string]*importUpload
	inUse   map[string]bool // upload ids whose files belong to an import job
	// Test hooks: connect to an IMAP server on 127.0.0.1 on any port, trusting rootCAs.
	allowLocal bool
	rootCAs    *x509.CertPool
}

func newImporter() *importer {
	return &importer{runs: map[int64]*importRun{}, slots: make(chan struct{}, importConcurrency), uploads: map[string]*importUpload{}, inUse: map[string]bool{}}
}

var (
	errImportBusy  = errors.New("An import is already running for your mailbox. Wait for it to finish or cancel it first.")
	errImportQuota = errors.New("Your mailbox is full, so the import stopped. Ask your administrator for more space, then run it again — messages already imported are skipped.")
)

// recoverImports marks imports that were running when the server stopped.
func (a *App) recoverImports() {
	a.DB.Exec(`UPDATE imports SET status='interrupted', finished_at=?, message='The server restarted during the import. Run it again to continue — messages already imported are skipped.'
		WHERE status IN ('queued','running')`, nowMs())
	os.MkdirAll(a.importDir(), 0o750)
}

func (a *App) importDir() string { return filepath.Join(a.Cfg.DataDir, "imports") }

// ---------------------------------------------------------------- jobs

func (r *importRun) snapshot() ImportJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}

func (r *importRun) update(fn func(j *ImportJob)) {
	r.mu.Lock()
	fn(&r.job)
	r.mu.Unlock()
}

func (a *App) saveImport(r *importRun, force bool) {
	r.mu.Lock()
	if !force && time.Since(r.lastSave) < importSaveEvery {
		r.mu.Unlock()
		return
	}
	r.lastSave = time.Now()
	j := r.job
	r.mu.Unlock()
	a.DB.Exec(`UPDATE imports SET status=?, total=?, imported=?, skipped=?, failed=?, current=?, message=?, started_at=?, finished_at=? WHERE id=?`,
		j.Status, j.Total, j.Imported, j.Skipped, j.Failed, j.Current, j.Message, j.StartedAt, j.FinishedAt, j.ID)
}

// startImport records a job and runs work in the background.
// cleanup (optional) runs when the job ends, whether or not work ever started.
func (a *App) startImport(u *User, kind, source string, work func(ctx context.Context, r *importRun) error, cleanup func()) (ImportJob, error) {
	im := a.imp
	im.mu.Lock()
	for _, r := range im.runs {
		if r.userID == u.ID {
			im.mu.Unlock()
			return ImportJob{}, errImportBusy
		}
	}
	now := nowMs()
	res, err := a.DB.Exec(`INSERT INTO imports(user_id, kind, source, status, created_at) VALUES(?,?,?,?,?)`, u.ID, kind, source, "queued", now)
	if err != nil {
		im.mu.Unlock()
		return ImportJob{}, err
	}
	id, _ := res.LastInsertId()
	ctx, cancel := context.WithCancel(a.ctx)
	r := &importRun{userID: u.ID, cancel: cancel, job: ImportJob{ID: id, Kind: kind, Source: source, Status: "queued", CreatedAt: now}}
	im.runs[id] = r
	im.mu.Unlock()

	a.Go(func(appCtx context.Context) {
		finish := func(err error) { // clean up first, so a finished job never has leftovers
			if cleanup != nil {
				cleanup()
			}
			a.finishImport(r, ctx, err)
		}
		defer func() {
			cancel()
			im.mu.Lock()
			delete(im.runs, id)
			im.mu.Unlock()
		}()
		select { // wait for a free slot
		case im.slots <- struct{}{}:
		case <-ctx.Done():
			finish(nil)
			return
		}
		defer func() { <-im.slots }()
		r.update(func(j *ImportJob) { j.Status = "running"; j.StartedAt = nowMs() })
		a.saveImport(r, true)
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					a.Log.Error("import panic", "job", id, "panic", p)
					err = errors.New("The import stopped because of an unexpected error.")
				}
			}()
			err = work(ctx, r)
		}()
		finish(err)
	})
	return r.snapshot(), nil
}

func (a *App) finishImport(r *importRun, ctx context.Context, err error) {
	r.mu.Lock()
	cancelled := r.cancelled
	r.mu.Unlock()
	r.update(func(j *ImportJob) {
		j.FinishedAt = nowMs()
		j.Current = ""
		switch {
		case cancelled:
			j.Status = "cancelled"
			j.Message = "Cancelled. Messages imported so far stay in your mailbox."
		case ctx.Err() != nil:
			j.Status = "interrupted"
			j.Message = "The server restarted during the import. Run it again to continue — messages already imported are skipped."
		case err != nil:
			j.Status = "failed"
			j.Message = err.Error()
		default:
			j.Status = "done"
			if j.Failed > 0 {
				j.Message = fmt.Sprintf("%s couldn't be imported (damaged or larger than %d MB).", plural(j.Failed, "message"), a.importMsgLimit()>>20)
			}
		}
	})
	a.saveImport(r, true)
	a.Events.Publish(r.userID, Event{Type: "sync"})
	a.Log.Info("import finished", "job", r.job.ID, "status", r.job.Status, "imported", r.job.Imported, "skipped", r.job.Skipped, "failed", r.job.Failed)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// CancelImport stops a user's import.
func (a *App) CancelImport(userID, id int64) bool {
	a.imp.mu.Lock()
	r := a.imp.runs[id]
	a.imp.mu.Unlock()
	if r == nil || r.userID != userID {
		return false
	}
	r.mu.Lock()
	r.cancelled = true
	r.mu.Unlock()
	r.cancel()
	return true
}

// ListImports returns the user's recent imports with live progress.
func (a *App) ListImports(userID int64) []ImportJob {
	out := []ImportJob{}
	rows, err := a.DB.Query(`SELECT id, kind, source, status, total, imported, skipped, failed, current, message, created_at, started_at, finished_at
		FROM imports WHERE user_id=? ORDER BY id DESC LIMIT 20`, userID)
	if err != nil {
		return out
	}
	for rows.Next() {
		var j ImportJob
		rows.Scan(&j.ID, &j.Kind, &j.Source, &j.Status, &j.Total, &j.Imported, &j.Skipped, &j.Failed, &j.Current, &j.Message, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
		out = append(out, j)
	}
	rows.Close()
	a.imp.mu.Lock()
	for i := range out {
		if r := a.imp.runs[out[i].ID]; r != nil {
			out[i] = r.snapshot()
		}
	}
	a.imp.mu.Unlock()
	return out
}

// ---------------------------------------------------------------- storing one message

func (a *App) importMsgLimit() int64 { return max(a.MaxMessageBytes(), importMinMsgLimit) }

type importMsg struct {
	raw      []byte
	folder   string // inbox | archive | sent | spam | trash | auto
	seen     bool
	flagged  bool
	received time.Time
}

// importOne stores a message; it returns errImportQuota to stop the whole import.
func (a *App) importOne(u *User, r *importRun, mine map[string]bool, m importMsg) error {
	count := func(fn func(j *ImportJob)) { r.update(fn); a.importTick(r) }
	raw := m.raw
	if len(raw) == 0 || int64(len(raw)) > a.importMsgLimit() || !looksLikeMessage(raw) {
		count(func(j *ImportJob) { j.Failed++ })
		return nil
	}
	sum := sha256.Sum256(raw)
	var seenHash int
	a.DB.QueryRow(`SELECT 1 FROM import_hashes WHERE user_id=? AND hash=?`, u.ID, sum[:]).Scan(&seenHash)
	if seenHash == 1 {
		count(func(j *ImportJob) { j.Skipped++ })
		return nil
	}
	meta := ParseMeta(raw)
	if meta.MessageID != "" {
		var dup int
		a.DB.QueryRow(`SELECT 1 FROM messages WHERE user_id=? AND message_id=? LIMIT 1`, u.ID, meta.MessageID).Scan(&dup)
		if dup == 1 {
			a.DB.Exec(`INSERT OR IGNORE INTO import_hashes(user_id, hash) VALUES(?,?)`, u.ID, sum[:])
			count(func(j *ImportJob) { j.Skipped++ })
			return nil
		}
	}
	folder := m.folder
	if folder == "auto" || folder == "" {
		folder = "inbox"
	}
	if folder == "inbox" && mine[strings.ToLower(meta.From.Email)] {
		folder = "sent" // something this person sent, found among received mail (e.g. a mixed export)
	}
	if !realFolders[folder] {
		folder = "archive"
	}
	seen := m.seen || folder == "sent"
	received := m.received
	if received.IsZero() || received.Year() < 1980 {
		received = meta.Date
	}
	var recv int64
	if !received.IsZero() && received.Year() >= 1980 {
		recv = received.UnixMilli()
	}
	_, err := a.Deliver(u.ID, raw, DeliverOpts{Folder: folder, Seen: seen, Flagged: m.flagged, Meta: &meta, EnforceQuota: true, ReceivedAt: recv, Quiet: true})
	if errors.Is(err, ErrQuota) {
		return errImportQuota
	}
	if err != nil {
		if isDiskFull(err) {
			return errors.New("The server ran out of disk space, so the import stopped.")
		}
		a.Log.Warn("import: message not stored", "job", r.job.ID, "err", err)
		count(func(j *ImportJob) { j.Failed++ })
		return nil
	}
	a.DB.Exec(`INSERT OR IGNORE INTO import_hashes(user_id, hash) VALUES(?,?)`, u.ID, sum[:])
	count(func(j *ImportJob) { j.Imported++ })
	return nil
}

// importTick persists progress and refreshes the user's mailbox now and then.
func (a *App) importTick(r *importRun) {
	a.saveImport(r, false)
	r.mu.Lock()
	due := time.Since(r.lastSync) >= importSyncEvery
	if due {
		r.lastSync = time.Now()
	}
	r.mu.Unlock()
	if due {
		a.Events.Publish(r.userID, Event{Type: "sync"})
	}
}

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// looksLikeMessage requires a header block: a "Name: value" line before any blank line.
func looksLikeMessage(raw []byte) bool {
	head := raw
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	line, _, _ := bytes.Cut(head, []byte("\n"))
	line = bytes.TrimRight(line, "\r")
	c := bytes.IndexByte(line, ':')
	if c <= 0 {
		return false
	}
	for _, ch := range line[:c] {
		if ch <= ' ' || ch > '~' {
			return false
		}
	}
	return true
}

// myAddresses are addresses that mark a message as sent by this user.
func (a *App) myAddresses(u *User, extra ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range a.SendAsAddresses(u) {
		m[strings.ToLower(s.Email)] = true
	}
	for _, e := range extra {
		if e = strings.ToLower(strings.TrimSpace(e)); strings.Contains(e, "@") {
			m[e] = true
		}
	}
	return m
}

// ---------------------------------------------------------------- IMAP

// IMAPSource is the account to import from. The password is never stored.
type IMAPSource struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"` // tls | starttls
	Username string `json:"username"`
	Password string `json:"password"`
}

// IMAPFolder is one folder on the source server and where its mail goes.
type IMAPFolder struct {
	Name     string `json:"name"`
	Messages int    `json:"messages"`
	Target   string `json:"target"` // inbox | archive | sent | spam | trash | skip
}

func (s *IMAPSource) normalize(anyPort bool) error {
	s.Host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s.Host)), ".")
	s.Username = strings.TrimSpace(s.Username)
	if s.Security == "" {
		s.Security = "tls"
	}
	if s.Port == 0 {
		s.Port = 993
	}
	switch {
	case s.Host == "" || (net.ParseIP(s.Host) == nil && !ValidDomainName(s.Host)):
		return errors.New("Enter the IMAP server name, like imappro.zoho.in")
	case s.Security != "tls":
		return errors.New("Only encrypted connections (SSL/TLS on port 993) are supported")
	case s.Port != 993 && !(anyPort && s.Port > 0 && s.Port < 65536):
		return errors.New("Use port 993 (SSL/TLS)")
	case s.Username == "" || s.Password == "":
		return errors.New("Enter the email address and password of the account to import from")
	case len(s.Username) > 320 || len(s.Password) > 1024 || strings.ContainsAny(s.Username+s.Password, "\r\n\x00"):
		return errors.New("Check the email address and password")
	}
	return nil
}

func (s IMAPSource) label() string { return s.Username + " · " + s.Host }

// errIMAPAuth marks a rejected sign-in (retrying won't help).
var errIMAPAuth = errors.New("The server didn't accept the email address or password. If the account uses two-step verification, create an app-specific password and use that. For Zoho, also check that IMAP access is turned on (Zoho Mail → Settings → Mail Accounts → IMAP).")

// imapConn is a signed-in connection plus the guard that limits what the server can send.
type imapConn struct {
	c     *imapclient.Client
	guard *imapGuardConn
	stop  func() bool
}

func (ic *imapConn) close() {
	if ic == nil {
		return
	}
	ic.stop()
	ic.guard.resetBudget(64 << 10)
	ic.c.Logout()
	ic.c.Terminate()
}

// imapDial connects over TLS and signs in. It never connects to this machine or link-local
// addresses, and it limits literal sizes before the IMAP library allocates them.
func (a *App) imapDial(ctx context.Context, s IMAPSource) (*imapConn, error) {
	allowLocal := a.imp.allowLocal
	dialer := &net.Dialer{Timeout: 20 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || (!allowLocal && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast())) {
			return errors.New("address not allowed")
		}
		return nil
	}}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := dialer.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, friendlyIMAPDialError(s, err)
	}
	tconn := tls.Client(raw, &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12, RootCAs: a.imp.rootCAs})
	if err := tconn.HandshakeContext(dctx); err != nil {
		raw.Close()
		return nil, friendlyIMAPDialError(s, err)
	}
	guard := newIMAPGuard(tconn, 1<<20) // greeting and sign-in
	// The library's greeting/CAPABILITY exchange has no timeout of its own.
	type newResult struct {
		c   *imapclient.Client
		err error
	}
	ready := make(chan newResult, 1)
	go func() {
		c, err := imapclient.New(guard)
		ready <- newResult{c, err}
	}()
	var c *imapclient.Client
	select {
	case res := <-ready:
		if res.err != nil {
			raw.Close()
			if gerr := guard.cause(nil); gerr != nil {
				return nil, fmt.Errorf("Couldn't connect to %s: %w", s.Host, gerr)
			}
			return nil, friendlyIMAPDialError(s, res.err)
		}
		c = res.c
	case <-time.After(30 * time.Second):
		raw.Close()
		return nil, fmt.Errorf("%s didn't answer in time.", s.Host)
	case <-ctx.Done():
		raw.Close()
		return nil, ctx.Err()
	}
	c.Timeout = imapCmdTimeout
	c.ErrorLog = imapErrorLog{guard}
	// Twice the import limit: servers like Exchange report estimated sizes, so a message listed
	// just under the limit may arrive a little over it. It's then counted as failed (importOne)
	// instead of ending the import.
	if !setMaxLiteral(c, uint32(min(2*a.importMsgLimit(), 1<<32-1))) {
		c.Terminate()
		return nil, errors.New("Importing isn't available in this build (IMAP size limit couldn't be set).")
	}
	guard.release()
	stop := context.AfterFunc(ctx, func() { c.Terminate() })
	if err := c.Login(s.Username, s.Password); err != nil {
		// The library returns the server's text for a NO/BAD answer, and its own errors when the
		// connection broke before an answer came (worth retrying, not "wrong password").
		var netErr net.Error
		dropped := err.Error() == "imap: connection closed" || errors.As(err, &netErr) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
		stop()
		c.Terminate()
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case guard.cause(nil) != nil:
			return nil, fmt.Errorf("Couldn't sign in to %s: %w", s.Host, guard.cause(nil))
		case dropped:
			return nil, fmt.Errorf("The connection to %s dropped while signing in", s.Host)
		}
		return nil, errIMAPAuth
	}
	return &imapConn{c: c, guard: guard, stop: stop}, nil
}

func friendlyIMAPDialError(s IMAPSource, err error) error {
	msg := err.Error()
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return fmt.Errorf("Couldn't find the server %s. Check the server name.", s.Host)
	case strings.Contains(msg, "address not allowed"):
		return errors.New("That server address isn't allowed.")
	case strings.Contains(msg, "certificate"):
		return fmt.Errorf("The server's security certificate isn't valid for %s, so the connection was refused.", s.Host)
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o timeout"):
		return fmt.Errorf("Couldn't reach %s on port %d (timed out).", s.Host, s.Port)
	case strings.Contains(msg, "refused"):
		return fmt.Errorf("%s refused the connection on port %d.", s.Host, s.Port)
	}
	return fmt.Errorf("Couldn't connect to %s: %v", s.Host, err)
}

// IMAPFolders signs in and lists the folders with a suggested destination for each.
func (a *App) IMAPFolders(ctx context.Context, s IMAPSource) ([]IMAPFolder, error) {
	if err := s.normalize(a.imp.allowLocal); err != nil {
		return nil, err
	}
	ic, err := a.imapDial(ctx, s)
	if err != nil {
		return nil, err
	}
	defer ic.close()
	return imapListFolders(ic)
}

// imapMaxFolders is the most folders offered from one account.
const imapMaxFolders = 5000

func imapListFolders(ic *imapConn) ([]IMAPFolder, error) {
	c := ic.c
	ic.guard.resetBudget(4 << 20) // 5000 folders × a few hundred bytes
	ch := make(chan *imap.MailboxInfo, 64)
	done := make(chan error, 1)
	go func() { done <- c.List("", "*", ch) }()
	var infos []*imap.MailboxInfo
	for mi := range ch {
		if len(infos) <= imapMaxFolders {
			infos = append(infos, mi)
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("Couldn't list the folders: %w", ic.guard.cause(err))
	}
	if len(infos) > imapMaxFolders {
		return nil, fmt.Errorf("That account has more than %d folders, which is more than can be imported at once.", imapMaxFolders)
	}
	var out []IMAPFolder
	for _, mi := range infos {
		if hasAttr(mi.Attributes, imap.NoSelectAttr) || hasAttr(mi.Attributes, "\\NonExistent") {
			continue
		}
		f := IMAPFolder{Name: mi.Name, Target: suggestTarget(mi.Name, mi.Delimiter, mi.Attributes)}
		ic.guard.resetBudget(1 << 20)
		if st, err := c.Status(mi.Name, []imap.StatusItem{imap.StatusMessages}); err == nil {
			f.Messages = int(st.Messages)
		}
		out = append(out, f)
	}
	// Inbox first, then Sent, then the rest (so threads and Sent copies are recognised in order).
	rank := func(f IMAPFolder) int {
		switch f.Target {
		case "inbox":
			return 0
		case "sent":
			return 1
		case "skip":
			return 4
		}
		if isAllMail(f.Name) {
			return 3
		}
		return 2
	}
	for i := 1; i < len(out); i++ { // stable insertion sort by rank
		for j := i; j > 0 && rank(out[j]) < rank(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func hasAttr(attrs []string, want string) bool {
	for _, a := range attrs {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

func isAllMail(name string) bool {
	n := strings.ToLower(name)
	return n == "[gmail]/all mail" || n == "[google mail]/all mail" || strings.HasSuffix(n, "/all mail") || n == "all mail"
}

// suggestTarget maps a source folder to one of ours.
func suggestTarget(name, delim string, attrs []string) string {
	switch {
	case strings.EqualFold(name, "INBOX"):
		return "inbox"
	case hasAttr(attrs, imap.SentAttr):
		return "sent"
	case hasAttr(attrs, imap.DraftsAttr), hasAttr(attrs, imap.JunkAttr), hasAttr(attrs, imap.TrashAttr):
		return "skip"
	case hasAttr(attrs, imap.AllAttr), hasAttr(attrs, imap.ArchiveAttr):
		return "archive"
	}
	leaf := strings.ToLower(name)
	if delim != "" {
		if i := strings.LastIndex(leaf, strings.ToLower(delim)); i >= 0 {
			leaf = leaf[i+len(delim):]
		}
	}
	switch strings.TrimSpace(leaf) {
	case "inbox":
		return "inbox"
	case "sent", "sent items", "sent mail", "sent messages", "sent-mail", "sentmail", "outbox sent":
		return "sent"
	case "drafts", "draft", "templates", "outbox", "notes", "contacts", "calendar", "tasks", "chats", "chat", "journal":
		return "skip"
	case "spam", "junk", "junk e-mail", "junk email", "bulk mail", "bulk", "trash", "deleted", "deleted items", "deleted messages", "bin":
		return "skip"
	}
	return "archive"
}

// ImportIMAP starts importing the chosen folders.
func (a *App) ImportIMAP(u *User, s IMAPSource, folders []IMAPFolder) (ImportJob, error) {
	if err := s.normalize(a.imp.allowLocal); err != nil {
		return ImportJob{}, err
	}
	var chosen []IMAPFolder
	for _, f := range folders {
		f.Name = strings.TrimSpace(f.Name)
		if f.Name == "" || f.Target == "skip" {
			continue
		}
		if !realFolders[f.Target] {
			return ImportJob{}, fmt.Errorf("Choose where the mail in %q should go", f.Name)
		}
		chosen = append(chosen, f)
	}
	if len(chosen) == 0 {
		return ImportJob{}, errors.New("Choose at least one folder to import")
	}
	if len(chosen) > 1000 {
		return ImportJob{}, errors.New("Too many folders")
	}
	return a.startImport(u, "imap", s.label(), func(ctx context.Context, r *importRun) error {
		return a.runIMAPImport(ctx, r, u, s, chosen)
	}, nil)
}

type imapMsgInfo struct { // kept small: a folder can have millions
	uid           uint32
	size          uint32
	seen, flagged bool
	date          time.Time
}

func (a *App) runIMAPImport(ctx context.Context, r *importRun, u *User, s IMAPSource, folders []IMAPFolder) error {
	mine := a.myAddresses(u, s.Username)
	var ic *imapConn
	defer func() { ic.close() }()
	connect := func() error {
		ic.close()
		ic = nil
		var err error
		ic, err = a.imapDial(ctx, s)
		return err
	}
	if err := connect(); err != nil {
		return err
	}
	// failures counts consecutive problems; any successful step resets it. Each problem gets
	// a fresh connection, retried with growing pauses before giving up.
	failures := 0
	retryAfter := func(cause error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failures++
		if failures > 6 {
			return fmt.Errorf("The connection to %s kept dropping: %v", s.Host, cause)
		}
		var err error
		for attempt := 1; attempt <= 5; attempt++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(min(2<<(failures+attempt-2), 60)) * time.Second):
			}
			if err = connect(); err == nil || errors.Is(err, errIMAPAuth) || isGuardErr(err) || ctx.Err() != nil {
				break
			}
		}
		return err
	}
	// Totals up front, so progress means something.
	total := 0
	for _, f := range folders {
		ic.guard.resetBudget(1 << 20)
		if st, err := ic.c.Status(f.Name, []imap.StatusItem{imap.StatusMessages}); err == nil {
			total += int(min(st.Messages, imapMaxFolder))
		}
	}
	r.update(func(j *ImportJob) { j.Total = total })
	limit := a.importMsgLimit()
	section := &imap.BodySectionName{Peek: true}
	for _, f := range folders {
		r.update(func(j *ImportJob) { j.Current = f.Name })
		a.saveImport(r, true)
		var infos []imapMsgInfo
		for {
			var err error
			infos, err = imapFolderIndex(ic, f.Name)
			if err == nil {
				failures = 0
				break
			}
			if errors.Is(err, errIMAPFolderTooBig) {
				return err
			}
			if isGuardErr(err) {
				return fmt.Errorf("Stopped: %s %v.", s.Host, err)
			}
			if rerr := retryAfter(err); rerr != nil {
				return rerr
			}
		}
		// Messages larger than the limit aren't downloaded at all.
		kept := infos[:0]
		for _, m := range infos {
			if int64(m.size) > limit {
				r.update(func(j *ImportJob) { j.Failed++ })
			} else {
				kept = append(kept, m)
			}
		}
		infos = kept
		reselect := false
		for i := 0; i < len(infos); {
			if err := ctx.Err(); err != nil {
				return err
			}
			if reselect {
				ic.guard.resetBudget(1 << 20)
				if _, err := ic.c.Select(f.Name, true); err != nil {
					if err = ic.guard.cause(err); isGuardErr(err) {
						return fmt.Errorf("Stopped: %s %v.", s.Host, err)
					}
					if rerr := retryAfter(err); rerr != nil {
						return rerr
					}
					continue
				}
				reselect = false
			}
			// Batch of up to 25 messages or ~32 MB.
			start := i
			var bytesIn int64
			for i < len(infos) && i-start < 25 && (i == start || bytesIn+int64(infos[i].size) <= 32<<20) {
				bytesIn += int64(infos[i].size)
				i++
			}
			batch := infos[start:i]
			bodies, err := imapFetchBodies(ic, batch, section, limit)
			if err != nil {
				if isGuardErr(err) {
					return fmt.Errorf("Stopped: %s %v.", s.Host, err)
				}
				if rerr := retryAfter(err); rerr != nil {
					return rerr
				}
				i = start // retry this batch on the new connection
				reselect = true
				continue
			}
			failures = 0
			for _, m := range batch {
				raw, ok := bodies[m.uid]
				if !ok { // deleted on the server meanwhile
					r.update(func(j *ImportJob) { j.Skipped++ })
					continue
				}
				msg := importMsg{raw: raw, folder: f.Target, seen: m.seen, flagged: m.flagged, received: m.date}
				if err := a.importOne(u, r, mine, msg); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// imapMaxFolder is the most messages imported from one folder (a sanity bound on what the
// server claims, so a bogus count can't make us allocate huge buffers).
const imapMaxFolder = 5_000_000

var errIMAPFolderTooBig = errors.New("A folder on that server reports more than 5 million messages, which looks wrong. Import the other folders or contact your old provider.")

// imapFolderIndex selects a folder (read-only) and lists its messages oldest first.
func imapFolderIndex(ic *imapConn, name string) ([]imapMsgInfo, error) {
	ic.guard.resetBudget(1 << 20)
	st, err := ic.c.Select(name, true)
	if err != nil {
		return nil, err
	}
	if st.Messages == 0 {
		return nil, nil
	}
	if st.Messages > imapMaxFolder {
		return nil, errIMAPFolderTooBig
	}
	set := new(imap.SeqSet)
	set.AddRange(1, st.Messages)
	ic.guard.resetBudget(4<<20 + int64(st.Messages)*1024) // usually ~150 bytes per message
	ch := make(chan *imap.Message, 256)
	done := make(chan error, 1)
	go func() {
		done <- ic.c.Fetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size}, ch)
	}()
	out := make([]imapMsgInfo, 0, min(st.Messages, 10_000))
	for m := range ch {
		if m.Uid == 0 || len(out) >= imapMaxFolder {
			continue
		}
		out = append(out, imapMsgInfo{uid: m.Uid, size: m.Size, seen: hasAttr(m.Flags, imap.SeenFlag), flagged: hasAttr(m.Flags, imap.FlaggedFlag), date: m.InternalDate})
	}
	if err := <-done; err != nil {
		return nil, ic.guard.cause(err)
	}
	return out, nil
}

// imapFetchBodies downloads full messages for a batch of UIDs. The guard caps the literal
// bytes for the whole batch, whatever sizes the server claimed earlier.
func imapFetchBodies(ic *imapConn, batch []imapMsgInfo, section *imap.BodySectionName, limit int64) (map[uint32][]byte, error) {
	set := new(imap.SeqSet)
	var claimed int64
	for _, m := range batch {
		set.AddNum(m.uid)
		claimed += int64(m.size)
	}
	ic.guard.resetBudget(claimed + 2*limit + int64(len(batch))*4096 + 1<<20)
	wanted := make(map[uint32]bool, len(batch))
	for _, m := range batch {
		wanted[m.uid] = true
	}
	ch := make(chan *imap.Message, len(batch))
	done := make(chan error, 1)
	go func() { done <- ic.c.UidFetch(set, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, ch) }()
	out := make(map[uint32][]byte, len(batch))
	for m := range ch {
		body := m.GetBody(section)
		if body == nil || !wanted[m.Uid] || out[m.Uid] != nil {
			continue // only what was asked for, once
		}
		raw, err := io.ReadAll(io.LimitReader(body, limit+1))
		if err != nil {
			continue
		}
		out[m.Uid] = raw
	}
	if err := <-done; err != nil {
		return nil, ic.guard.cause(err)
	}
	return out, nil
}

// ---------------------------------------------------------------- uploaded files

type importUpload struct {
	id       string
	userID   int64
	name     string
	size     int64 // declared
	received int64
	touched  time.Time // last activity; abandoned uploads expire from here
	busy     bool
	discard  bool // delete as soon as the chunk being written is finished
}

func (a *App) uploadPath(id string) string { return filepath.Join(a.importDir(), id+".part") }

// BeginImportUpload reserves space for a file that will arrive in chunks.
func (a *App) BeginImportUpload(userID int64, name string, size int64) (string, error) {
	name = filepath.Base(strings.TrimSpace(sanitizeHeaderValue(name)))
	if name == "" || name == "." || name == "/" {
		name = "import"
	}
	if size <= 0 {
		return "", errors.New("The file is empty")
	}
	if size > importUploadMax {
		return "", fmt.Errorf("The file is larger than %d GB. Split the export into smaller parts.", importUploadMax>>30)
	}
	im := a.imp
	im.mu.Lock()
	defer im.mu.Unlock()
	n := 0
	for _, up := range im.uploads {
		if up.userID == userID {
			n++
		}
	}
	if n >= 3 {
		return "", errors.New("Finish or cancel your other uploads first")
	}
	id := RandomHex(16)
	f, err := os.OpenFile(a.uploadPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return "", err
	}
	f.Close()
	im.uploads[id] = &importUpload{id: id, userID: userID, name: name, size: size, touched: time.Now()}
	return id, nil
}

// AppendImportUpload writes one chunk at offset (which must be where the file currently ends).
func (a *App) AppendImportUpload(userID int64, id string, offset int64, body io.Reader) (int64, error) {
	im := a.imp
	im.mu.Lock()
	up := im.uploads[id]
	if up == nil || up.userID != userID {
		im.mu.Unlock()
		return 0, os.ErrNotExist
	}
	if up.busy || up.discard {
		got := up.received
		im.mu.Unlock()
		return got, errUploadBusy
	}
	if offset != up.received {
		got := up.received
		im.mu.Unlock()
		return got, errUploadOffset
	}
	up.busy = true
	up.touched = time.Now()
	im.mu.Unlock()
	defer func() {
		im.mu.Lock()
		up.busy = false
		up.touched = time.Now()
		drop := up.discard
		if drop {
			delete(im.uploads, id)
		}
		im.mu.Unlock()
		if drop {
			os.Remove(a.uploadPath(id))
		}
	}()

	f, err := os.OpenFile(a.uploadPath(id), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return up.received, err
	}
	room := up.size - up.received
	n, err := io.Copy(f, io.LimitReader(body, min(room, importChunkMax)+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if n > room {
		f2, _ := os.OpenFile(a.uploadPath(id), os.O_WRONLY, 0)
		if f2 != nil {
			f2.Truncate(up.received)
			f2.Close()
		}
		return up.received, errors.New("More data arrived than the file's size")
	}
	if err != nil {
		// keep the file consistent with what we report
		if f2, _ := os.OpenFile(a.uploadPath(id), os.O_WRONLY, 0); f2 != nil {
			f2.Truncate(up.received)
			f2.Close()
		}
		if isDiskFull(err) {
			return up.received, errors.New("The server is out of disk space")
		}
		return up.received, err
	}
	im.mu.Lock()
	up.received += n
	got := up.received
	im.mu.Unlock()
	return got, nil
}

var (
	errUploadOffset = errors.New("upload offset mismatch")
	errUploadBusy   = errors.New("upload busy")
)

// DiscardImportUpload deletes an upload that won't be used.
func (a *App) DiscardImportUpload(userID int64, id string) {
	im := a.imp
	im.mu.Lock()
	up := im.uploads[id]
	if up == nil || up.userID != userID {
		im.mu.Unlock()
		return
	}
	if up.busy { // a chunk is still arriving: remove it when that finishes
		up.discard = true
		im.mu.Unlock()
		return
	}
	delete(im.uploads, id)
	im.mu.Unlock()
	os.Remove(a.uploadPath(id))
}

// cleanImportUploads removes abandoned uploads.
func (a *App) cleanImportUploads() {
	im := a.imp
	cut := time.Now().Add(-importUploadTTL)
	im.mu.Lock()
	for id, up := range im.uploads {
		if !up.busy && up.touched.Before(cut) {
			delete(im.uploads, id)
			os.Remove(a.uploadPath(id))
		}
	}
	known := map[string]bool{}
	for id := range im.uploads {
		known[id] = true
	}
	for id := range im.inUse { // files owned by queued or running imports
		known[id] = true
	}
	im.mu.Unlock()
	entries, _ := os.ReadDir(a.importDir())
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".part")
		if info, err := e.Info(); err == nil && !known[id] && info.ModTime().Before(cut) {
			os.Remove(filepath.Join(a.importDir(), e.Name()))
		}
	}
}

// FileImportOpts says where messages from a file go.
type FileImportOpts struct {
	UploadID string `json:"upload_id"`
	Folder   string `json:"folder"`    // auto | inbox | archive | sent
	MarkRead bool   `json:"mark_read"` // messages without their own read state
	OldEmail string `json:"old_email"` // your address in the old account (to recognise sent mail)
}

// ImportFile starts importing a fully uploaded file.
func (a *App) ImportFile(u *User, o FileImportOpts) (ImportJob, error) {
	switch o.Folder {
	case "", "auto":
		o.Folder = "auto"
	case "inbox", "archive", "sent":
	default:
		return ImportJob{}, errors.New("Choose where the messages should go")
	}
	im := a.imp
	im.mu.Lock()
	up := im.uploads[o.UploadID]
	if up == nil || up.userID != u.ID {
		im.mu.Unlock()
		return ImportJob{}, errors.New("The uploaded file wasn't found. Upload it again.")
	}
	if up.busy || up.received != up.size {
		im.mu.Unlock()
		return ImportJob{}, errors.New("The file hasn't finished uploading")
	}
	delete(im.uploads, o.UploadID) // the job owns the file now
	im.inUse[up.id] = true
	im.mu.Unlock()
	file := a.uploadPath(up.id)
	job, err := a.startImport(u, "file", up.name, func(ctx context.Context, r *importRun) error {
		return a.runFileImport(ctx, r, u, file, up.name, o)
	}, func() {
		os.Remove(file)
		im.mu.Lock()
		delete(im.inUse, up.id)
		im.mu.Unlock()
	})
	if err != nil {
		im.mu.Lock() // give the upload back so the user can retry
		delete(im.inUse, up.id)
		up.touched = time.Now()
		im.uploads[up.id] = up
		im.mu.Unlock()
	}
	return job, err
}

func (a *App) runFileImport(ctx context.Context, r *importRun, u *User, file, name string, o FileImportOpts) error {
	mine := a.myAddresses(u, o.OldEmail)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [5]byte
	n, _ := io.ReadFull(f, magic[:])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	emit := func(m importMsg) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.folder == "skip" { // e.g. Gmail chats and drafts
			r.update(func(j *ImportJob) { j.Skipped++ })
			a.importTick(r)
			return nil
		}
		return a.importOne(u, r, mine, m)
	}
	switch {
	case n >= 4 && string(magic[:4]) == "PK\x03\x04":
		return a.importZip(ctx, r, file, o, emit)
	case n >= 5 && string(magic[:5]) == "From ":
		cnt, _ := countMbox(ctx, f)
		r.update(func(j *ImportJob) { j.Total = cnt })
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return readMbox(ctx, f, a.importMsgLimit(), func(raw []byte, tooBig bool) error {
			return emit(fileMsg(raw, tooBig, o, ""))
		})
	default:
		r.update(func(j *ImportJob) { j.Total = 1 })
		raw, err := io.ReadAll(io.LimitReader(f, a.importMsgLimit()+1))
		if err != nil {
			return err
		}
		if !looksLikeMessage(raw) {
			return errors.New("This file isn't an email export. Use a .zip of .eml files (Zoho), an .mbox file (Gmail Takeout, Thunderbird) or a single .eml file.")
		}
		return emit(fileMsg(raw, false, o, ""))
	}
}

func (a *App) importZip(ctx context.Context, r *importRun, file string, o FileImportOpts, emit func(importMsg) error) error {
	zr, zc, err := openZipLimited(file, 1_000_000, 64<<20) // ~500k files with typical names
	if errors.Is(err, errZipTooManyFiles) {
		return err
	}
	if err != nil {
		return errors.New("The .zip file is damaged or incomplete. Download the export again.")
	}
	defer zc.Close()
	limit := a.importMsgLimit()
	total := 0
	var entries []*zip.File
	for _, zf := range zr.File {
		lower := strings.ToLower(zf.Name)
		if zf.FileInfo().IsDir() || strings.HasPrefix(lower, "__macosx/") || strings.HasPrefix(path.Base(lower), "._") {
			continue
		}
		switch {
		case strings.HasSuffix(lower, ".eml"):
			total++
			entries = append(entries, zf)
		case strings.HasSuffix(lower, ".mbox") || path.Base(lower) == "mbox":
			entries = append(entries, zf)
			if rc, err := zf.Open(); err == nil {
				cnt, _ := countMbox(ctx, rc)
				rc.Close()
				total += cnt
			}
		}
		if len(entries) > 2_000_000 {
			return errors.New("The .zip file has too many files")
		}
	}
	if len(entries) == 0 {
		return errors.New("No emails were found in the .zip file. It should contain .eml files (like a Zoho Mail export) or .mbox files.")
	}
	r.update(func(j *ImportJob) { j.Total = total })
	for _, zf := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		hint := folderHint(path.Dir(zf.Name))
		lower := strings.ToLower(zf.Name)
		rc, err := zf.Open()
		if err != nil {
			r.update(func(j *ImportJob) { j.Failed++ })
			continue
		}
		if strings.HasSuffix(lower, ".eml") {
			raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
			rc.Close()
			if err != nil {
				r.update(func(j *ImportJob) { j.Failed++ })
				continue
			}
			if err := emit(fileMsg(raw, false, o, hint)); err != nil {
				return err
			}
			continue
		}
		err = readMbox(ctx, rc, limit, func(raw []byte, tooBig bool) error { return emit(fileMsg(raw, tooBig, o, hint)) })
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// fileMsg decides folder and flags for a message from a file.
func fileMsg(raw []byte, tooBig bool, o FileImportOpts, hint string) importMsg {
	if tooBig {
		return importMsg{} // empty raw: counted as failed
	}
	m := importMsg{raw: raw, folder: o.Folder, seen: o.MarkRead}
	if o.Folder == "auto" {
		if hint != "" {
			m.folder = hint
		}
		// Gmail Takeout labels each message.
		if labels := headerValue(raw, "X-Gmail-Labels"); labels != "" {
			folder, seen, flagged, skip := gmailLabels(labels)
			if skip {
				return importMsg{raw: nil, folder: "skip"}
			}
			m.folder, m.seen, m.flagged = folder, seen, flagged
		}
	}
	return m
}

// folderHint maps a folder name inside an archive ("Sent", "Inbox") to one of ours.
func folderHint(dir string) string {
	if dir == "." || dir == "" {
		return ""
	}
	switch t := suggestTarget(path.Base(dir), "/", nil); t {
	case "inbox", "sent":
		return t
	case "skip":
		leaf := strings.ToLower(path.Base(dir))
		switch leaf {
		case "spam", "junk", "junk e-mail", "junk email", "bulk mail":
			return "spam"
		case "trash", "deleted", "deleted items", "deleted messages", "bin":
			return "trash"
		}
		return "archive"
	}
	return "archive"
}

func gmailLabels(v string) (folder string, seen, flagged, skip bool) {
	var labels []string
	for _, l := range splitLabels(v) {
		labels = append(labels, strings.ToLower(strings.TrimSpace(l)))
	}
	has := func(name string) bool {
		for _, l := range labels {
			if l == name {
				return true
			}
		}
		return false
	}
	seen = !has("unread")
	flagged = has("starred")
	switch {
	case has("chat") || has("chats") || has("draft") || has("drafts"):
		return "", false, false, true
	case has("spam"):
		folder = "spam"
	case has("trash"):
		folder = "trash"
	case has("inbox"):
		folder = "inbox"
	case has("sent"):
		folder = "sent"
	default:
		folder = "archive"
	}
	return
}

// splitLabels splits a comma-separated label list, honouring double quotes.
func splitLabels(v string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range v {
		switch {
		case r == '"':
			inQ = !inQ
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// headerValue returns an unfolded header from the header block (empty when absent).
func headerValue(raw []byte, name string) string {
	var val []byte
	found := false
	for pos := 0; pos < len(raw); {
		n := bytes.IndexByte(raw[pos:], '\n')
		line := raw[pos:]
		if n >= 0 {
			line = raw[pos : pos+n]
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if found {
				val = append(append(val, ' '), bytes.TrimSpace(line)...)
			}
		} else if found {
			break
		} else if c := bytes.IndexByte(line, ':'); c > 0 && strings.EqualFold(strings.TrimRight(string(line[:c]), " \t"), name) {
			found = true
			val = append(val[:0], bytes.TrimSpace(line[c+1:])...)
		}
		if n < 0 {
			break
		}
		pos += n + 1
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(string(val)); err == nil {
		return dec
	}
	return string(val)
}

// ---------------------------------------------------------------- mbox

// isMboxFrom reports whether line starts a new message ("From sender date...").
// The separator ends in a date ("From sender Mon Jun  2 10:00:00 2025" or "... +0000 2025"),
// which tells it apart from ordinary text that happens to start with "From ".
var reMboxFrom = regexp.MustCompile(`^From \S+ .*\b\d{1,2}:\d{2}(:\d{2})?\b.*\b\d{4}\s*$`)

func isMboxFrom(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("From ")) || len(line) > 512 {
		return false
	}
	return reMboxFrom.Match(bytes.TrimRight(line, "\r\n"))
}

// countMbox counts messages without keeping them.
func countMbox(ctx context.Context, r io.Reader) (int, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	n := 0
	prevBlank := true
	for reads := 0; ; reads++ {
		if reads&4095 == 0 && ctx.Err() != nil {
			return n, ctx.Err()
		}
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			if prevBlank && isMboxFrom(line) {
				n++
			}
			prevBlank = len(bytes.TrimRight(line, "\r\n")) == 0
		}
		if err == bufio.ErrBufferFull {
			prevBlank = false
			for err == bufio.ErrBufferFull { // skip the rest of a very long line
				_, err = br.ReadSlice('\n')
			}
		}
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
	}
}

// readMbox splits an mbox stream into messages (mboxo/mboxrd), undoing ">From " quoting.
// Messages larger than limit are reported with tooBig and no data. Memory stays bounded by
// limit even for malformed input (one enormous line).
func readMbox(ctx context.Context, r io.Reader, limit int64, fn func(raw []byte, tooBig bool) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf bytes.Buffer
	inMsg, tooBig := false, false
	prevBlank, atLineStart := true, true
	add := func(b []byte) {
		if !inMsg || tooBig {
			return
		}
		if int64(buf.Len()+len(b)) > limit {
			tooBig = true
			buf.Reset()
			return
		}
		buf.Write(b)
	}
	flush := func() error {
		if !inMsg {
			return nil
		}
		raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // the blank line before the next "From "
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		var err error
		if tooBig {
			err = fn(nil, true)
		} else if len(bytes.TrimSpace(raw)) > 0 {
			err = fn(append([]byte(nil), raw...), false)
		}
		buf.Reset()
		tooBig = false
		return err
	}
	for reads := 0; ; reads++ {
		if reads&4095 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		chunk, err := br.ReadSlice('\n')
		complete := err == nil || (err == io.EOF && len(chunk) > 0)
		if len(chunk) > 0 {
			if atLineStart && prevBlank && complete && isMboxFrom(chunk) {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				inMsg = true
				prevBlank = false
			} else {
				body := chunk
				if atLineStart {
					if q := bytes.TrimLeft(chunk, ">"); len(q) < len(chunk) && bytes.HasPrefix(q, []byte("From ")) {
						body = chunk[1:] // ">From " -> "From ", ">>From " -> ">From "
					}
				}
				add(body)
				if atLineStart {
					prevBlank = complete && len(bytes.TrimRight(chunk, "\r\n")) == 0
				} else {
					prevBlank = false
				}
			}
			atLineStart = err == nil // ReadSlice returned a whole line ending in \n
		}
		if err == bufio.ErrBufferFull {
			continue // rest of a long line follows
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}
