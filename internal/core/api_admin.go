package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var startedAt = time.Now()

func (a *App) handleAdminOverview(w http.ResponseWriter, r *http.Request, u *User) {
	var users, active, admins int
	var storage, messages int64
	a.DB.QueryRow(`SELECT COUNT(*), SUM(status='active'), SUM(role='admin') FROM users`).Scan(&users, &active, &admins)
	a.DB.QueryRow(`SELECT COALESCE(SUM(used_bytes),0) FROM users`).Scan(&storage)
	a.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages)
	var pending, deferred, failed int
	a.DB.QueryRow(`SELECT COALESCE(SUM(status='pending'),0), COALESCE(SUM(status='deferred'),0), COALESCE(SUM(status='failed'),0) FROM queue_rcpts`).Scan(&pending, &deferred, &failed)
	dayAgo := time.Now().Add(-24 * time.Hour).UnixMilli()
	stats := map[string]int{}
	rows, err := a.DB.Query(`SELECT direction || ':' || status, COUNT(*) FROM mail_log WHERE ts > ? GROUP BY 1`, dayAgo)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			rows.Scan(&k, &n)
			stats[k] = n
		}
		rows.Close()
	}
	// 14-day traffic chart
	type day struct {
		Day string `json:"day"`
		In  int    `json:"in"`
		Out int    `json:"out"`
	}
	var days []day
	start := time.Now().AddDate(0, 0, -13)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
	for i := 0; i < 14; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		days = append(days, day{Day: d})
	}
	// Count per day in SQL (one indexed range per local day) instead of streaming every row to Go.
	var vals []string
	var dargs []any
	for i := range days {
		lo := start.AddDate(0, 0, i)
		vals = append(vals, "(?,?,?)")
		dargs = append(dargs, i, lo.UnixMilli(), lo.AddDate(0, 0, 1).UnixMilli())
	}
	rows, err = a.DB.Query(`WITH d(i, lo, hi) AS (VALUES `+strings.Join(vals, ",")+`)
		SELECT d.i, (SELECT COUNT(*) FROM mail_log WHERE ts >= d.lo AND ts < d.hi AND direction = 'out' AND status IN ('delivered','sent','spam')),
		            (SELECT COUNT(*) FROM mail_log WHERE ts >= d.lo AND ts < d.hi AND direction <> 'out' AND status IN ('delivered','sent','spam'))
		FROM d`, dargs...)
	if err == nil {
		for rows.Next() {
			var i, out, in int
			rows.Scan(&i, &out, &in)
			days[i].Out, days[i].In = out, in
		}
		rows.Close()
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	writeJSON(w, http.StatusOK, map[string]any{
		"users": users, "active_users": active, "admins": admins, "storage": storage, "messages": messages,
		"queue":   map[string]int{"pending": pending, "deferred": deferred, "failed": failed},
		"last24h": stats, "traffic": days, "tls_mode": a.TLS.Mode(), "hostname": a.Hostname(),
		"uptime": int64(time.Since(startedAt).Seconds()), "memory": mem.Sys, "goroutines": runtime.NumGoroutine(),
		"relay": a.Settings.Get(SetRelayHost) != "",
	})
}

// ---------------------------------------------------------------- users

func (a *App) handleAdminUsers(w http.ResponseWriter, r *http.Request, u *User) {
	users, err := a.ListUsers()
	if err != nil {
		a.internalError(w, err)
		return
	}
	if users == nil {
		users = []*User{}
	}
	writeJSON(w, http.StatusOK, users)
}

func (a *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name     string `json:"name"`
		Local    string `json:"local"`
		DomainID int64  `json:"domain_id"`
		Password string `json:"password"`
		Role     string `json:"role"`
		QuotaMB  int64  `json:"quota_mb"`
		Title    string `json:"title"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	generated := false
	if strings.TrimSpace(in.Password) == "" {
		in.Password = GeneratePassword()
		generated = true
	}
	nu, err := a.CreateUser(NewUser{Name: in.Name, Local: in.Local, DomainID: in.DomainID, Password: in.Password, Role: in.Role,
		QuotaBytes: quotaBytes(in.QuotaMB), Title: in.Title, MustChange: true})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{"user": nu}
	if generated {
		resp["password"] = in.Password
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	target, err := a.UserByID(id)
	if err != nil {
		httpError(w, http.StatusNotFound, "User not found")
		return
	}
	var in struct {
		Name    string `json:"name"`
		Role    string `json:"role"`
		Status  string `json:"status"`
		QuotaMB int64  `json:"quota_mb"`
		Title   string `json:"title"`
		Phone   string `json:"phone"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name = sanitizeHeaderValue(strings.TrimSpace(in.Name))
	if in.Name == "" {
		httpError(w, http.StatusBadRequest, "Enter a name")
		return
	}
	if in.Role != "admin" {
		in.Role = "user"
	}
	if in.Status != "disabled" {
		in.Status = "active"
	}
	if target.ID == u.ID && (in.Role != "admin" || in.Status != "active") {
		httpError(w, http.StatusBadRequest, "You can't remove your own admin access or disable yourself")
		return
	}
	if target.IsAdmin() && target.Status == "active" && (in.Role != "admin" || in.Status != "active") && a.adminCount() <= 1 {
		httpError(w, http.StatusBadRequest, "There must be at least one active administrator")
		return
	}
	in.QuotaMB = clampQuotaMB(in.QuotaMB)
	if _, err := a.DB.Exec(`UPDATE users SET name=?, role=?, status=?, quota_bytes=?, title=?, phone=? WHERE id=?`,
		in.Name, in.Role, in.Status, quotaBytes(in.QuotaMB), strings.TrimSpace(in.Title), strings.TrimSpace(in.Phone), id); err != nil {
		a.internalError(w, err)
		return
	}
	if in.Status == "disabled" {
		a.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
		a.Events.Publish(id, Event{Type: "signout"})
	}
	nu, _ := a.UserByID(id)
	writeJSON(w, http.StatusOK, nu)
}

func (a *App) handleAdminResetPassword(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	if _, err := a.UserByID(id); err != nil {
		httpError(w, http.StatusNotFound, "User not found")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	generated := false
	if strings.TrimSpace(in.Password) == "" {
		in.Password = GeneratePassword()
		generated = true
	}
	if err := a.SetPassword(id, in.Password, id != u.ID); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if id != u.ID {
		a.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
		a.Events.Publish(id, Event{Type: "signout"})
	}
	resp := map[string]any{"ok": true}
	if generated {
		resp["password"] = in.Password
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	if id == u.ID {
		httpError(w, http.StatusBadRequest, "You can't delete your own account")
		return
	}
	if err := a.DeleteUser(id); err != nil {
		if isNotFound(err) {
			httpError(w, http.StatusNotFound, "User not found")
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAdminUserAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	a.saveAvatar(w, r, pathInt(r, "id"))
}

// ---------------------------------------------------------------- groups

func (a *App) handleAdminAliases(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.ListAliases()
	if err != nil {
		a.internalError(w, err)
		return
	}
	if list == nil {
		list = []*Alias{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleAdminSaveAlias(w http.ResponseWriter, r *http.Request, u *User) {
	var in AliasInput
	if !decodeJSON(w, r, &in) {
		return
	}
	id, err := a.SaveAlias(pathInt(r, "id"), in)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *App) handleAdminDeleteAlias(w http.ResponseWriter, r *http.Request, u *User) {
	if err := a.DeleteAlias(pathInt(r, "id")); err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- domains & DNS

// DNSRecord is a record the administrator must publish.
type DNSRecord struct {
	Kind     string `json:"kind"` // MX | TXT | A | PTR
	Purpose  string `json:"purpose"`
	Host     string `json:"host"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Status   string `json:"status"` // ok | warn | missing | unknown
	Found    string `json:"found"`
	Help     string `json:"help"`
}

func (a *App) dnsRecords(d *Domain, check bool) []DNSRecord {
	host := a.Hostname()
	dkimVal, _ := DKIMRecord(d.keyPEM)
	recs := []DNSRecord{
		{Kind: "A", Purpose: "Server address", Host: host, Value: "Your server's public IP address", Status: "unknown",
			Help: "Points the mail hostname at this server. Usually already present if the web address works."},
		{Kind: "MX", Purpose: "Receive mail", Host: d.Name, Value: host, Priority: 10, Status: "unknown",
			Help: "Tells the world to deliver mail for " + d.Name + " to this server."},
		{Kind: "TXT", Purpose: "SPF", Host: d.Name, Value: "v=spf1 mx a:" + host + " -all", Status: "unknown",
			Help: "Lists which servers may send mail for " + d.Name + ". If you send through a relay, add its include: as well."},
		{Kind: "TXT", Purpose: "DKIM", Host: d.Selector + "._domainkey." + d.Name, Value: dkimVal, Status: "unknown",
			Help: "Public key used to verify this server's signatures. Paste the value exactly; long values are split automatically by most DNS providers."},
		{Kind: "TXT", Purpose: "DMARC", Host: "_dmarc." + d.Name, Value: "v=DMARC1; p=quarantine; adkim=r; aspf=r", Status: "unknown",
			Help: "Tells receivers what to do with mail that fails SPF and DKIM. Start with p=quarantine; move to p=reject once everything works."},
		{Kind: "PTR", Purpose: "Reverse DNS", Host: "Your server's IP address", Value: host, Status: "unknown",
			Help: "Set at your hosting provider (not your DNS provider). Gmail and Outlook reject mail from IPs without matching reverse DNS."},
	}
	if !check {
		return recs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	res := net.DefaultResolver
	txt := func(name string) []string {
		v, _ := res.LookupTXT(ctx, name)
		return v
	}
	// A + PTR
	ips, err := res.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		recs[0].Status, recs[0].Found = "missing", "no address found"
		recs[5].Status = "unknown"
	} else {
		recs[0].Status, recs[0].Found = "ok", strings.Join(ips, ", ")
		recs[5].Host = ips[0]
		var ptrs []string
		ok := false
		for _, ip := range ips {
			names, _ := res.LookupAddr(ctx, ip)
			for _, n := range names {
				n = strings.TrimSuffix(strings.ToLower(n), ".")
				ptrs = append(ptrs, n)
				if n == host {
					ok = true
				}
			}
		}
		switch {
		case ok:
			recs[5].Status = "ok"
		case len(ptrs) > 0:
			recs[5].Status = "warn"
		default:
			recs[5].Status = "missing"
		}
		recs[5].Found = strings.Join(ptrs, ", ")
	}
	// MX
	mxs, _ := res.LookupMX(ctx, d.Name)
	var found []string
	recs[1].Status = "missing"
	for _, mx := range mxs {
		h := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		found = append(found, fmt.Sprintf("%d %s", mx.Pref, h))
		if h == host {
			recs[1].Status = "ok"
		}
	}
	if recs[1].Status != "ok" && len(found) > 0 {
		recs[1].Status = "warn"
	}
	recs[1].Found = strings.Join(found, ", ")
	// SPF
	recs[2].Status = "missing"
	for _, t := range txt(d.Name) {
		if strings.HasPrefix(strings.ToLower(t), "v=spf1") {
			recs[2].Found = t
			lt := strings.ToLower(t)
			if strings.Contains(lt, " mx") || strings.Contains(lt, "a:"+host) || containsAny(lt, ips) {
				recs[2].Status = "ok"
			} else {
				recs[2].Status = "warn"
			}
		}
	}
	// DKIM
	recs[3].Status = "missing"
	if v := strings.Join(txt(recs[3].Host), ""); v != "" {
		recs[3].Found = v
		if dkimP(v) != "" && dkimP(v) == dkimP(dkimVal) {
			recs[3].Status = "ok"
		} else {
			recs[3].Status = "warn"
		}
	}
	// DMARC
	recs[4].Status = "missing"
	for _, t := range txt(recs[4].Host) {
		if strings.HasPrefix(strings.ToUpper(t), "V=DMARC1") {
			recs[4].Status, recs[4].Found = "ok", t
		}
	}
	return recs
}

func containsAny(s string, ips []string) bool {
	for _, ip := range ips {
		if strings.Contains(s, ip) {
			return true
		}
	}
	return false
}

func dkimP(v string) string {
	for _, part := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == "p" {
			return strings.Join(strings.Fields(val), "")
		}
	}
	return ""
}

func (a *App) handleAdminDomains(w http.ResponseWriter, r *http.Request, u *User) {
	type dom struct {
		*Domain
		Users   int         `json:"users"`
		Records []DNSRecord `json:"records"`
	}
	out := []dom{}
	for _, d := range a.Domains() {
		var n int
		a.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE domain_id=?`, d.ID).Scan(&n)
		out = append(out, dom{Domain: d, Users: n, Records: a.dnsRecords(d, false)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleAdminCheckDomain(w http.ResponseWriter, r *http.Request, u *User) {
	d := a.DomainByID(pathInt(r, "id"))
	if d == nil {
		httpError(w, http.StatusNotFound, "Domain not found")
		return
	}
	writeJSON(w, http.StatusOK, a.dnsRecords(d, true))
}

func (a *App) handleAdminCreateDomain(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := a.CreateDomain(in.Name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *App) handleAdminDeleteDomain(w http.ResponseWriter, r *http.Request, u *User) {
	if err := a.DeleteDomain(pathInt(r, "id")); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- queue & log

func (a *App) handleAdminQueue(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.DB.Query(`SELECT r.id, r.rcpt, r.status, r.attempts, r.next_attempt, r.last_error, r.updated_at, q.sender, q.subject, q.created_at, q.size
		FROM queue_rcpts r JOIN queue q ON q.id = r.queue_id ORDER BY (r.status IN ('pending','deferred')) DESC, q.created_at DESC LIMIT 300`)
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID          int64  `json:"id"`
		Rcpt        string `json:"rcpt"`
		Status      string `json:"status"`
		Attempts    int    `json:"attempts"`
		NextAttempt int64  `json:"next_attempt"`
		LastError   string `json:"last_error"`
		UpdatedAt   int64  `json:"updated_at"`
		Sender      string `json:"sender"`
		Subject     string `json:"subject"`
		CreatedAt   int64  `json:"created_at"`
		Size        int64  `json:"size"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		rows.Scan(&it.ID, &it.Rcpt, &it.Status, &it.Attempts, &it.NextAttempt, &it.LastError, &it.UpdatedAt, &it.Sender, &it.Subject, &it.CreatedAt, &it.Size)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleAdminQueueRetry(w http.ResponseWriter, r *http.Request, u *User) {
	res, err := a.DB.Exec(`UPDATE queue_rcpts SET next_attempt=?, status='pending' WHERE id=? AND status IN ('pending','deferred')`, nowMs(), pathInt(r, "id"))
	if err != nil {
		a.internalError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusBadRequest, "Only waiting messages can be retried")
		return
	}
	a.KickQueue()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAdminQueueCancel(w http.ResponseWriter, r *http.Request, u *User) {
	a.DB.Exec(`UPDATE queue_rcpts SET status='failed', last_error='Cancelled by administrator', updated_at=? WHERE id=? AND status IN ('pending','deferred')`, nowMs(), pathInt(r, "id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAdminLog(w http.ResponseWriter, r *http.Request, u *User) {
	q := "%" + strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))) + "%"
	offset := queryInt(r, "offset", 0, 0, 1<<30)
	rows, err := a.DB.Query(`SELECT id, ts, direction, sender, rcpt, subject, status, detail, remote FROM mail_log
		WHERE lower(sender) LIKE ? OR lower(rcpt) LIKE ? OR lower(subject) LIKE ? OR lower(status) LIKE ? ORDER BY ts DESC LIMIT 100 OFFSET ?`, q, q, q, q, offset)
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID        int64  `json:"id"`
		TS        int64  `json:"ts"`
		Direction string `json:"direction"`
		Sender    string `json:"sender"`
		Rcpt      string `json:"rcpt"`
		Subject   string `json:"subject"`
		Status    string `json:"status"`
		Detail    string `json:"detail"`
		Remote    string `json:"remote"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		rows.Scan(&it.ID, &it.TS, &it.Direction, &it.Sender, &it.Rcpt, &it.Subject, &it.Status, &it.Detail, &it.Remote)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- settings

var editableSettings = []string{SetOrgName, SetHostname, SetRelayHost, SetRelayPort, SetRelayUser, SetRelayTLS,
	SetMaxMessageMB, SetSpamThreshold, SetDNSBL, SetDefaultQuotaMB, SetTrashDays}

func (a *App) handleAdminGetSettings(w http.ResponseWriter, r *http.Request, u *User) {
	out := map[string]any{}
	for _, k := range editableSettings {
		out[k] = a.Settings.Get(k)
	}
	out["relay_pass_set"] = a.Settings.Get(SetRelayPass) != ""
	out["hostname_locked"] = a.Cfg.Hostname != ""
	out["tls_mode"] = a.TLS.Mode()
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleAdminSetSettings(w http.ResponseWriter, r *http.Request, u *User) {
	var in map[string]any
	if !decodeJSON(w, r, &in) {
		return
	}
	vals := map[string]string{}
	str := func(k string) (string, bool) {
		v, ok := in[k]
		if !ok || v == nil {
			return "", false
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t), true
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(t), true
		}
		return "", false
	}
	for _, k := range editableSettings {
		if v, ok := str(k); ok {
			vals[k] = v
		}
	}
	if v, ok := str("relay_pass"); ok && v != "" {
		vals[SetRelayPass] = v
	}
	if b, ok := in["relay_pass_clear"].(bool); ok && b {
		vals[SetRelayPass] = ""
	}
	if v, ok := vals[SetOrgName]; ok && v == "" {
		httpError(w, http.StatusBadRequest, "Organisation name can't be empty")
		return
	}
	if v, ok := vals[SetHostname]; ok {
		v = strings.ToLower(v)
		vals[SetHostname] = v
		if !ValidDomainName(v) {
			httpError(w, http.StatusBadRequest, "Enter a valid hostname, like mail.example.com")
			return
		}
	}
	for _, k := range []string{SetMaxMessageMB, SetDefaultQuotaMB, SetTrashDays, SetRelayPort} {
		if v, ok := vals[k]; ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (k == SetMaxMessageMB && (n < 1 || n > 100)) || (k == SetRelayPort && (n < 1 || n > 65535)) ||
				(k == SetDefaultQuotaMB && n > maxQuotaMB) || (k == SetTrashDays && n > 3650) {
				httpError(w, http.StatusBadRequest, "Check the number for "+strings.ReplaceAll(k, "_", " "))
				return
			}
		}
	}
	if v, ok := vals[SetSpamThreshold]; ok {
		// NaN passes neither "< 1" nor "> 50" and would silently disable score-based filing.
		if f, err := strconv.ParseFloat(v, 64); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 1 || f > 50 {
			httpError(w, http.StatusBadRequest, "Spam threshold must be between 1 and 50")
			return
		}
	}
	if v, ok := vals[SetRelayHost]; ok && v != "" {
		v = strings.TrimSuffix(strings.ToLower(v), ".")
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			v = v[1 : len(v)-1] // [2001:db8::1]
		}
		vals[SetRelayHost] = v
		if ip := net.ParseIP(v); ip == nil && !validRelayHost(v) {
			httpError(w, http.StatusBadRequest, "Enter the relay's host name only, like smtp.example.com (put the port in the Port field)")
			return
		}
	}
	if v, ok := vals[SetRelayTLS]; ok && v != "tls" && v != "starttls" {
		vals[SetRelayTLS] = "starttls"
	}
	restart := vals[SetHostname] != "" && vals[SetHostname] != a.Settings.Get(SetHostname) ||
		vals[SetMaxMessageMB] != "" && vals[SetMaxMessageMB] != a.Settings.Get(SetMaxMessageMB)
	if err := a.Settings.Set(vals); err != nil {
		a.internalError(w, err)
		return
	}
	if restart {
		go a.RestartSMTP()
	}
	a.handleAdminGetSettings(w, r, u)
}

// handleAdminTestPort25 checks whether this server can reach other mail servers.
func (a *App) handleAdminTestPort25(w http.ResponseWriter, r *http.Request, u *User) {
	target := "gmail-smtp-in.l.google.com:25"
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, 8*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "target": target,
			"message": "Can't connect to other mail servers on port 25. Many cloud providers (Google Cloud, AWS, Azure…) block it — ask your provider to open it, or configure an SMTP relay in Settings.",
			"error":   err.Error()})
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, net.ErrClosed) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "target": target, "message": "Connected, but the remote server did not greet us.", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": strings.HasPrefix(line, "220"), "target": target, "banner": strings.TrimSpace(line),
		"ms": time.Since(start).Milliseconds(), "message": "Outbound port 25 is open — this server can deliver mail directly."})
}

// maxQuotaMB caps mailbox sizes (16 TiB) so MB→bytes can't overflow into "unlimited".
const maxQuotaMB = 1 << 24

func clampQuotaMB(mb int64) int64 {
	return min(max(mb, 0), maxQuotaMB)
}

func quotaBytes(mb int64) int64 { return clampQuotaMB(mb) << 20 }

var reHostLabel = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// validRelayHost accepts any plain host name — including single-label Docker service names
// like "postfix" — but not a "host:port" or anything with spaces.
func validRelayHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !reHostLabel.MatchString(l) {
			return false
		}
	}
	return true
}
