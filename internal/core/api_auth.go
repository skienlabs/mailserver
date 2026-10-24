package core

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	cookieName  = "sm_session"
	ttlRemember = 30 * 24 * time.Hour
	ttlShort    = 24 * time.Hour
)

func (a *App) createSession(w http.ResponseWriter, r *http.Request, userID int64, remember bool) error {
	token := RandomToken(32)
	ttl := ttlShort
	if remember {
		ttl = ttlRemember
	}
	now := time.Now()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if _, err := a.DB.Exec(`INSERT INTO sessions(token_hash, user_id, created_at, last_seen, expires_at, ttl, ip, user_agent) VALUES(?,?,?,?,?,?,?,?)`,
		sha256Hex(token), userID, now.UnixMilli(), now.UnixMilli(), now.Add(ttl).UnixMilli(), ttl.Milliseconds(), a.clientIP(r), ua); err != nil {
		return err
	}
	c := &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isSecure(r)}
	if remember {
		c.MaxAge = int(ttl.Seconds())
	}
	http.SetCookie(w, c)
	a.DB.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, now.UnixMilli(), userID)
	return nil
}

func (a *App) sessionUser(r *http.Request) (*User, int64) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" || len(c.Value) > 100 {
		return nil, 0
	}
	now := nowMs()
	var sid, lastSeen, ttl, uid int64
	err = a.DB.QueryRow(`SELECT s.id, s.last_seen, s.ttl, s.user_id FROM sessions s WHERE s.token_hash=? AND s.expires_at > ?`,
		sha256Hex(c.Value), now).Scan(&sid, &lastSeen, &ttl, &uid)
	if err != nil {
		return nil, 0
	}
	u, err := a.UserByID(uid)
	if err != nil || u.Status != "active" {
		return nil, 0
	}
	if now-lastSeen > int64(5*time.Minute/time.Millisecond) {
		a.DB.Exec(`UPDATE sessions SET last_seen=?, expires_at=?, ip=? WHERE id=?`, now, now+ttl, a.clientIP(r), sid)
	}
	return u, sid
}

// authed wraps handlers that need a signed-in user.
func (a *App) authed(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, sid := a.sessionUser(r)
		if u == nil {
			httpError(w, http.StatusUnauthorized, "Please sign in")
			return
		}
		if u.MustChangePassword {
			p := r.URL.Path
			if p != "/api/me" && p != "/api/profile/password" && p != "/api/auth/logout" && p != "/api/events" {
				httpError(w, http.StatusForbidden, "password_change_required")
				return
			}
		}
		ctx := context.WithValue(context.WithValue(r.Context(), ctxUser, u), ctxSession, sid)
		h(w, r.WithContext(ctx), u)
	}
}

func (a *App) admin(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return a.authed(func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.IsAdmin() {
			httpError(w, http.StatusForbidden, "Administrator access required")
			return
		}
		h(w, r, u)
	})
}

// ---------------------------------------------------------------- handlers

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(email, "@") {
		// Allow signing in with just the user name on the primary domain.
		if d := a.PrimaryDomain(); d != nil && email != "" {
			email += "@" + d.Name
		}
	}
	ip := a.clientIP(r)
	if !a.authBegin(ip, email) {
		httpError(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait 15 minutes and try again.")
		return
	}
	u, err := a.UserByEmail(email)
	if err != nil {
		CheckPassword(dummyHash, in.Password)
	}
	if err != nil || !CheckPassword(u.passwordHash, in.Password) {
		a.logMail("auth", email, "", "", "failed", "web sign-in failed", ip)
		httpError(w, http.StatusUnauthorized, "Wrong email or password")
		return
	}
	if u.Status != "active" {
		httpError(w, http.StatusForbidden, "This account is disabled. Contact your administrator.")
		return
	}
	a.authSucceeded(ip, email)
	if err := a.createSession(w, r, u.ID, in.Remember); err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request, u *User) {
	a.DB.Exec(`DELETE FROM sessions WHERE id=?`, sessionFrom(r))
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.isSecure(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) mePayload(u *User) map[string]any {
	var domains []map[string]any
	for _, d := range a.Domains() {
		domains = append(domains, map[string]any{"id": d.ID, "name": d.Name})
	}
	return map[string]any{
		"user":     u,
		"send_as":  a.SendAsAddresses(u),
		"counts":   a.FolderCounts(u.ID),
		"org":      map[string]any{"name": a.OrgName(), "hostname": a.Hostname(), "domains": domains},
		"max_size": a.MaxMessageBytes(),
		"product":  a.AppName(),
		"brand":    a.brandInfo(),
		"version":  Version,
	}
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, a.mePayload(u))
}

// ---------------------------------------------------------------- first-run setup

func (a *App) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"needs_setup": !a.SetupDone(), "org": a.OrgName(), "product": a.AppName(), "brand": a.brandInfo(),
		"presets": AccentPresets})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if a.SetupDone() {
		httpError(w, http.StatusConflict, "Already set up")
		return
	}
	if !a.limiter.Allow("setup:"+a.clientIP(r), 20, 15*time.Minute) {
		httpError(w, http.StatusTooManyRequests, "Too many attempts. Wait a few minutes.")
		return
	}
	var in struct {
		Token    string `json:"token"`
		OrgName  string `json:"org_name"`
		Domain   string `json:"domain"`
		Hostname string `json:"hostname"`
		Name     string `json:"name"`
		Local    string `json:"local"`
		Password string `json:"password"`
		AppName  string `json:"app_name"`
		Accent   string `json:"accent"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToUpper(strings.TrimSpace(in.Token))), []byte(strings.ToUpper(a.setupToken))) != 1 {
		httpError(w, http.StatusForbidden, "The setup code is not correct. Find it in the server logs (docker logs).")
		return
	}
	in.OrgName = strings.TrimSpace(in.OrgName)
	in.Hostname = strings.ToLower(strings.TrimSpace(in.Hostname))
	if in.OrgName == "" {
		httpError(w, http.StatusBadRequest, "Enter your organisation name")
		return
	}
	if !ValidDomainName(in.Hostname) {
		httpError(w, http.StatusBadRequest, "Enter the web address of this server, like mail.example.com")
		return
	}
	if strings.TrimSpace(in.AppName) == "" {
		in.AppName = defaultAppName(in.OrgName)
	}
	appName, err := cleanAppName(in.AppName)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	accent := DefaultAccent
	if strings.TrimSpace(in.Accent) != "" {
		var ok bool
		if accent, ok = normalizeAccent(in.Accent); !ok {
			httpError(w, http.StatusBadRequest, "Choose a main colour, like #4453f2")
			return
		}
	}
	if err := ValidatePassword(in.Password); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	d := a.Domain(in.Domain)
	if d == nil {
		d, err = a.CreateDomain(in.Domain)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	u, err := a.CreateUser(NewUser{Name: in.Name, Local: in.Local, DomainID: d.ID, Password: in.Password, Role: "admin",
		QuotaBytes: 0, Title: "Administrator"})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.Settings.Set(map[string]string{SetOrgName: in.OrgName, SetHostname: in.Hostname, SetAppName: appName, SetAccent: accent, SetSetupDone: "1"}); err != nil {
		a.internalError(w, err)
		return
	}
	a.Log.Info("organisation created", "org", in.OrgName, "app", appName, "domain", d.Name, "admin", u.Email)
	go a.RestartSMTP() // pick up the new hostname for banners and TLS
	if err := a.createSession(w, r, u.ID, true); err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- password & sessions

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !a.limiter.Allow("pwchange:"+u.Email, 10, 15*time.Minute) {
		httpError(w, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}
	if !CheckPassword(u.passwordHash, in.Current) {
		httpError(w, http.StatusBadRequest, "Your current password is not correct")
		return
	}
	if in.New == in.Current {
		httpError(w, http.StatusBadRequest, "Choose a password you haven't used here")
		return
	}
	if err := a.SetPassword(u.ID, in.New, false); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND id<>?`, u.ID, sessionFrom(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleSessions(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.DB.Query(`SELECT id, created_at, last_seen, ip, user_agent FROM sessions WHERE user_id=? AND expires_at > ? ORDER BY last_seen DESC`, u.ID, nowMs())
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer rows.Close()
	type sess struct {
		ID        int64  `json:"id"`
		Created   int64  `json:"created_at"`
		LastSeen  int64  `json:"last_seen"`
		IP        string `json:"ip"`
		UserAgent string `json:"user_agent"`
		Current   bool   `json:"current"`
	}
	out := []sess{}
	cur := sessionFrom(r)
	for rows.Next() {
		var s sess
		rows.Scan(&s.ID, &s.Created, &s.LastSeen, &s.IP, &s.UserAgent)
		s.Current = s.ID == cur
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleRevokeSession(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	a.DB.Exec(`DELETE FROM sessions WHERE id=? AND user_id=?`, id, u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleRevokeOthers(w http.ResponseWriter, r *http.Request, u *User) {
	a.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND id<>?`, u.ID, sessionFrom(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func isNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
