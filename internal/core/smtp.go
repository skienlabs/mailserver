package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type smtpServers struct {
	mu      sync.Mutex
	servers []*smtp.Server
}

var smtpState smtpServers

func (a *App) newSMTPServer(be smtp.Backend, addr string) *smtp.Server {
	s := smtp.NewServer(be)
	s.Addr = addr
	s.Domain = a.Hostname()
	s.MaxMessageBytes = a.MaxMessageBytes()
	s.MaxRecipients = 100
	s.ReadTimeout = 5 * time.Minute
	s.WriteTimeout = 5 * time.Minute
	s.TLSConfig = a.TLS.ServerConfig()
	s.AllowInsecureAuth = false
	s.ErrorLog = log.New(io.Discard, "", 0)
	if a.Cfg.LogLevelDebug {
		s.ErrorLog = log.Default()
	}
	return s
}

func (a *App) startSMTP() error {
	smtpState.mu.Lock()
	defer smtpState.mu.Unlock()
	type lst struct {
		srv *smtp.Server
		ln  net.Listener
	}
	var started []lst
	listen := func(srv *smtp.Server, implicitTLS bool) error {
		if srv.Addr == "" || srv.Addr == "off" {
			return nil
		}
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		if implicitTLS {
			ln = tls.NewListener(ln, srv.TLSConfig)
		}
		started = append(started, lst{srv, ln})
		return nil
	}
	mx := a.newSMTPServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return a.newSession(c, false) }), a.Cfg.SMTPAddr)
	sub := a.newSMTPServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return a.newSession(c, true) }), a.Cfg.SubmissionAddr)
	subs := a.newSMTPServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return a.newSession(c, true) }), a.Cfg.SubmissionsAddr)
	for _, x := range []struct {
		s   *smtp.Server
		tls bool
	}{{mx, false}, {sub, false}, {subs, true}} {
		if err := listen(x.s, x.tls); err != nil {
			for _, l := range started {
				l.ln.Close()
			}
			return err
		}
	}
	for _, l := range started {
		smtpState.servers = append(smtpState.servers, l.srv)
		go func(l lst) {
			if err := l.srv.Serve(l.ln); err != nil && !errors.Is(err, smtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				a.Log.Error("smtp server stopped", "addr", l.srv.Addr, "err", err)
			}
		}(l)
		a.Log.Info("smtp listening", "addr", l.srv.Addr)
	}
	return nil
}

func (a *App) stopSMTP(ctx context.Context) {
	smtpState.mu.Lock()
	servers := smtpState.servers
	smtpState.servers = nil
	smtpState.mu.Unlock()
	for _, s := range servers {
		s.Shutdown(ctx)
	}
}

// RestartSMTP re-creates listeners (after hostname / size changes).
func (a *App) RestartSMTP() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.stopSMTP(ctx)
	if err := a.startSMTP(); err != nil {
		a.Log.Error("restart smtp", "err", err)
	}
}

// ---------------------------------------------------------------- sessions

type session struct {
	a          *App
	conn       *smtp.Conn
	ip         net.IP
	submission bool
	user       *User

	from       string
	size       int64    // SIZE= from MAIL FROM, when the client declared it
	localRcpts []string // addresses as given
	userIDs    []int64  // resolved local mailboxes (deduplicated)
	remote     []string // submission only
}

func (a *App) newSession(c *smtp.Conn, submission bool) (smtp.Session, error) {
	var ip net.IP
	if ta, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		ip = ta.IP
	}
	if !a.limiter.Allow("smtpconn:"+ip.String(), 120, time.Minute) {
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many connections, slow down"}
	}
	s := &session{a: a, conn: c, ip: ip, submission: submission}
	if submission {
		return &authSession{s}, nil
	}
	return s, nil
}

// authSession adds AUTH support for submission ports.
type authSession struct{ *session }

func (s *authSession) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

func (s *authSession) Auth(mech string) (sasl.Server, error) {
	check := func(username, password string) error {
		username = strings.ToLower(strings.TrimSpace(username))
		if !s.a.authBegin(s.ip.String(), username) {
			return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many failed attempts, try later"}
		}
		u, err := s.a.UserByEmail(username)
		ok := err == nil && u.Status == "active" && CheckPassword(u.passwordHash, password)
		if err != nil {
			CheckPassword(dummyHash, password)
		}
		if !ok {
			s.a.logMail("auth", username, "", "", "failed", "SMTP authentication failed", s.ip.String())
			return smtp.ErrAuthFailed
		}
		s.a.authSucceeded(s.ip.String(), username)
		s.user = u
		return nil
	}
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && !strings.EqualFold(identity, username) {
				return smtp.ErrAuthFailed
			}
			return check(username, password)
		}), nil
	case sasl.Login:
		return &loginServer{auth: check}, nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

// loginServer implements the (obsolete but widely used) LOGIN mechanism.
type loginServer struct {
	step int
	user string
	auth func(u, p string) error
}

func (l *loginServer) Next(resp []byte) ([]byte, bool, error) {
	switch l.step {
	case 0:
		if len(resp) > 0 {
			l.user, l.step = string(resp), 2
			return []byte("Password:"), false, nil
		}
		l.step = 1
		return []byte("Username:"), false, nil
	case 1:
		l.user, l.step = string(resp), 2
		return []byte("Password:"), false, nil
	default:
		return nil, true, l.auth(l.user, string(resp))
	}
}

func (s *session) Reset() {
	s.from, s.size = "", 0
	s.localRcpts, s.userIDs, s.remote = nil, nil, nil
}

func (s *session) Logout() error { return nil }

func smtpErr(code int, ec smtp.EnhancedCode, msg string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: ec, Message: msg}
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.Reset()
	if s.submission {
		if s.user == nil {
			return smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Authentication required")
		}
		if _, ok := s.a.CanSendAs(s.user, from); !ok {
			return smtpErr(553, smtp.EnhancedCode{5, 7, 1}, "You are not allowed to send as "+from)
		}
	}
	if opts != nil && opts.Size > s.a.MaxMessageBytes() {
		return smtpErr(552, smtp.EnhancedCode{5, 3, 4}, "Message too big")
	}
	s.from = strings.ToLower(strings.TrimSpace(from))
	if opts != nil {
		s.size = opts.Size
	}
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	to = strings.ToLower(strings.TrimSpace(to))
	if _, _, ok := SplitAddress(to); !ok {
		return smtpErr(501, smtp.EnhancedCode{5, 1, 3}, "Bad recipient address")
	}
	ids, local, err := s.a.ResolveLocal(to)
	if !local {
		if s.submission {
			s.remote = append(s.remote, to)
			return nil
		}
		return smtpErr(554, smtp.EnhancedCode{5, 7, 1}, "Relay access denied")
	}
	if errors.Is(err, ErrNoSuchUser) {
		if !s.submission {
			s.a.logMail("in", s.from, to, "", "rejected", "unknown recipient", s.ip.String())
		}
		return smtpErr(550, smtp.EnhancedCode{5, 1, 1}, "No such user here")
	}
	if err != nil {
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary lookup failure")
	}
	// Refuse a full personal mailbox here, during the SMTP dialogue, so the sending
	// server reports it. Accepting and bouncing later would send "backscatter" to
	// forged senders. (Groups still accept; full members are skipped silently.)
	if !s.submission && len(ids) == 1 && s.a.mailboxFull(ids[0], s.size) {
		s.a.logMail("in", s.from, to, "", "rejected", "mailbox full", s.ip.String())
		return smtpErr(452, smtp.EnhancedCode{4, 2, 2}, "Mailbox full, try again later")
	}
	s.localRcpts = append(s.localRcpts, to)
	for _, id := range ids {
		dup := false
		for _, x := range s.userIDs {
			if x == id {
				dup = true
			}
		}
		if !dup {
			s.userIDs = append(s.userIDs, id)
		}
	}
	return nil
}

func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(r, s.a.MaxMessageBytes()+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > s.a.MaxMessageBytes() {
		return smtpErr(552, smtp.EnhancedCode{5, 3, 4}, "Message too big")
	}
	if MIMETooComplex(raw) {
		s.a.logMail("in", s.from, strings.Join(s.localRcpts, ", "), "", "rejected", "MIME structure too complex", s.ip.String())
		return smtpErr(552, smtp.EnhancedCode{5, 6, 0}, "Message structure too complex")
	}
	if s.submission {
		return s.dataSubmission(raw)
	}
	return s.dataInbound(raw)
}

func (s *session) receivedHeader(rcpt string) string {
	proto := "ESMTP"
	if _, ok := s.conn.TLSConnectionState(); ok {
		proto = "ESMTPS"
	}
	if s.submission {
		proto += "A"
	}
	helo := s.conn.Hostname()
	for_ := ""
	if rcpt != "" {
		for_ = "\r\n\tfor <" + rcpt + ">"
	}
	return fmt.Sprintf("Received: from %s ([%s])\r\n\tby %s (%s) with %s id %s%s;\r\n\t%s\r\n",
		sanitizeHeaderValue(helo), s.ip, s.a.Hostname(), ProductName, proto, RandomHex(6), for_, time.Now().Format(time.RFC1123Z))
}

// sanitizeHeaderValue removes control characters (CR/LF header injection) and caps
// the length at 500 characters without splitting a multi-byte character.
func sanitizeHeaderValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return truncateRunes(s, 500)
}

// truncateRunes shortens s to at most n characters (not bytes).
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func (s *session) dataInbound(raw []byte) error {
	if len(s.userIDs) == 0 {
		return smtpErr(554, smtp.EnhancedCode{5, 5, 1}, "No valid recipients")
	}
	meta := ParseMeta(raw)
	v := s.a.CheckInbound(s.ip, s.conn.Hostname(), s.from, raw, &meta)
	rcptList := strings.Join(s.localRcpts, ", ")
	if v.Reject {
		s.a.logMail("in", s.from, rcptList, meta.Subject, "rejected", v.RejectMsg+" ("+v.Summary+")", s.ip.String())
		return smtpErr(550, smtp.EnhancedCode{5, 7, 1}, v.RejectMsg)
	}
	folder := "inbox"
	if v.Quarantine {
		folder = "spam"
	}
	rcptHdr := ""
	if len(s.localRcpts) == 1 {
		rcptHdr = s.localRcpts[0]
	}
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "Return-Path: <%s>\r\n", sanitizeHeaderValue(s.from))
	fmt.Fprintf(&hdr, "Authentication-Results: %s\r\n", v.Header)
	if v.Quarantine {
		fmt.Fprintf(&hdr, "X-Spam-Flag: YES\r\nX-Spam-Score: %.1f\r\n", v.Score)
	}
	hdr.WriteString(s.receivedHeader(rcptHdr))
	final := append(hdr.Bytes(), raw...)

	full := 0
	delivered := 0
	for _, uid := range s.userIDs {
		_, err := s.a.Deliver(uid, final, DeliverOpts{Folder: folder, SpamScore: v.Score, AuthSummary: v.Summary, Meta: &meta, Notify: folder == "inbox", EnforceQuota: true})
		if errors.Is(err, ErrQuota) {
			full++ // a group member over quota: skipped (no bounce to a possibly forged sender)
			continue
		}
		if err != nil {
			s.a.Log.Error("deliver", "err", err)
			if delivered == 0 {
				return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary local problem, try again later")
			}
			continue
		}
		delivered++
		if folder == "inbox" {
			s.a.maybeVacation(uid, s.from, &meta)
		}
	}
	if delivered == 0 && full > 0 {
		s.a.logMail("in", s.from, rcptList, meta.Subject, "rejected", "mailbox full", s.ip.String())
		return smtpErr(452, smtp.EnhancedCode{4, 2, 2}, "Mailbox full, try again later")
	}
	status := "delivered"
	if folder == "spam" {
		status = "spam"
	}
	s.a.logMail("in", s.from, rcptList, meta.Subject, status, v.Summary, s.ip.String())
	return nil
}

func (s *session) dataSubmission(raw []byte) error {
	if s.user == nil {
		return smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Authentication required")
	}
	raw = stripHeader(raw, "Bcc")
	// Exactly one author, and it must be an address this user may send as. Several From
	// headers or mailboxes would let a mail app display a colleague as the sender.
	from, err := SingleFrom(raw)
	if err != nil {
		return smtpErr(550, smtp.EnhancedCode{5, 6, 0}, err.Error())
	}
	if _, ok := s.a.CanSendAs(s.user, from.Email); !ok {
		return smtpErr(553, smtp.EnhancedCode{5, 7, 1}, "You are not allowed to send as "+from.Email)
	}
	if senders, n := HeaderAddresses(raw, "Sender"); n > 0 {
		if n != 1 || len(senders) != 1 {
			return smtpErr(550, smtp.EnhancedCode{5, 6, 0}, "the Sender header must contain exactly one address")
		}
		if _, ok := s.a.CanSendAs(s.user, senders[0].Email); !ok {
			return smtpErr(553, smtp.EnhancedCode{5, 7, 1}, "You are not allowed to send as "+senders[0].Email)
		}
	}
	meta := ParseMeta(raw)
	if !s.a.limiter.Allow(fmt.Sprintf("send:%d", s.user.ID), 400, time.Hour) {
		return smtpErr(450, smtp.EnhancedCode{4, 7, 1}, "Sending limit reached, try again later")
	}
	final := append([]byte(s.receivedHeader("")), raw...)
	rcpts := append(append([]string{}, s.localRcpts...), s.remote...)
	if err := s.a.Submit(SubmitReq{User: s.user, EnvFrom: s.from, Raw: final, Rcpts: rcpts, Meta: &meta, SaveSent: true, Via: "smtp"}); err != nil {
		s.a.Log.Error("submission", "err", err)
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary problem, try again later")
	}
	return nil
}

// stripHeader removes every occurrence of a header (with folded lines) from a raw message.
func stripHeader(raw []byte, name string) []byte {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	sep := 4
	if end < 0 {
		end = bytes.Index(raw, []byte("\n\n"))
		sep = 2
	}
	if end < 0 {
		return raw
	}
	head := raw[:end]
	lines := bytes.SplitAfter(head, []byte("\n"))
	var out bytes.Buffer
	skipping := false
	prefix := strings.ToLower(name) + ":"
	for _, l := range lines {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') {
			if skipping {
				continue
			}
		} else {
			skipping = strings.HasPrefix(strings.ToLower(string(l)), prefix)
			if skipping {
				continue
			}
		}
		out.Write(l)
	}
	nl := "\r\n"
	if sep == 2 {
		nl = "\n"
	}
	res := out.Bytes()
	if !bytes.HasSuffix(res, []byte("\n")) {
		res = append(res, nl...)
	}
	// re-attach the blank separator line and the body
	res = append(res, raw[end+len(nl):]...)
	return res
}
