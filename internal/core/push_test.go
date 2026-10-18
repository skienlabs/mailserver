package core

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func b64u(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 Appendix A: the worked example, byte for byte.
func TestPushEncryptRFC8291Example(t *testing.T) {
	asKey, err := ecdh.P256().NewPrivateKey(b64u(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(asKey.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public %s", got)
	}
	ua := b64u(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	auth := b64u(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64u(t, "DGv6ra1nlYgDCS1FRnbzlw")
	body, err := pushEncrypt(ua, auth, []byte("When I grow up, I want to be a watermelon"), asKey, salt)
	if err != nil {
		t.Fatal(err)
	}
	// The RFC lists header and ciphertext as separate base64 strings.
	header := b64u(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	ct := b64u(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	if !bytes.Equal(body, append(header, ct...)) {
		t.Fatalf("body\n got %x\nwant %x%x", body, header, ct)
	}
	// And the receiving side (as a browser does it) gets the text back.
	uaKey, _ := ecdh.P256().NewPrivateKey(b64u(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if got := pushDecryptForTest(t, uaKey, auth, body); string(got) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("decrypt: %q", got)
	}
}

// pushDecryptForTest is the browser's side of RFC 8291.
func pushDecryptForTest(t *testing.T, uaKey *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatal("short body")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	if rs != 4096 || idlen != 65 || len(ct) > int(rs) {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := uaKey.ECDH(pub)
	info := append(append([]byte("WebPush: info\x00"), uaKey.PublicKey().Bytes()...), asPub...)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatal("missing last-record delimiter")
	}
	return plain[:len(plain)-1]
}

func TestPushEndpointAndAddressChecks(t *testing.T) {
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc:def", "https://updates.push.services.mozilla.com/wpush/v2/gAAA",
		"https://web.push.apple.com/QGx", "https://wns2-par02p.notify.windows.com/w/?token=x", "https://fcm.googleapis.com:443/x",
	} {
		if !pushEndpointOK(ok) {
			t.Errorf("refused %s", ok)
		}
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/x", "https://fcm.googleapis.com:8443/x", "https://evil.example/x", "https://fcm.googleapis.com.evil.example/x",
		"https://user:pw@fcm.googleapis.com/x", "https://notify.windows.com/x", "https://169.254.169.254/latest", "https://localhost/x",
		"https://fcm.googleapis.com/x#frag", "javascript:alert(1)", "", "https://" + strings.Repeat("a", 1100) + ".push.apple.com/",
	} {
		if pushEndpointOK(bad) {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "224.0.0.1"} {
		if pushAddrOK(netip.MustParseAddr(s)) {
			t.Errorf("address %s allowed", s)
		}
	}
	for _, s := range []string{"142.250.183.10", "2a00:1450:4001::200a", "17.188.180.1"} {
		if !pushAddrOK(netip.MustParseAddr(s)) {
			t.Errorf("address %s refused", s)
		}
	}
	// The real client refuses internal addresses even if DNS points a push host there.
	app, _, _, _ := newTestApp(t, false)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer srv.Close()
	if _, err := app.push.client.Post(srv.URL, "text/plain", nil); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("connected to a loopback address: %v", err)
	}
}

// fakePushService plays the push service (Google/Mozilla/Apple) and the browser together: it
// checks the VAPID signature and decrypts each message with the browser's private key.
type fakePushService struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	got    map[string][]pushMessage // by endpoint path
	status map[string]int
	keys   map[string][2][]byte // endpoint path -> ua private key bytes, auth
	appPub []byte
}

func newFakePushService(t *testing.T, app *App) *fakePushService {
	f := &fakePushService{t: t, got: map[string][]pushMessage{}, status: map[string]int{}, keys: map[string][2][]byte{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	target, _ := url.Parse(f.srv.URL)
	inner := f.srv.Client().Transport
	pushTestTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("X-Orig-Host", r.URL.Host)
		r.URL.Host = target.Host
		r.Host = target.Host
		return inner.RoundTrip(r)
	})
	t.Cleanup(func() { pushTestTransport = nil })
	_, f.appPub, _ = app.push.keys()
	return f
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// subscription makes a browser-side key pair and returns the JSON a browser would post.
func (f *fakePushService) subscription(path string) map[string]any {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	f.mu.Lock()
	f.keys[path] = [2][]byte{k.Bytes(), auth}
	f.mu.Unlock()
	return map[string]any{"endpoint": "https://fcm.googleapis.com" + path,
		"keys": map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(auth)}}
}

func (f *fakePushService) handle(w http.ResponseWriter, r *http.Request) {
	t := f.t
	f.mu.Lock()
	keys, known := f.keys[r.URL.Path]
	st := f.status[r.URL.Path]
	f.mu.Unlock()
	if st != 0 {
		w.WriteHeader(st)
		return
	}
	if !known || r.Method != "POST" || r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "86400" ||
		r.Header.Get("Urgency") != "high" || r.Header.Get("Topic") != "newmail" {
		t.Errorf("bad request: %s %s %v", r.Method, r.URL.Path, r.Header)
		w.WriteHeader(400)
		return
	}
	// Authorization: vapid t=<jwt>, k=<server public key>
	a := r.Header.Get("Authorization")
	var tok, k string
	for _, part := range strings.Split(strings.TrimPrefix(a, "vapid "), ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "t="); ok {
			tok = v
		} else if v, ok := strings.CutPrefix(part, "k="); ok {
			k = v
		}
	}
	if k != base64.RawURLEncoding.EncodeToString(f.appPub) {
		t.Errorf("vapid k mismatch")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Errorf("jwt %q", tok)
		w.WriteHeader(401)
		return
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	json.Unmarshal(b64u(t, parts[1]), &claims)
	if claims.Aud != "https://"+r.Header.Get("X-Orig-Host") || claims.Exp < time.Now().Unix() || claims.Exp > time.Now().Add(24*time.Hour).Unix() || !strings.HasPrefix(claims.Sub, "https://") {
		t.Errorf("claims %+v", claims)
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), f.appPub)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	sig := b64u(t, parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Errorf("VAPID signature doesn't verify")
		w.WriteHeader(403)
		return
	}
	body, _ := io.ReadAll(r.Body)
	uaKey, _ := ecdh.P256().NewPrivateKey(keys[0])
	var m pushMessage
	if err := json.Unmarshal(pushDecryptForTest(t, uaKey, keys[1], body), &m); err != nil {
		t.Errorf("payload: %v", err)
	}
	f.mu.Lock()
	f.got[r.URL.Path] = append(f.got[r.URL.Path], m)
	f.mu.Unlock()
	w.WriteHeader(201)
}

func (f *fakePushService) messages(path string) []pushMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushMessage(nil), f.got[path]...)
}

func TestPushEndToEnd(t *testing.T) {
	old := pushDelay
	pushDelay = 500 * time.Millisecond // wide enough that a slow CI machine can't split a burst
	t.Cleanup(func() { pushDelay = old })
	app, _, a, b := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	f := newFakePushService(t, app)
	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	if code := c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	var info struct {
		PublicKey string        `json:"public_key"`
		Devices   []pushSubInfo `json:"devices"`
		Available bool          `json:"available"`
	}
	c.do("GET", "/api/push", nil, &info)
	if !info.Available || len(b64u(t, info.PublicKey)) != 65 || len(info.Devices) != 0 {
		t.Fatalf("info %+v", info)
	}
	// Bad subscriptions are refused.
	for _, bad := range []map[string]any{
		{"endpoint": "https://evil.example/x", "keys": map[string]string{"p256dh": "BCVx", "auth": "BTBZMqHH6r4Tts7J_aSIgg"}},
		{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(make([]byte, 65)), "auth": "BTBZMqHH6r4Tts7J_aSIgg"}},
		{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4", "auth": "short"}},
	} {
		if code := c.do("POST", "/api/push", bad, nil); code != 400 {
			t.Errorf("bad subscription accepted (%d): %v", code, bad)
		}
	}
	var sub struct {
		ID int64 `json:"id"`
	}
	if code := c.do("POST", "/api/push", f.subscription("/phone"), &sub); code != 200 || sub.ID == 0 {
		t.Fatalf("subscribe %d", code)
	}
	priv := f.subscription("/laptop")
	priv["preview"] = false
	c.do("POST", "/api/push", priv, nil)

	// One message: sender and subject (or not, for the private device), link to the conversation.
	id := deliverNotify(t, app, a, "Ravi Menon <ravi@x.test>", "Quarterly report")
	waitFor(t, "push", func() bool { return len(f.messages("/phone")) == 1 && len(f.messages("/laptop")) == 1 })
	m := f.messages("/phone")[0]
	if m.Type != "mail" || m.Title != "Ravi Menon" || m.Body != "Quarterly report" || m.Unread != 1 || m.URL != "/#/inbox/t/"+itoa(threadOf(t, app, id)) {
		t.Fatalf("phone got %+v", m)
	}
	if p := f.messages("/laptop")[0]; strings.Contains(p.Title+p.Body, "Ravi") || strings.Contains(p.Title+p.Body, "Quarterly") || p.Title != "New message" {
		t.Fatalf("private device got %+v", p)
	}
	// A burst becomes one notification.
	for i := 0; i < 3; i++ {
		deliverNotify(t, app, a, "Anil <anil@x.test>", "Burst "+itoa(int64(i)))
	}
	waitFor(t, "burst", func() bool { return len(f.messages("/phone")) == 2 })
	time.Sleep(700 * time.Millisecond) // a wrongly split burst would show up as a third message
	if ms := f.messages("/phone"); len(ms) != 2 || ms[1].Title != "3 new messages" || !strings.Contains(ms[1].Body, "Burst 2") || ms[1].Unread != 4 {
		t.Fatalf("burst: %+v", ms)
	}
	// Read on the computer before the notification goes out: nothing is sent.
	id = deliverNotify(t, app, a, "Late <late@x.test>", "Already read")
	app.ThreadAction(a.ID, []int64{threadOf(t, app, id)}, "inbox", "read")
	time.Sleep(900 * time.Millisecond)
	if n := len(f.messages("/phone")); n != 2 {
		t.Fatalf("pushed a message that was already read (%d)", n)
	}
	// Spam, quiet deliveries and other folders never notify.
	app.Deliver(a.ID, rawMsg("s@spam.test", a.Email, "Spammy"), DeliverOpts{Folder: "spam", Notify: false})
	app.Deliver(a.ID, rawMsg("i@imp.test", a.Email, "Imported"), DeliverOpts{Folder: "inbox", Quiet: true, Notify: true})
	time.Sleep(900 * time.Millisecond)
	if n := len(f.messages("/phone")); n != 2 {
		t.Fatalf("pushed spam/import (%d)", n)
	}
	// Another person's mail never reaches these devices.
	deliverNotify(t, app, b, "x <x@x.test>", "For Sajin")
	time.Sleep(900 * time.Millisecond)
	if n := len(f.messages("/phone")); n != 2 {
		t.Fatal("another user's mail was pushed")
	}

	// Test button, preview toggle, and someone else can't touch these devices.
	if code := c.do("POST", "/api/push/"+itoa(sub.ID)+"/test", nil, nil); code != 200 {
		t.Fatalf("test push %d", code)
	}
	if ms := f.messages("/phone"); ms[len(ms)-1].Type != "test" {
		t.Fatal("test message not delivered")
	}
	if code := c.do("POST", "/api/push/"+itoa(sub.ID)+"/test", nil, nil); code != 429 {
		t.Fatalf("test push not rate limited: %d", code)
	}
	cb := newClient(t, srv.URL)
	cb.do("POST", "/api/auth/login", map[string]string{"email": b.Email, "password": "password-sajin"}, nil)
	if code := cb.do("PUT", "/api/push/"+itoa(sub.ID), map[string]bool{"preview": false}, nil); code != 404 {
		t.Fatalf("other user changed a device: %d", code)
	}
	cb.do("DELETE", "/api/push/"+itoa(sub.ID), nil, nil)
	if code := cb.do("POST", "/api/push/"+itoa(sub.ID)+"/test", nil, nil); code != 404 {
		t.Fatalf("other user tested a device: %d", code)
	}
	c.do("GET", "/api/push", nil, &info)
	if len(info.Devices) != 2 {
		t.Fatalf("devices after other user's attempts: %d", len(info.Devices))
	}

	// The push service says the subscription is gone: it's removed.
	f.mu.Lock()
	f.status["/laptop"] = 410
	f.mu.Unlock()
	deliverNotify(t, app, a, "Gone <g@x.test>", "To a removed device")
	waitFor(t, "cleanup", func() bool {
		var n int
		app.DB.QueryRow(`SELECT COUNT(*) FROM push_subs WHERE endpoint LIKE '%/laptop'`).Scan(&n)
		return n == 0
	})
	// A failing service is remembered, not fatal.
	f.mu.Lock()
	f.status["/phone"] = 500
	f.mu.Unlock()
	deliverNotify(t, app, a, "Err <e@x.test>", "Service down")
	waitFor(t, "error recorded", func() bool {
		var e string
		app.DB.QueryRow(`SELECT last_err FROM push_subs WHERE id=?`, sub.ID).Scan(&e)
		return e == "HTTP 500"
	})

	// The same browser signed in as someone else now belongs to them.
	cb.do("POST", "/api/push", map[string]any{"endpoint": "https://fcm.googleapis.com/phone", "keys": f.subscription("/phone")["keys"]}, nil)
	var owner int64
	app.DB.QueryRow(`SELECT user_id FROM push_subs WHERE endpoint='https://fcm.googleapis.com/phone'`).Scan(&owner)
	if owner != b.ID {
		t.Fatalf("browser owner %d", owner)
	}
	// Signing out ends notifications for that browser.
	cb.do("POST", "/api/auth/logout", nil, nil)
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM push_subs WHERE user_id=?`, b.ID).Scan(&n)
	if n != 0 {
		t.Fatal("subscription survived sign-out")
	}
}

// Changing the password (or an administrator disabling the account) stops notifications on
// every other device, like it signs them out.
func TestPushEndsWithSessions(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	app.Settings.Set(map[string]string{SetSetupDone: "1"})
	f := newFakePushService(t, app)
	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	phone, laptop := newClient(t, srv.URL), newClient(t, srv.URL)
	for _, c := range []*client{phone, laptop} {
		c.do("POST", "/api/auth/login", map[string]string{"email": a.Email, "password": "password-asi"}, nil)
	}
	phone.do("POST", "/api/push", f.subscription("/p"), nil)
	laptop.do("POST", "/api/push", f.subscription("/l"), nil)
	if code := laptop.do("POST", "/api/profile/password", map[string]string{"current": "password-asi", "new": "Another-Pass-77"}, nil); code != 200 {
		t.Fatalf("password change %d", code)
	}
	var eps []string
	rows, _ := app.DB.Query(`SELECT endpoint FROM push_subs`)
	for rows.Next() {
		var e string
		rows.Scan(&e)
		eps = append(eps, e)
	}
	rows.Close()
	if len(eps) != 1 || !strings.HasSuffix(eps[0], "/l") {
		t.Fatalf("after password change: %v", eps)
	}
	// Device limit: the oldest go.
	old := pushMaxPerUser
	pushMaxPerUser = 3
	t.Cleanup(func() { pushMaxPerUser = old })
	for i := 0; i < 5; i++ {
		laptop.do("POST", "/api/push", f.subscription("/d"+itoa(int64(i))), nil)
		time.Sleep(2 * time.Millisecond)
	}
	var n int
	app.DB.QueryRow(`SELECT COUNT(*) FROM push_subs WHERE user_id=?`, a.ID).Scan(&n)
	if n != 3 {
		t.Fatalf("device limit: %d", n)
	}
	// Key survives a restart (same public key), so devices keep working.
	k1 := app.push.publicKey()
	app.push = newPusher(app)
	if app.push.publicKey() != k1 {
		t.Fatal("VAPID key changed across restart")
	}
}

func deliverNotify(t *testing.T, app *App, u *User, from, subject string) int64 {
	t.Helper()
	id, err := app.Deliver(u.ID, rawMsg(from, u.Email, subject), DeliverOpts{Folder: "inbox", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func rawMsg(from, to, subject string) []byte {
	return []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\nMessage-ID: <" + RandomHex(8) + "@x.test>\r\nDate: Mon, 02 Jun 2025 10:00:00 +0000\r\n\r\nbody\r\n")
}

func itoa(n int64) string { return big.NewInt(n).String() }
