package core

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"
)

// Mail-app access (IMAP) on ports 993 (TLS) and 143 (STARTTLS). Sign-in uses the same account,
// password and failed-attempt limits as the web app. Each connection keeps its own view of a
// folder's message numbers and is told about changes on NOOP / IDLE (see imap_mailbox.go).

// Limits (variables so tests can lower them).
var (
	imapMaxConns           = 2000 // all clients
	imapMaxConnsPerIP      = 300  // a whole office behind one address
	imapMaxPreAuthPerIP    = 20   // connections from one address that haven't signed in yet
	imapMaxConnsPerUser    = 50   // phones, laptops, Thunderbird's several connections each
	imapLiteralMemoryMin   = int64(128 << 20)
	imapLiteralMemoryTest  int64 // tests: a small budget
	imapLiteralPerUserTest int64
	imapLiteralGraceTest   time.Duration
	imapPreAuthTimeout     = 60 * time.Second
	imapIdlePoll           = 30 * time.Second
	imapAliveEvery         = time.Minute // how often a session re-checks its account
)

var imapState struct {
	mu      sync.Mutex
	servers []*imapserver.Server
	lns     []net.Listener  // closed by stopIMAP even if Serve hasn't started yet
	ports   map[bool]string // listening port by implicit TLS (true = 993-style, false = STARTTLS)
	users   map[int64]int   // live sessions per user
	serving sync.WaitGroup
}

func (a *App) startIMAP() {
	// Message uploads held in memory at once, for both ports together, and per account.
	maxLit := a.MaxMessageBytes() + 1<<20
	budget := &imapserver.LiteralBudget{Max: max(imapLiteralMemoryMin, 3*maxLit), PerUser: 2 * maxLit}
	if imapLiteralMemoryTest > 0 {
		budget.Max, budget.PerUser, budget.Grace = imapLiteralMemoryTest, imapLiteralPerUserTest, imapLiteralGraceTest
	}
	listen := func(addr string, implicitTLS bool) {
		if addr == "" || addr == "off" {
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Mail apps are an extra: a busy port must not stop mail delivery and the web app.
			a.Log.Error("IMAP not started", "addr", addr, "err", err)
			return
		}
		var l net.Listener = &imapLimitListener{Listener: ln}
		if implicitTLS {
			l = tls.NewListener(l, a.TLS.ServerConfig())
		}
		s := imapserver.New(&imapBackend{a: a})
		s.TLSConfig = a.TLS.ServerConfig()
		s.AllowInsecureAuth = false
		s.AutoLogout = imapserver.MinAutoLogout
		s.PreAuthTimeout = imapPreAuthTimeout
		s.MaxLiteralSize = 64 << 10 // before sign-in
		lim := a.MaxMessageBytes() + 1<<20
		if lim >= 1<<32 {
			lim = 1<<32 - 1
		}
		s.MaxLiteralSizeAuthed = uint32(lim)
		s.LiteralBudget = budget
		s.CommandDone = a.imapCommandDone
		s.IdlePoll = imapIdlePoll
		s.ErrorLog = log.New(io.Discard, "", 0)
		if a.Cfg.LogLevelDebug {
			s.ErrorLog = log.Default()
		}
		s.Enable(imapSpecialUse{})
		imapState.mu.Lock()
		imapState.servers = append(imapState.servers, s)
		imapState.lns = append(imapState.lns, ln)
		if imapState.ports == nil {
			imapState.ports = map[bool]string{}
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		imapState.ports[implicitTLS] = port
		imapState.mu.Unlock()
		imapState.serving.Add(1)
		go func() {
			defer imapState.serving.Done()
			if err := s.Serve(l); err != nil && !errors.Is(err, net.ErrClosed) {
				a.Log.Error("IMAP server stopped", "addr", addr, "err", err)
			}
		}()
		a.Log.Info("imap listening", "addr", addr, "tls", implicitTLS)
	}
	listen(a.Cfg.IMAPAddr, false)
	listen(a.Cfg.IMAPSAddr, true)
}

func (a *App) stopIMAP() {
	imapState.mu.Lock()
	servers, lns := imapState.servers, imapState.lns
	imapState.servers, imapState.lns, imapState.ports = nil, nil, nil
	imapState.mu.Unlock()
	for _, ln := range lns {
		ln.Close() // Serve returns at once, even if it starts after this
	}
	for _, s := range servers {
		s.Close()
	}
	imapState.serving.Wait()
	for _, s := range servers {
		s.WaitConns(10 * time.Second) // commands already running finish before storage closes
	}
}

// imapConns counts connections on both IMAP ports together: in total, per address, and per
// address before sign-in (an office shares one address, so the per-address limit is generous,
// but connections that never sign in are held to a small number).
var imapConns = struct {
	sync.Mutex
	total   int
	perIP   map[string]int
	preAuth map[string]int
	byAddr  map[string]*imapCountedConn
}{perIP: map[string]int{}, preAuth: map[string]int{}, byAddr: map[string]*imapCountedConn{}}

// imapLimitListener refuses connections beyond the limits, and gives every new connection a
// deadline so a silent client can't hold one open before the greeting.
type imapLimitListener struct {
	net.Listener
}

func (l *imapLimitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		addr := c.RemoteAddr().String()
		host, _, _ := net.SplitHostPort(addr)
		ip := ipKey(host)
		imapConns.Lock()
		if imapConns.total >= imapMaxConns || imapConns.perIP[ip] >= imapMaxConnsPerIP || imapConns.preAuth[ip] >= imapMaxPreAuthPerIP {
			imapConns.Unlock()
			c.Close()
			continue
		}
		imapConns.total++
		imapConns.perIP[ip]++
		imapConns.preAuth[ip]++
		cc := &imapCountedConn{Conn: c, ip: ip, addr: addr}
		imapConns.byAddr[addr] = cc
		imapConns.Unlock()
		c.SetDeadline(time.Now().Add(imapPreAuthTimeout))
		return cc, nil
	}
}

type imapCountedConn struct {
	net.Conn
	ip, addr string
	authed   bool // guarded by imapConns
	once     sync.Once
}

func (c *imapCountedConn) Close() error {
	c.once.Do(func() {
		imapConns.Lock()
		imapConns.total--
		if imapConns.perIP[c.ip]--; imapConns.perIP[c.ip] <= 0 {
			delete(imapConns.perIP, c.ip)
		}
		if !c.authed {
			if imapConns.preAuth[c.ip]--; imapConns.preAuth[c.ip] <= 0 {
				delete(imapConns.preAuth, c.ip)
			}
		}
		if imapConns.byAddr[c.addr] == c {
			delete(imapConns.byAddr, c.addr)
		}
		imapConns.Unlock()
	})
	return c.Conn.Close()
}

// imapSignedIn moves a connection out of the "not signed in yet" count.
func imapSignedIn(remote net.Addr) {
	if remote == nil {
		return
	}
	imapConns.Lock()
	defer imapConns.Unlock()
	if c := imapConns.byAddr[remote.String()]; c != nil && !c.authed {
		c.authed = true
		if imapConns.preAuth[c.ip]--; imapConns.preAuth[c.ip] <= 0 {
			delete(imapConns.preAuth, c.ip)
		}
	}
}

// imapSpecialUse advertises RFC 6154 so apps recognise Sent, Drafts, Spam, Trash and Archive.
type imapSpecialUse struct{}

func (imapSpecialUse) Capabilities(c imapserver.Conn) []string {
	if c.Context().State&imap.AuthenticatedState != 0 {
		return []string{"SPECIAL-USE"}
	}
	return nil
}
func (imapSpecialUse) Command(string) imapserver.HandlerFactory { return nil }

// ---------------------------------------------------------------- sign-in

type imapBackend struct{ a *App }

// imapAuthCache remembers recent successful sign-ins (mail apps sign in on every connection,
// often several times a minute) so Argon2 doesn't run each time. Entries are keyed by an HMAC
// of the password with a random per-process key and the stored hash, so a password change
// invalidates them.
var imapAuthCache = struct {
	sync.Mutex
	key  []byte
	m    map[string]time.Time
	fail map[string]time.Time // recently failed credentials (see imapRecentFailure)
}{m: map[string]time.Time{}, fail: map[string]time.Time{}}

// imapFailKey identifies one (account, stored password, attempted password) combination.
func imapFailKey(account, storedHash, password string) string {
	imapAuthCache.Lock()
	defer imapAuthCache.Unlock()
	if imapAuthCache.key == nil {
		imapAuthCache.key = make([]byte, 32)
		rand.Read(imapAuthCache.key)
	}
	mac := hmac.New(sha256.New, imapAuthCache.key)
	fmt.Fprintf(mac, "fail\x00%s\x00%s\x00%s", account, storedHash, password)
	return string(mac.Sum(nil))
}

// imapRecentFailure reports whether exactly this sign-in failed in the last 15 minutes. Mail
// apps retry an old password over and over after it's changed; those repeats are refused at
// once without counting again toward the sign-in limits or running Argon2. The stored hash is
// part of the key, so after a password reset the same text is checked afresh.
func imapRecentFailure(k string) bool {
	imapAuthCache.Lock()
	defer imapAuthCache.Unlock()
	exp, ok := imapAuthCache.fail[k]
	return ok && time.Now().Before(exp)
}

func imapRememberFailure(k string) {
	imapAuthCache.Lock()
	defer imapAuthCache.Unlock()
	if len(imapAuthCache.fail) > 10000 {
		now := time.Now()
		for kk, e := range imapAuthCache.fail {
			if now.After(e) {
				delete(imapAuthCache.fail, kk)
			}
		}
		if len(imapAuthCache.fail) > 10000 {
			imapAuthCache.fail = map[string]time.Time{}
		}
	}
	imapAuthCache.fail[k] = time.Now().Add(authWindow)
}

func (a *App) checkPasswordCached(u *User, password string) bool {
	imapAuthCache.Lock()
	if imapAuthCache.key == nil {
		imapAuthCache.key = make([]byte, 32)
		rand.Read(imapAuthCache.key)
	}
	mac := hmac.New(sha256.New, imapAuthCache.key)
	fmt.Fprintf(mac, "%d\x00%s\x00%s", u.ID, u.passwordHash, password)
	k := string(mac.Sum(nil))
	exp, ok := imapAuthCache.m[k]
	imapAuthCache.Unlock()
	if ok && time.Now().Before(exp) {
		return true
	}
	if !CheckPassword(u.passwordHash, password) {
		return false
	}
	imapAuthCache.Lock()
	if len(imapAuthCache.m) > 10000 {
		now := time.Now()
		for k, e := range imapAuthCache.m {
			if now.After(e) {
				delete(imapAuthCache.m, k)
			}
		}
		if len(imapAuthCache.m) > 10000 {
			imapAuthCache.m = map[string]time.Time{}
		}
	}
	imapAuthCache.m[k] = time.Now().Add(10 * time.Minute)
	imapAuthCache.Unlock()
	return true
}

func (b *imapBackend) Login(info *imap.ConnInfo, username, password string) (backend.User, error) {
	a := b.a
	ip := ""
	if info != nil && info.RemoteAddr != nil {
		ip, _, _ = net.SplitHostPort(info.RemoteAddr.String())
	}
	username = strings.ToLower(strings.TrimSpace(username))
	errWrong := errors.New("[AUTHENTICATIONFAILED] Wrong email address or password")
	u, err := a.UserByEmail(username)
	stored := ""
	if err == nil {
		stored = u.passwordHash + "|" + u.Status
	}
	failKey := imapFailKey(username, stored, password)
	if imapRecentFailure(failKey) {
		return nil, errWrong // the same wrong password again: already counted and logged
	}
	keys := imapAuthKeys(ip, username)
	if !a.authBeginKeys(keys) {
		return nil, errors.New("[UNAVAILABLE] Too many failed sign-in attempts. Try again later.")
	}
	ok := err == nil && u.Status == "active" && a.checkPasswordCached(u, password)
	if err != nil {
		CheckPassword(dummyHash, password)
	}
	if !ok {
		imapRememberFailure(failKey)
		a.logMail("auth", username, "", "", "failed", "Mail app (IMAP) sign-in failed", ip)
		return nil, errWrong
	}
	a.authRefund(keys)
	if u.MustChangePassword {
		return nil, errors.New("[AUTHENTICATIONFAILED] Sign in to the web app once to choose a new password, then try again")
	}
	imapState.mu.Lock()
	if imapState.users == nil {
		imapState.users = map[int64]int{}
	}
	if imapState.users[u.ID] >= imapMaxConnsPerUser {
		imapState.mu.Unlock()
		return nil, errors.New("[LIMIT] Too many mail app connections for this account")
	}
	imapState.users[u.ID]++
	imapState.mu.Unlock()
	if info != nil {
		imapSignedIn(info.RemoteAddr)
	}
	return &imapUser{a: a, u: u, loginAt: nowMs(), checked: time.Now()}, nil
}

// ---------------------------------------------------------------- user (one per connection)

type imapUser struct {
	a       *App
	u       *User
	loginAt int64

	mu       sync.Mutex
	selected *imapMailbox // bound to this connection by SELECT/EXAMINE
	checked  time.Time
	gone     bool
	closed   bool
}

var errIMAPSessionEnded = backend.ErrSessionEnded

// alive re-checks (at most once a minute) that the account still exists, is active and that
// the password hasn't changed since this connection signed in.
func (s *imapUser) alive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return errIMAPSessionEnded
	}
	if time.Since(s.checked) < imapAliveEvery {
		return nil
	}
	s.checked = time.Now()
	var status string
	var changed int64
	err := s.a.DB.QueryRow(`SELECT status, password_changed_at FROM users WHERE id=?`, s.u.ID).Scan(&status, &changed)
	if err != nil || status != "active" || changed > s.loginAt {
		s.gone = true
		return errIMAPSessionEnded
	}
	return nil
}

func (s *imapUser) Username() string { return s.u.Email }

func (s *imapUser) Logout() error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if !already {
		imapState.mu.Lock()
		if imapState.users[s.u.ID]--; imapState.users[s.u.ID] <= 0 {
			delete(imapState.users, s.u.ID)
		}
		imapState.mu.Unlock()
	}
	return nil
}

// imapSystemFolders: name in mail apps, our key, special-use attribute.
var imapSystemFolders = []struct{ name, key, attr string }{
	{"INBOX", "inbox", ""},
	{"Sent", "sent", imap.SentAttr},
	{"Drafts", imapDraftsKey, imap.DraftsAttr},
	{"Archive", "archive", imap.ArchiveAttr},
	{"Spam", "spam", imap.JunkAttr},
	{"Trash", "trash", imap.TrashAttr},
}

// resolve maps a mailbox name to our folder key and its canonical name.
func (s *imapUser) resolve(name string) (key, canonical string, err error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), folderDelimiter)
	for _, f := range imapSystemFolders {
		if strings.EqualFold(name, f.name) {
			return f.key, f.name, nil
		}
	}
	if strings.EqualFold(name, "Junk") {
		return "spam", "Spam", nil
	}
	f, err := s.a.folderByName(s.u.ID, name)
	if err != nil {
		return "", "", backend.ErrNoSuchMailbox
	}
	return f.Key, f.Name, nil
}

func (s *imapUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	var out []backend.Mailbox
	for _, f := range imapSystemFolders {
		out = append(out, &imapMailbox{s: s, key: f.key, name: f.name, attrs: attrList(f.attr, imap.HasNoChildrenAttr)})
	}
	folders, err := s.a.ListFolders(s.u.ID)
	if err != nil {
		return nil, err
	}
	for i, f := range folders {
		child := imap.HasNoChildrenAttr
		if i+1 < len(folders) && strings.HasPrefix(strings.ToLower(folders[i+1].Name), strings.ToLower(f.Name)+folderDelimiter) {
			child = imap.HasChildrenAttr
		}
		out = append(out, &imapMailbox{s: s, key: f.Key, name: f.Name, attrs: []string{child}})
	}
	return out, nil
}

func attrList(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (s *imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	key, canonical, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	return &imapMailbox{s: s, key: key, name: canonical}, nil
}

func (s *imapUser) CreateMailbox(name string) error {
	if err := s.alive(); err != nil {
		return err
	}
	name = strings.TrimSuffix(strings.TrimSpace(name), folderDelimiter)
	if _, _, err := s.resolve(name); err == nil {
		return backend.ErrMailboxAlreadyExists
	}
	_, err := s.a.CreateFolder(s.u.ID, name)
	return err
}

func (s *imapUser) DeleteMailbox(name string) error {
	if err := s.alive(); err != nil {
		return err
	}
	key, _, err := s.resolve(name)
	if err != nil {
		return err
	}
	if !isCustomFolder(key) {
		return errors.New("Built-in folders can't be deleted")
	}
	return s.a.DeleteFolder(s.u.ID, key) // its mail moves to Archive
}

func (s *imapUser) RenameMailbox(existingName, newName string) error {
	if err := s.alive(); err != nil {
		return err
	}
	key, _, err := s.resolve(existingName)
	if err != nil {
		return err
	}
	if !isCustomFolder(key) {
		return errors.New("Built-in folders can't be renamed")
	}
	if k2, _, err := s.resolve(newName); err == nil && k2 != key {
		return backend.ErrMailboxAlreadyExists
	}
	_, err = s.a.RenameFolder(s.u.ID, key, strings.TrimSuffix(strings.TrimSpace(newName), folderDelimiter))
	return err
}

// GET /api/mail-apps — what a person needs to add their account to a mail app.
func (a *App) handleMailApps(w http.ResponseWriter, r *http.Request, u *User) {
	imapState.mu.Lock()
	imapTLS, imapStartTLS := imapState.ports[true], imapState.ports[false]
	imapState.mu.Unlock()
	port := func(addr string) string {
		if addr == "" || addr == "off" {
			return ""
		}
		_, p, err := net.SplitHostPort(addr)
		if err != nil {
			return ""
		}
		return p
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host":          a.Hostname(),
		"username":      u.Email,
		"imap_ssl":      imapTLS,      // "" when not listening
		"imap_starttls": imapStartTLS, // "" when not listening
		"smtp_ssl":      port(a.Cfg.SubmissionsAddr),
		"smtp_starttls": port(a.Cfg.SubmissionAddr),
		"tls_mode":      a.TLS.Mode(),
	})
}

// imapSlowCommand: commands taking longer are logged (IDLE waits by design and isn't counted).
var imapSlowCommand = 2 * time.Second

func (a *App) imapCommandDone(c imapserver.Conn, name string, took time.Duration) {
	if took < imapSlowCommand || strings.EqualFold(name, "IDLE") {
		return
	}
	user := ""
	if u := c.Context().User; u != nil {
		user = u.Username()
	}
	a.Log.Warn("slow mail-app command", "user", user, "command", name, "ms", took.Milliseconds())
}
