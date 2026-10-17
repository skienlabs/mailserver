package core

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// SubmitReq is an outgoing message from an authenticated user (or the system).
type SubmitReq struct {
	User     *User  // nil for system messages (bounces, auto-replies)
	EnvFrom  string // envelope sender; "" for null sender
	Raw      []byte
	Rcpts    []string
	Meta     *Meta
	Bcc      []Addr
	SaveSent bool
	Via      string // web | smtp | system
}

// RecipientError names a local recipient that doesn't exist.
type RecipientError struct{ Addr string }

func (e *RecipientError) Error() string { return "No mailbox exists for " + e.Addr }

// Submit stores the sent copy, delivers to local mailboxes and queues the rest.
func (a *App) Submit(r SubmitReq) error {
	if r.Meta == nil {
		m := ParseMeta(r.Raw)
		r.Meta = &m
	}
	var localIDs []int64
	var localAddrs, remote []string
	seenAddr := map[string]bool{}
	seenID := map[int64]bool{}
	for _, rc := range r.Rcpts {
		rc = strings.ToLower(strings.TrimSpace(rc))
		if rc == "" || seenAddr[rc] {
			continue
		}
		seenAddr[rc] = true
		ids, local, err := a.ResolveLocal(rc)
		if !local {
			remote = append(remote, rc)
			continue
		}
		if errors.Is(err, ErrNoSuchUser) {
			return &RecipientError{Addr: rc}
		}
		if err != nil {
			return err
		}
		localAddrs = append(localAddrs, rc)
		for _, id := range ids {
			if !seenID[id] {
				seenID[id] = true
				localIDs = append(localIDs, id)
			}
		}
	}

	if r.SaveSent && r.User != nil {
		if _, err := a.Deliver(r.User.ID, r.Raw, DeliverOpts{Folder: "sent", Seen: true, Bcc: r.Bcc, Meta: r.Meta}); err != nil {
			return fmt.Errorf("saving sent copy: %w", err)
		}
	}

	// Local delivery
	if len(localIDs) > 0 {
		local := append([]byte(fmt.Sprintf("Return-Path: <%s>\r\n", sanitizeHeaderValue(r.EnvFrom))), r.Raw...)
		failures := map[string]string{}
		for _, uid := range localIDs {
			_, err := a.Deliver(uid, local, DeliverOpts{Folder: "inbox", Meta: r.Meta, Notify: true, EnforceQuota: true, AuthSummary: "internal"})
			if err != nil {
				reason := "451 4.3.0 local delivery failed"
				if errors.Is(err, ErrQuota) {
					reason = "552 5.2.2 Mailbox full"
				}
				if u, e := a.UserByID(uid); e == nil {
					failures[u.Email] = reason
				}
				a.Log.Error("local delivery", "user", uid, "err", err)
				continue
			}
			if r.Via != "system" {
				a.maybeVacation(uid, r.EnvFrom, r.Meta)
			}
		}
		for _, addr := range localAddrs {
			a.logMail("local", r.EnvFrom, addr, r.Meta.Subject, "delivered", "", r.Via)
		}
		if len(failures) > 0 {
			a.Bounce(r.EnvFrom, r.Raw, failures)
		}
	}

	// Remote delivery
	if len(remote) > 0 {
		signed := a.dkimSign(r.Raw, r.Meta.From.Email)
		var uid int64
		if r.User != nil {
			uid = r.User.ID
		}
		if err := a.enqueue(uid, r.EnvFrom, r.Meta, signed, remote); err != nil {
			return err
		}
	}

	if r.User != nil && r.Via == "web" {
		a.rememberContacts(r.User.ID, append(append(append([]Addr{}, r.Meta.To...), r.Meta.Cc...), r.Bcc...))
	}
	return nil
}

func (a *App) dkimSign(raw []byte, fromAddr string) []byte {
	_, domain, _ := SplitAddress(fromAddr)
	d := a.Domain(domain)
	if d == nil || d.key == nil {
		if p := a.PrimaryDomain(); p != nil && domain == "" {
			d = p
		} else {
			return raw
		}
	}
	var buf bytes.Buffer
	err := dkim.Sign(&buf, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 d.Name,
		Selector:               d.Selector,
		Signer:                 d.key,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys: []string{"From", "Reply-To", "Subject", "Date", "To", "Cc", "Message-ID", "In-Reply-To", "References",
			"MIME-Version", "Content-Type", "Content-Transfer-Encoding", "Auto-Submitted"},
	})
	if err != nil {
		a.Log.Error("dkim sign", "err", err)
		return raw
	}
	return buf.Bytes()
}

func (a *App) enqueue(userID int64, envFrom string, meta *Meta, raw []byte, rcpts []string) error {
	rel := RandomHex(12) + ".eml"
	if err := writeFileSync(filepath.Join(a.Cfg.DataDir, "queue", rel), raw); err != nil {
		return err
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowMs()
	res, err := tx.Exec(`INSERT INTO queue(user_id, sender, subject, message_id, blob, size, created_at) VALUES(?,?,?,?,?,?,?)`,
		userID, envFrom, meta.Subject, meta.MessageID, rel, len(raw), now)
	if err != nil {
		return err
	}
	qid, _ := res.LastInsertId()
	for _, rc := range rcpts {
		_, dom, _ := SplitAddress(rc)
		if _, err := tx.Exec(`INSERT INTO queue_rcpts(queue_id, rcpt, domain, next_attempt, updated_at) VALUES(?,?,?,?,?)`, qid, rc, dom, now, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		os.Remove(filepath.Join(a.Cfg.DataDir, "queue", rel))
		return err
	}
	a.KickQueue()
	return nil
}

// KickQueue wakes the delivery worker.
func (a *App) KickQueue() {
	select {
	case a.queueKick <- struct{}{}:
	default:
	}
}

func (a *App) rememberContacts(userID int64, list []Addr) {
	now := nowMs()
	for _, c := range list {
		if c.Email == "" {
			continue
		}
		a.DB.Exec(`INSERT INTO contacts(user_id, address, name, uses, last_used) VALUES(?,?,?,1,?)
			ON CONFLICT(user_id, address) DO UPDATE SET uses = uses + 1, last_used = excluded.last_used,
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE contacts.name END`, userID, strings.ToLower(c.Email), c.Name, now)
	}
}

// systemFrom is the address used for bounces.
func (a *App) systemFrom() Addr {
	dom := "localhost"
	if d := a.PrimaryDomain(); d != nil {
		dom = d.Name
	}
	return Addr{Name: "Mail Delivery System", Email: "mailer-daemon@" + dom}
}

// Bounce notifies the sender that some recipients failed.
func (a *App) Bounce(to string, original []byte, failures map[string]string) {
	to = strings.ToLower(strings.TrimSpace(to))
	if to == "" || strings.HasPrefix(to, "mailer-daemon@") || len(failures) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("<p>Your message couldn't be delivered to ")
	if len(failures) == 1 {
		sb.WriteString("this recipient")
	} else {
		sb.WriteString("these recipients")
	}
	sb.WriteString(":</p><ul>")
	for rc, why := range failures {
		fmt.Fprintf(&sb, "<li><b>%s</b><br><span style=\"color:#5f6368\">%s</span></li>", html.EscapeString(rc), html.EscapeString(why))
	}
	sb.WriteString("</ul>")
	headEnd := bytes.Index(original, []byte("\r\n\r\n"))
	if headEnd < 0 {
		headEnd = bytes.Index(original, []byte("\n\n"))
	}
	if headEnd > 0 {
		hdrs := original[:headEnd]
		if len(hdrs) > 16*1024 {
			hdrs = hdrs[:16*1024]
		}
		fmt.Fprintf(&sb, "<p style=\"color:#5f6368\">Original message headers:</p><pre style=\"font-size:12px;white-space:pre-wrap;color:#5f6368\">%s</pre>", html.EscapeString(string(hdrs)))
	}
	om := ParseMeta(original)
	c := Compose{
		From:    a.systemFrom(),
		To:      []Addr{{Email: to}},
		Subject: "Delivery Status Notification (Failure)",
		HTML:    sb.String(),
		Extra:   map[string]string{"Auto-Submitted": "auto-replied"},
	}
	if om.MessageID != "" {
		c.InReplyTo = om.MessageID
		c.References = append(om.Refs, om.MessageID)
	}
	raw, _, err := BuildMessage(c, a.Hostname())
	if err != nil {
		a.Log.Error("build bounce", "err", err)
		return
	}
	if err := a.Submit(SubmitReq{EnvFrom: "", Raw: raw, Rcpts: []string{to}, Via: "system"}); err != nil {
		a.Log.Error("send bounce", "err", err)
	}
}

// personallyAddressed reports whether email (or a +tag variant of it) is in To or Cc.
func personallyAddressed(email string, m *Meta) bool {
	for _, a := range append(append([]Addr{}, m.To...), m.Cc...) {
		local, domain, ok := SplitAddress(a.Email)
		if !ok {
			continue
		}
		if i := strings.IndexByte(local, '+'); i > 0 {
			local = local[:i]
		}
		if strings.EqualFold(local+"@"+domain, email) {
			return true
		}
	}
	return false
}

// maybeVacation sends an out-of-office reply (RFC 3834 rules).
func (a *App) maybeVacation(userID int64, sender string, m *Meta) {
	sender = strings.ToLower(strings.TrimSpace(sender))
	if sender == "" {
		return
	}
	u, err := a.UserByID(userID)
	if err != nil || !u.VacationEnabled {
		return
	}
	now := nowMs()
	if (u.VacationStart > 0 && now < u.VacationStart) || (u.VacationEnd > 0 && now > u.VacationEnd) {
		return
	}
	if strings.EqualFold(sender, u.Email) {
		return
	}
	if !personallyAddressed(u.Email, m) {
		return // RFC 3834: don't auto-reply to group/list traffic or Bcc copies
	}
	if m.AutoSubmitted != "" && m.AutoSubmitted != "no" {
		return
	}
	if m.ListID != "" || m.Precedence == "bulk" || m.Precedence == "list" || m.Precedence == "junk" {
		return
	}
	local, _, _ := SplitAddress(sender)
	for _, p := range []string{"mailer-daemon", "noreply", "no-reply", "donotreply", "do-not-reply", "postmaster", "bounce", "notifications"} {
		if strings.HasPrefix(local, p) {
			return
		}
	}
	var last int64
	a.DB.QueryRow(`SELECT sent_at FROM vacation_log WHERE user_id=? AND sender=?`, userID, sender).Scan(&last)
	if last > 0 && now-last < int64(4*24*time.Hour/time.Millisecond) {
		return
	}
	a.DB.Exec(`INSERT INTO vacation_log(user_id, sender, sent_at) VALUES(?,?,?) ON CONFLICT(user_id, sender) DO UPDATE SET sent_at=excluded.sent_at`, userID, sender, now)

	subject := strings.TrimSpace(u.VacationSubject)
	if subject == "" {
		subject = "Re: " + m.Subject
	}
	body := u.VacationBody
	if !strings.Contains(body, "<") {
		body = strings.ReplaceAll(html.EscapeString(body), "\n", "<br>")
	} else {
		body = SanitizeHTML(body, 0)
	}
	c := Compose{
		From:    Addr{Name: u.Name, Email: u.Email},
		To:      []Addr{{Email: sender}},
		Subject: subject,
		HTML:    body,
		Extra:   map[string]string{"Auto-Submitted": "auto-replied", "X-Auto-Response-Suppress": "All"},
	}
	if m.MessageID != "" {
		c.InReplyTo = m.MessageID
		c.References = append(append([]string{}, m.Refs...), m.MessageID)
	}
	raw, _, err := BuildMessage(c, a.Hostname())
	if err != nil {
		return
	}
	go func() {
		if err := a.Submit(SubmitReq{EnvFrom: "", Raw: raw, Rcpts: []string{sender}, Via: "system"}); err != nil {
			a.Log.Error("vacation reply", "err", err)
		}
	}()
}
