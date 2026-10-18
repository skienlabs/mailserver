package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"golang.org/x/net/idna"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const queueMaxAge = 4 * 24 * time.Hour

var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour,
	2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 8 * time.Hour}

// The outbound queue lives in the database (queue + queue_rcpts), so nothing is lost on a
// restart or crash: every recipient stays "pending"/"deferred" until a remote server accepted
// it, it failed permanently, or it expired after 4 days (then the sender gets a bounce).
//
// A dispatcher hands due recipients to a pool of workers (SM_QUEUE_WORKERS, default 16).
// Recipients of one message at the same destination share one SMTP session, at most
// SM_QUEUE_PER_DOMAIN sessions (default 4) run against one destination at a time, and a slow
// server never holds up mail to other destinations. Every attempt is logged for the admin.

type queueDispatcher struct {
	mu        sync.Mutex
	inflight  map[int64]bool // recipient rows being delivered
	perDest   map[string]int // sessions running per destination
	workers   int
	perDomain int
	slots     chan struct{}
	// send replaces real SMTP delivery in tests.
	send func(ctx context.Context, domain, from string, rcpts []string, raw []byte) (map[string]error, string)
}

func newQueueDispatcher() *queueDispatcher {
	w := envIntRange("SM_QUEUE_WORKERS", 16, 1, 256)
	return &queueDispatcher{
		inflight: map[int64]bool{}, perDest: map[string]int{},
		workers: w, perDomain: envIntRange("SM_QUEUE_PER_DOMAIN", 4, 1, 64),
		slots: make(chan struct{}, w),
	}
}

func envIntRange(key string, def, lo, hi int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v >= lo && v <= hi {
		return v
	}
	return def
}

// QueueActive reports recipients being delivered right now.
func (a *App) QueueActive() int {
	a.qd.mu.Lock()
	defer a.qd.mu.Unlock()
	return len(a.qd.inflight)
}

func (a *App) queueWorker(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.dispatchQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.queueKick:
		}
	}
}

type dueRcpt struct {
	id       int64
	queueID  int64
	rcpt     string
	domain   string
	attempts int
}

// dispatchQueue starts deliveries for due recipients until workers or destinations are busy.
func (a *App) dispatchQueue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := a.DB.Query(`SELECT id, queue_id, rcpt, domain, attempts FROM queue_rcpts
		WHERE status IN ('pending','deferred') AND next_attempt <= ? ORDER BY next_attempt, id LIMIT 2000`, nowMs())
	if err != nil {
		a.Log.Error("queue scan", "err", err)
		return
	}
	groups := map[string][]dueRcpt{}
	var order []string
	relay := a.Settings.Get(SetRelayHost) != ""
	qd := a.qd
	qd.mu.Lock()
	for rows.Next() {
		var d dueRcpt
		if err := rows.Scan(&d.id, &d.queueID, &d.rcpt, &d.domain, &d.attempts); err != nil || qd.inflight[d.id] {
			continue
		}
		key := fmt.Sprintf("%d|%s", d.queueID, strings.ToLower(d.domain))
		if relay {
			key = fmt.Sprintf("%d|*", d.queueID)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], d)
	}
	qd.mu.Unlock()
	rows.Close()
	for _, k := range order {
		list := groups[k]
		dest := strings.ToLower(list[0].domain)
		if relay {
			dest = "*relay*"
		}
		qd.mu.Lock()
		if qd.perDest[dest] >= qd.perDomain {
			qd.mu.Unlock()
			continue // this destination is busy; other destinations still go
		}
		select {
		case qd.slots <- struct{}{}:
		default:
			qd.mu.Unlock()
			return // all workers busy; a finishing delivery kicks the next round
		}
		qd.perDest[dest]++
		for _, d := range list {
			qd.inflight[d.id] = true
		}
		qd.mu.Unlock()
		a.Go(func(ctx context.Context) {
			defer func() {
				qd.mu.Lock()
				qd.perDest[dest]--
				if qd.perDest[dest] <= 0 {
					delete(qd.perDest, dest)
				}
				for _, d := range list {
					delete(qd.inflight, d.id)
				}
				qd.mu.Unlock()
				<-qd.slots
				a.KickQueue()
			}()
			a.deliverGroup(ctx, list)
		})
	}
}

func (a *App) deliverGroup(ctx context.Context, list []dueRcpt) {
	// The dispatcher's scan can be a moment stale: a recipient another worker just finished
	// (or an admin just stopped) may still look due in it. Re-read each one now that it's
	// claimed, so nothing is ever sent twice.
	fresh := list[:0:0]
	now := nowMs()
	for _, d := range list {
		var status string
		var next int64
		if err := a.DB.QueryRow(`SELECT status, next_attempt, attempts FROM queue_rcpts WHERE id=?`, d.id).Scan(&status, &next, &d.attempts); err != nil {
			continue
		}
		if (status == "pending" || status == "deferred") && next <= now {
			fresh = append(fresh, d)
		}
	}
	if len(fresh) == 0 {
		return
	}
	list = fresh
	qid := list[0].queueID
	var sender, subject, blob string
	var userID, created int64
	if err := a.DB.QueryRow(`SELECT sender, subject, blob, user_id, created_at FROM queue WHERE id=?`, qid).Scan(&sender, &subject, &blob, &userID, &created); err != nil {
		return
	}
	if ctx.Err() != nil {
		return // shutting down: stays queued
	}
	raw, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, "queue", blob))
	if blob == "" || err != nil {
		for _, d := range list {
			a.finishRcpt(d, "failed", "queued message file missing")
			a.logAttempt(d, "", "failed", "queued message file missing", 0)
		}
		return
	}
	rcpts := make([]string, len(list))
	for i, d := range list {
		rcpts[i] = d.rcpt
	}
	start := time.Now()
	var results map[string]error
	var host string
	switch {
	case a.qd.send != nil:
		results, host = a.qd.send(ctx, list[0].domain, sender, rcpts, raw)
	case a.Settings.Get(SetRelayHost) != "":
		results, host = a.sendViaRelay(ctx, a.Settings.Get(SetRelayHost), sender, rcpts, raw)
	default:
		results, host = a.sendDirect(ctx, list[0].domain, sender, rcpts, raw)
	}
	took := time.Since(start)
	// Shutting down mid-delivery: what the server accepted or refused for good is recorded as
	// usual (so it's never sent again); only recipients without a final answer stay queued as
	// they were, with no attempt counted.
	stopping := ctx.Err() != nil
	failures := map[string]string{}
	expired := time.Since(time.UnixMilli(created)) > queueMaxAge
	for _, d := range list {
		err, known := results[d.rcpt]
		if !known {
			err = tempErr("no result from %s", firstNonEmpty(host, d.domain))
		}
		if stopping && err != nil && !isPermanent(err) {
			continue
		}
		switch {
		case err == nil:
			a.finishRcpt(d, "sent", "")
			a.logAttempt(d, host, "sent", "", took)
			a.logMail("out", sender, d.rcpt, subject, "sent", "", "")
		case isPermanent(err) || expired:
			msg := err.Error()
			if expired && !isPermanent(err) {
				msg = "Gave up after 4 days of retrying: " + msg
			}
			a.finishRcpt(d, "failed", msg)
			a.logAttempt(d, host, "failed", msg, took)
			a.logMail("out", sender, d.rcpt, subject, "failed", msg, "")
			failures[d.rcpt] = msg
		default:
			n := d.attempts
			if n >= len(backoff) {
				n = len(backoff) - 1
			}
			next := time.Now().Add(backoff[n])
			a.DB.Exec(`UPDATE queue_rcpts SET status='deferred', attempts=attempts+1, next_attempt=?, last_error=?, updated_at=? WHERE id=?`,
				next.UnixMilli(), truncate(err.Error(), 500), nowMs(), d.id)
			a.logAttempt(d, host, "deferred", err.Error(), took)
			a.logMail("out", sender, d.rcpt, subject, "deferred", err.Error(), "")
		}
	}
	if len(failures) > 0 {
		a.Bounce(sender, raw, failures)
	}
	if userID > 0 {
		a.Events.Publish(userID, Event{Type: "queue"})
	}
}

// logAttempt records one delivery attempt for the admin's queue log.
func (a *App) logAttempt(d dueRcpt, host, result, detail string, took time.Duration) {
	a.DB.Exec(`INSERT INTO queue_attempts(queue_id, rcpt_id, rcpt, at, host, result, detail, ms) VALUES(?,?,?,?,?,?,?,?)`,
		d.queueID, d.id, d.rcpt, nowMs(), host, result, truncate(detail, 500), took.Milliseconds())
}

func truncate(s string, n int) string { return truncateRunes(strings.ToValidUTF8(s, ""), n) }

func (a *App) finishRcpt(d dueRcpt, status, msg string) {
	a.DB.Exec(`UPDATE queue_rcpts SET status=?, attempts=attempts+1, last_error=?, updated_at=? WHERE id=?`, status, truncate(msg, 500), nowMs(), d.id)
}

// deliveryError carries an SMTP reply or connection problem.
type deliveryError struct {
	perm bool
	msg  string
}

func (e *deliveryError) Error() string { return e.msg }

func isPermanent(err error) bool {
	var de *deliveryError
	return errors.As(err, &de) && de.perm
}

func tempErr(format string, args ...any) error {
	return &deliveryError{msg: fmt.Sprintf(format, args...)}
}

func smtpReplyErr(host string, err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		return &deliveryError{perm: te.Code >= 500, msg: fmt.Sprintf("%s said: %d %s", host, te.Code, te.Msg)}
	}
	return tempErr("%s: %v", host, err)
}

func allErr(rcpts []string, err error) map[string]error {
	m := make(map[string]error, len(rcpts))
	for _, r := range rcpts {
		m[r] = err
	}
	return m
}

// sendDirect delivers to the recipient domain's MX hosts.
func (a *App) sendDirect(ctx context.Context, domain, from string, rcpts []string, raw []byte) (map[string]error, string) {
	if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
		domain = ascii // the resolver only takes ASCII names
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	mxs, err := net.DefaultResolver.LookupMX(lctx, domain)
	cancel()
	var hosts []string
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			// No MX: fall back to the domain's address record (RFC 5321 §5.1)
			actx, cancel := context.WithTimeout(ctx, 20*time.Second)
			addrs, aerr := net.DefaultResolver.LookupHost(actx, domain)
			cancel()
			if aerr != nil || len(addrs) == 0 {
				return allErr(rcpts, &deliveryError{perm: true, msg: "Domain " + domain + " does not exist or has no mail server"}), ""
			}
			hosts = []string{domain}
		} else {
			return allErr(rcpts, tempErr("DNS lookup for %s failed: %v", domain, err)), ""
		}
	} else {
		sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
		for _, mx := range mxs {
			h := strings.TrimSuffix(mx.Host, ".")
			if h == "" {
				return allErr(rcpts, &deliveryError{perm: true, msg: domain + " does not accept email (null MX)"}), ""
			}
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > 5 {
		hosts = hosts[:5]
	}
	var lastErr error = tempErr("no mail servers for %s", domain)
	lastHost := ""
	for _, h := range hosts {
		res, connErr := a.smtpTransaction(ctx, h, "25", false, false, nil, from, rcpts, raw)
		if connErr == nil {
			return res, h
		}
		lastErr, lastHost = connErr, h
		if isPermanent(connErr) || ctx.Err() != nil {
			return allErr(rcpts, connErr), h
		}
	}
	return allErr(rcpts, lastErr), lastHost
}

// sendViaRelay delivers everything through the configured smarthost.
func (a *App) sendViaRelay(ctx context.Context, host, from string, rcpts []string, raw []byte) (map[string]error, string) {
	port := a.Settings.Get(SetRelayPort)
	if port == "" {
		port = "587"
	}
	implicit := a.Settings.Get(SetRelayTLS) == "tls" || port == "465"
	var auth smtp.Auth
	if u := a.Settings.Get(SetRelayUser); u != "" {
		auth = smtp.PlainAuth("", u, a.Settings.Get(SetRelayPass), host)
	}
	res, err := a.smtpTransaction(ctx, host, port, implicit, true, auth, from, rcpts, raw)
	if err != nil {
		return allErr(rcpts, err), host
	}
	return res, host
}

// smtpTransaction runs one SMTP session. A non-nil error means the host could
// not be used at all (try the next MX); per-recipient results are returned otherwise.
func (a *App) smtpTransaction(ctx context.Context, host, port string, implicitTLS, requireTLS bool, auth smtp.Auth,
	from string, rcpts []string, raw []byte) (map[string]error, error) {

	attempt := func(useTLS bool) (map[string]error, error, bool) {
		d := net.Dialer{Timeout: 30 * time.Second}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(host, port))
		cancel()
		if err != nil {
			return nil, tempErr("could not connect to %s: %v", host, err), false
		}
		conn.SetDeadline(time.Now().Add(10 * time.Minute))
		if implicitTLS {
			tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, tempErr("TLS handshake with %s failed: %v", host, err), false
			}
			conn = tc
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return nil, smtpReplyErr(host, err), false
		}
		defer c.Close()
		if err := c.Hello(a.Hostname()); err != nil {
			return nil, smtpReplyErr(host, err), false
		}
		if !implicitTLS && useTLS {
			if ok, _ := c.Extension("STARTTLS"); ok {
				cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
				if !requireTLS {
					// Opportunistic TLS between MTAs: encrypt even when the certificate
					// can't be verified (same as Postfix "may" level).
					cfg.InsecureSkipVerify = true
				}
				if err := c.StartTLS(cfg); err != nil {
					if requireTLS {
						return nil, tempErr("STARTTLS with %s failed: %v", host, err), false
					}
					return nil, err, true // retry without TLS
				}
			} else if requireTLS {
				return nil, tempErr("%s does not support STARTTLS", host), false
			}
		}
		if auth != nil {
			if err := c.Auth(auth); err != nil {
				return nil, smtpReplyErr(host, err), false
			}
		}
		if ok, sz := c.Extension("SIZE"); ok {
			if max, err := strconv.ParseInt(strings.TrimSpace(sz), 10, 64); err == nil && max > 0 && int64(len(raw)) > max {
				return nil, &deliveryError{perm: true, msg: fmt.Sprintf("%s only accepts messages up to %d bytes", host, max)}, false
			}
		}
		if err := c.Mail(from); err != nil {
			return nil, smtpReplyErr(host, err), false
		}
		results := map[string]error{}
		var accepted []string
		for _, r := range rcpts {
			if err := c.Rcpt(r); err != nil {
				results[r] = smtpReplyErr(host, err)
				continue
			}
			accepted = append(accepted, r)
		}
		if len(accepted) == 0 {
			c.Reset()
			c.Quit()
			return results, nil, false
		}
		w, err := c.Data()
		if err != nil {
			e := smtpReplyErr(host, err)
			for _, r := range accepted {
				results[r] = e
			}
			return results, nil, false
		}
		if _, err := w.Write(raw); err != nil {
			w.Close()
			for _, r := range accepted {
				results[r] = tempErr("%s: write failed: %v", host, err)
			}
			return results, nil, false
		}
		if err := w.Close(); err != nil {
			e := smtpReplyErr(host, err)
			for _, r := range accepted {
				results[r] = e
			}
			return results, nil, false
		}
		for _, r := range accepted {
			results[r] = nil
		}
		c.Quit()
		return results, nil, false
	}

	res, err, retryPlain := attempt(true)
	if retryPlain {
		a.Log.Warn("STARTTLS failed, retrying without TLS", "host", host, "err", err)
		res, err, _ = attempt(false)
	}
	return res, err
}
