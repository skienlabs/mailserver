package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Web Push: new-mail notifications on phones and computers where Skien Mails is closed.
// Standards: RFC 8030 (delivery), RFC 8291 (end-to-end encryption, aes128gcm per RFC 8188) and
// RFC 8292 (VAPID). The push service (Google, Mozilla, Apple, Microsoft) only ever sees an
// encrypted blob; the browser decrypts it and the service worker shows the notification.

const setPushKey = "push_vapid_key" // PKCS#8, base64 — never sent to browsers (not an editable setting)

var (
	pushDelay      = 2 * time.Second // collects a burst of messages into one notification
	pushMaxPerUser = 20
	pushTTL        = 24 * time.Hour // a new-mail notice older than a day isn't worth showing
	// pushTestTransport lets tests deliver to a local server instead of the real push services
	// (endpoints are still checked against the allowlist first).
	pushTestTransport http.RoundTripper
)

// Push services browsers use. Subscriptions pointing anywhere else are refused, so the server
// can't be made to send requests to arbitrary (internal) addresses.
var pushHosts = []string{"fcm.googleapis.com", "android.googleapis.com", "updates.push.services.mozilla.com",
	"push.services.mozilla.com", "web.push.apple.com"}
var pushHostSuffixes = []string{".notify.windows.com", ".push.apple.com"}

func pushEndpointOK(raw string) bool {
	if len(raw) > 1024 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, ok := range pushHosts {
		if h == ok {
			return true
		}
	}
	for _, sfx := range pushHostSuffixes {
		if strings.HasSuffix(h, sfx) && len(h) > len(sfx) {
			return true
		}
	}
	return false
}

// pushBlocked are address ranges a push request must never reach, even if DNS points there.
var pushBlocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b::/96"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func pushAddrOK(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, p := range pushBlocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

type pusher struct {
	a      *App
	client *http.Client

	keyOnce sync.Once
	key     *ecdsa.PrivateKey
	pub     []byte // uncompressed P-256 point (the "applicationServerKey")
	keyErr  error

	mu      sync.Mutex
	pending map[int64]*pushPending
	jwts    map[string]pushJWT // by push service origin
	tested  map[int64]time.Time
	closed  bool
	wg      sync.WaitGroup
	sem     chan struct{}
}

type pushPending struct {
	count   int
	msgID   int64
	from    string
	subject string
}

type pushJWT struct {
	token string
	until time.Time
}

func newPusher(a *App) *pusher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !pushAddrOK(ap.Addr()) {
				return fmt.Errorf("push: refusing to connect to %s", address)
			}
			return nil
		}}
	tr := &http.Transport{
		Proxy:                 nil, // straight to the push service, so the address check applies
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &pusher{
		a: a,
		client: &http.Client{Transport: tr, Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		pending: map[int64]*pushPending{},
		jwts:    map[string]pushJWT{},
		tested:  map[int64]time.Time{},
		sem:     make(chan struct{}, 8),
	}
}

// keys loads the VAPID key pair, creating it on first use.
func (p *pusher) keys() (*ecdsa.PrivateKey, []byte, error) {
	p.keyOnce.Do(func() {
		if s := p.a.Settings.Get(setPushKey); s != "" {
			der, err := base64.StdEncoding.DecodeString(s)
			if err == nil {
				k, err2 := x509.ParsePKCS8PrivateKey(der)
				if ek, ok := k.(*ecdsa.PrivateKey); err2 == nil && ok && ek.Curve == elliptic.P256() {
					p.key = ek
				}
			}
			if p.key == nil {
				p.a.Log.Error("push: stored key unreadable; making a new one (devices must turn notifications on again)")
			}
		}
		if p.key == nil {
			k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				p.keyErr = err
				return
			}
			der, err := x509.MarshalPKCS8PrivateKey(k)
			if err != nil {
				p.keyErr = err
				return
			}
			if err := p.a.Settings.Set(map[string]string{setPushKey: base64.StdEncoding.EncodeToString(der)}); err != nil {
				p.keyErr = err
				return
			}
			// Subscriptions made with an older key can't be used any more.
			p.a.DB.Exec(`DELETE FROM push_subs`)
			p.key = k
		}
		ek, err := p.key.PublicKey.ECDH()
		if err != nil {
			p.keyErr = err
			return
		}
		p.pub = ek.Bytes()
	})
	return p.key, p.pub, p.keyErr
}

func (p *pusher) publicKey() string {
	_, pub, err := p.keys()
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(pub)
}

// vapidAuth is the Authorization header for one push service origin (RFC 8292).
func (p *pusher) vapidAuth(endpoint string) (string, error) {
	key, pub, err := p.keys()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	aud := u.Scheme + "://" + u.Host
	p.mu.Lock()
	j, ok := p.jwts[aud]
	p.mu.Unlock()
	if !ok || time.Now().After(j.until) {
		exp := time.Now().Add(12 * time.Hour)
		claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": "https://" + p.a.Hostname()})
		tok, err := signES256(key, []byte(`{"typ":"JWT","alg":"ES256"}`), claims)
		if err != nil {
			return "", err
		}
		j = pushJWT{token: tok, until: exp.Add(-time.Hour)}
		p.mu.Lock()
		p.jwts[aud] = j
		p.mu.Unlock()
	}
	return "vapid t=" + j.token + ", k=" + base64.RawURLEncoding.EncodeToString(pub), nil
}

func signES256(key *ecdsa.PrivateKey, header, claims []byte) (string, error) {
	in := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	h := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// pushEncrypt encrypts a message for one subscription (RFC 8291 / RFC 8188 aes128gcm, single
// record). asKey and salt are random per message; they're parameters only so tests can check
// against the RFC's worked example.
func pushEncrypt(uaPublic, authSecret, plaintext []byte, asKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	const rs = 4096
	if len(plaintext)+1+16 > rs-86 {
		return nil, errors.New("push: message too large")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("push: bad subscription key: %w", err)
	}
	if len(authSecret) != 16 || len(salt) != 16 {
		return nil, errors.New("push: bad auth secret or salt")
	}
	secret, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPublic := asKey.PublicKey().Bytes()
	// IKM = HKDF(auth_secret, ecdh_secret, "WebPush: info" || 0x00 || ua_public || as_public, 32)
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, string(info), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	padded := append(append([]byte{}, plaintext...), 0x02) // 0x02: last (only) record, no padding
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(rs))
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	out.Write(gcm.Seal(nil, nonce, padded, nil))
	return out.Bytes(), nil
}

// pushSub is one device.
type pushSub struct {
	ID       int64
	UserID   int64
	Endpoint string
	P256dh   string
	Auth     string
	Preview  bool
}

// pushMessage is what the service worker receives.
type pushMessage struct {
	Type   string `json:"type"` // "mail" | "test"
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Unread int    `json:"unread"`
}

// deliver sends one message to one device. It returns the push service's HTTP status.
func (p *pusher) deliver(ctx context.Context, sub pushSub, msg pushMessage) (int, error) {
	if !pushEndpointOK(sub.Endpoint) {
		return 0, errors.New("push: endpoint not allowed")
	}
	ua, err1 := b64urlDecode(sub.P256dh)
	auth, err2 := b64urlDecode(sub.Auth)
	if err1 != nil || err2 != nil {
		return 0, errors.New("push: bad subscription keys")
	}
	plain, _ := json.Marshal(msg)
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	body, err := pushEncrypt(ua, auth, plain, asKey, salt)
	if err != nil {
		return 0, err
	}
	authz, err := p.vapidAuth(sub.Endpoint)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", fmt.Sprint(int(pushTTL.Seconds())))
	req.Header.Set("Urgency", "high")  // "normal" can be held back for a long time on sleeping phones
	req.Header.Set("Topic", "newmail") // an undelivered earlier notice is replaced, not stacked
	req.Header.Set("Authorization", authz)
	client := p.client
	if pushTestTransport != nil {
		client = &http.Client{Transport: pushTestTransport, Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
	return res.StatusCode, nil
}

// record stores the outcome for a device; gone subscriptions are removed.
func (p *pusher) record(sub pushSub, status int, err error) {
	switch {
	case err == nil && status >= 200 && status < 300:
		p.a.DB.Exec(`UPDATE push_subs SET last_ok=?, last_err='' WHERE id=?`, nowMs(), sub.ID)
	case err == nil && (status == http.StatusNotFound || status == http.StatusGone):
		// The browser dropped the subscription (notifications turned off, app removed, expired).
		p.a.DB.Exec(`DELETE FROM push_subs WHERE id=?`, sub.ID)
	default:
		msg := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			msg = err.Error()
		}
		p.a.DB.Exec(`UPDATE push_subs SET last_err=? WHERE id=?`, truncateRunes(msg, 200), sub.ID)
		p.a.Log.Warn("push not delivered", "user", sub.UserID, "device", sub.ID, "err", msg)
	}
}

func (p *pusher) subs(userID int64, onlyID int64) []pushSub {
	rows, err := p.a.DB.Query(`SELECT id, user_id, endpoint, p256dh, auth, preview FROM push_subs WHERE user_id=? AND (?=0 OR id=?)`, userID, onlyID, onlyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []pushSub
	for rows.Next() {
		var s pushSub
		if rows.Scan(&s.ID, &s.UserID, &s.Endpoint, &s.P256dh, &s.Auth, &s.Preview) == nil {
			out = append(out, s)
		}
	}
	return out
}

// newMail is called for every message that lands unread in an inbox. A burst of messages
// becomes one notification a moment later.
func (p *pusher) newMail(userID, msgID int64, from, subject string) {
	var one int
	if p.a.DB.QueryRow(`SELECT 1 FROM push_subs WHERE user_id=? LIMIT 1`, userID).Scan(&one) != nil {
		return // no devices: nothing to do (the common case)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	pend := p.pending[userID]
	if pend == nil {
		pend = &pushPending{}
		p.pending[userID] = pend
		time.AfterFunc(pushDelay, func() { p.flush(userID) })
	}
	pend.count++
	pend.msgID, pend.from, pend.subject = msgID, from, subject
}

func (p *pusher) flush(userID int64) {
	p.mu.Lock()
	pend := p.pending[userID]
	delete(p.pending, userID)
	if p.closed || pend == nil {
		p.mu.Unlock()
		return
	}
	p.wg.Add(1)
	p.mu.Unlock()
	defer p.wg.Done()

	// Already read or moved in the meantime (e.g. on the computer): no notification.
	var stillNew int
	var tid int64
	p.a.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(thread_id),0) FROM messages WHERE id=? AND user_id=? AND folder='inbox' AND seen=0`,
		pend.msgID, userID).Scan(&stillNew, &tid)
	if stillNew == 0 && pend.count == 1 {
		return
	}
	var unread int
	p.a.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder='inbox' AND seen=0`, userID).Scan(&unread)
	if unread == 0 {
		return
	}
	full := pushMessage{Type: "mail", URL: "/#/inbox", Unread: unread}
	private := full
	if pend.count == 1 {
		full.Title = truncateRunes(firstNonEmpty(strings.TrimSpace(pend.from), "New message"), 80)
		full.Body = truncateRunes(firstNonEmpty(strings.TrimSpace(pend.subject), "(no subject)"), 160)
		if tid > 0 {
			full.URL = fmt.Sprintf("/#/inbox/t/%d", tid)
		}
		private.Title, private.Body = "New message", "Open "+ProductName+" to read it."
	} else {
		full.Title = fmt.Sprintf("%d new messages", pend.count)
		full.Body = truncateRunes(firstNonEmpty(strings.TrimSpace(pend.from), "New message")+": "+firstNonEmpty(strings.TrimSpace(pend.subject), "(no subject)"), 160)
		private.Title, private.Body = full.Title, "Open "+ProductName+" to read them."
	}
	var wg sync.WaitGroup
	for _, sub := range p.subs(userID, 0) {
		msg := full
		if !sub.Preview {
			msg = private
		}
		wg.Add(1)
		p.sem <- struct{}{}
		go func(sub pushSub, msg pushMessage) {
			defer func() { <-p.sem; wg.Done() }()
			status, err := p.deliver(p.a.ctx, sub, msg)
			if p.a.ctx.Err() == nil {
				p.record(sub, status, err)
			}
		}(sub, msg)
	}
	wg.Wait()
}

// stop ends pending notifications and waits for those being sent (shutdown).
func (p *pusher) stop() {
	p.mu.Lock()
	p.closed = true
	p.pending = map[int64]*pushPending{}
	p.mu.Unlock()
	p.wg.Wait()
}

func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

// ---------------------------------------------------------------- API

type pushSubInfo struct {
	ID        int64  `json:"id"`
	Endpoint  string `json:"endpoint"`
	Label     string `json:"label"`
	Preview   bool   `json:"preview"`
	CreatedAt int64  `json:"created_at"`
	LastOK    int64  `json:"last_ok"`
	LastErr   string `json:"last_err"`
}

// GET /api/push — the public key to subscribe with, and this person's devices.
func (a *App) handlePushInfo(w http.ResponseWriter, r *http.Request, u *User) {
	key := a.push.publicKey()
	rows, err := a.DB.Query(`SELECT id, endpoint, label, preview, created_at, last_ok, last_err FROM push_subs WHERE user_id=? ORDER BY created_at DESC`, u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer rows.Close()
	devices := []pushSubInfo{}
	for rows.Next() {
		var d pushSubInfo
		if rows.Scan(&d.ID, &d.Endpoint, &d.Label, &d.Preview, &d.CreatedAt, &d.LastOK, &d.LastErr) == nil {
			devices = append(devices, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": key, "available": key != "", "devices": devices})
}

// POST /api/push — turn notifications on for this browser (or refresh its subscription).
func (a *App) handlePushSubscribe(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Preview  *bool  `json:"preview"`
		Replaces string `json:"replaces"` // old endpoint, when the browser renewed the subscription
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	sid := sessionFrom(r)
	if sid == 0 {
		httpError(w, http.StatusUnauthorized, "Sign in again")
		return
	}
	if !pushEndpointOK(in.Endpoint) {
		httpError(w, http.StatusBadRequest, "This browser's notification service isn't supported")
		return
	}
	ua, err1 := b64urlDecode(in.Keys.P256dh)
	auth, err2 := b64urlDecode(in.Keys.Auth)
	if err1 != nil || err2 != nil || len(auth) != 16 {
		httpError(w, http.StatusBadRequest, "Invalid subscription")
		return
	}
	if _, err := ecdh.P256().NewPublicKey(ua); err != nil {
		httpError(w, http.StatusBadRequest, "Invalid subscription")
		return
	}
	preview := true
	if in.Preview != nil {
		preview = *in.Preview
	} else if in.Replaces != "" {
		a.DB.QueryRow(`SELECT preview FROM push_subs WHERE endpoint=? AND user_id=?`, in.Replaces, u.ID).Scan(&preview)
	}
	label := truncateRunes(strings.ToValidUTF8(r.UserAgent(), ""), 300)
	tx, err := a.DB.Begin()
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer tx.Rollback()
	if in.Replaces != "" && in.Replaces != in.Endpoint {
		tx.Exec(`DELETE FROM push_subs WHERE endpoint=? AND user_id=?`, in.Replaces, u.ID)
	}
	// The same browser may have been used by someone else before: the newest sign-in owns it.
	var id int64
	err = tx.QueryRow(`INSERT INTO push_subs(user_id, session_id, endpoint, p256dh, auth, label, preview, created_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET user_id=excluded.user_id, session_id=excluded.session_id, p256dh=excluded.p256dh,
		auth=excluded.auth, label=excluded.label, preview=excluded.preview, last_err=''
		RETURNING id`, u.ID, sid, in.Endpoint, base64.RawURLEncoding.EncodeToString(ua), base64.RawURLEncoding.EncodeToString(auth),
		label, preview, nowMs()).Scan(&id)
	if err != nil {
		a.internalError(w, err)
		return
	}
	// Keep the newest devices only.
	tx.Exec(`DELETE FROM push_subs WHERE user_id=? AND id NOT IN (SELECT id FROM push_subs WHERE user_id=? ORDER BY created_at DESC, id DESC LIMIT ?)`,
		u.ID, u.ID, pushMaxPerUser)
	if err := tx.Commit(); err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// PUT /api/push/{id} — change whether this device shows sender and subject.
func (a *App) handlePushUpdate(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Preview bool `json:"preview"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := a.DB.Exec(`UPDATE push_subs SET preview=? WHERE id=? AND user_id=?`, in.Preview, pathInt(r, "id"), u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "Device not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /api/push/{id}
func (a *App) handlePushDelete(w http.ResponseWriter, r *http.Request, u *User) {
	a.DB.Exec(`DELETE FROM push_subs WHERE id=? AND user_id=?`, pathInt(r, "id"), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/push/{id}/test — send a test notification to one device and report the result.
func (a *App) handlePushTest(w http.ResponseWriter, r *http.Request, u *User) {
	p := a.push
	p.mu.Lock()
	if t, ok := p.tested[u.ID]; ok && time.Since(t) < 5*time.Second {
		p.mu.Unlock()
		httpError(w, http.StatusTooManyRequests, "Wait a few seconds before sending another test")
		return
	}
	p.tested[u.ID] = time.Now()
	p.mu.Unlock()
	subs := p.subs(u.ID, pathInt(r, "id"))
	if len(subs) == 0 {
		httpError(w, http.StatusNotFound, "Device not found")
		return
	}
	var unread int
	a.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder='inbox' AND seen=0`, u.ID).Scan(&unread)
	msg := pushMessage{Type: "test", Title: "Notifications are on", Body: "New mail will show up here even when " + ProductName + " is closed.", URL: "/#/settings/general", Unread: unread}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, err := p.deliver(ctx, subs[0], msg)
	p.record(subs[0], status, err)
	switch {
	case err == nil && status >= 200 && status < 300:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case err == nil && (status == http.StatusNotFound || status == http.StatusGone):
		httpError(w, http.StatusGone, "This device no longer accepts notifications. Turn them on again on that device.")
	default:
		a.Log.Warn("push test failed", "user", u.Email, "status", status, "err", err)
		httpError(w, http.StatusBadGateway, "The notification service didn't accept the message. Try again later.")
	}
}
