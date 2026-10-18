package core

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GET /api/admin/queue?view=active|retrying|failed|sent|all&q=&limit=
// Recipients in the outbound queue plus a health summary.
func (a *App) handleAdminQueue(w http.ResponseWriter, r *http.Request, u *User) {
	view := r.URL.Query().Get("view")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := queryInt(r, "limit", 200, 1, 1000)
	cond := `r.status IN ('pending','deferred')`
	switch view {
	case "retrying":
		cond = `r.status = 'deferred'`
	case "failed":
		cond = `r.status = 'failed'`
	case "sent":
		cond = `r.status = 'sent'`
	case "all":
		cond = `1`
	}
	args := []any{}
	if q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		cond += ` AND (lower(r.rcpt) LIKE ? ESCAPE '\' OR lower(q.sender) LIKE ? ESCAPE '\' OR lower(q.subject) LIKE ? ESCAPE '\')`
		args = append(args, like, like, like)
	}
	rows, err := a.DB.Query(`SELECT r.id, r.queue_id, r.rcpt, r.status, r.attempts, r.next_attempt, r.last_error, r.updated_at, q.sender, q.subject, q.created_at, q.size
		FROM queue_rcpts r JOIN queue q ON q.id = r.queue_id WHERE `+cond+` ORDER BY r.updated_at DESC, r.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		a.internalError(w, err)
		return
	}
	type item struct {
		ID          int64  `json:"id"`
		QueueID     int64  `json:"queue_id"`
		Rcpt        string `json:"rcpt"`
		Status      string `json:"status"`
		Sending     bool   `json:"sending"`
		Attempts    int    `json:"attempts"`
		NextAttempt int64  `json:"next_attempt"`
		LastError   string `json:"last_error"`
		UpdatedAt   int64  `json:"updated_at"`
		Sender      string `json:"sender"`
		Subject     string `json:"subject"`
		CreatedAt   int64  `json:"created_at"`
		Size        int64  `json:"size"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		rows.Scan(&it.ID, &it.QueueID, &it.Rcpt, &it.Status, &it.Attempts, &it.NextAttempt, &it.LastError, &it.UpdatedAt, &it.Sender, &it.Subject, &it.CreatedAt, &it.Size)
		items = append(items, it)
	}
	rows.Close()
	a.qd.mu.Lock()
	for i := range items {
		items[i].Sending = a.qd.inflight[items[i].ID]
	}
	a.qd.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "summary": a.queueSummary()})
}

func (a *App) queueSummary() map[string]any {
	var pending, deferred, oldest int64
	a.DB.QueryRow(`SELECT COALESCE(SUM(r.status='pending'),0), COALESCE(SUM(r.status='deferred'),0), COALESCE(MIN(q.created_at),0)
		FROM queue_rcpts r JOIN queue q ON q.id=r.queue_id WHERE r.status IN ('pending','deferred')`).Scan(&pending, &deferred, &oldest)
	dayAgo := time.Now().Add(-24 * time.Hour).UnixMilli()
	var sent, failed, attempts, avgMs int64
	a.DB.QueryRow(`SELECT COALESCE(SUM(result='sent'),0), COALESCE(SUM(result='failed'),0), COUNT(*), COALESCE(AVG(CASE WHEN result='sent' THEN ms END),0)
		FROM queue_attempts WHERE at > ?`, dayAgo).Scan(&sent, &failed, &attempts, &avgMs)
	// Busiest destinations among waiting mail (helps spot one provider refusing us).
	type dest struct {
		Domain  string `json:"domain"`
		Waiting int    `json:"waiting"`
	}
	dests := []dest{}
	if rows, err := a.DB.Query(`SELECT lower(domain), COUNT(*) FROM queue_rcpts WHERE status IN ('pending','deferred') GROUP BY 1 ORDER BY 2 DESC LIMIT 5`); err == nil {
		for rows.Next() {
			var d dest
			rows.Scan(&d.Domain, &d.Waiting)
			dests = append(dests, d)
		}
		rows.Close()
	}
	return map[string]any{
		"waiting": pending + deferred, "retrying": deferred, "sending": a.QueueActive(),
		"sent_24h": sent, "failed_24h": failed, "attempts_24h": attempts, "avg_send_ms": avgMs,
		"oldest_waiting": oldest, "workers": a.qd.workers, "per_domain": a.qd.perDomain,
		"relay": a.Settings.Get(SetRelayHost), "destinations": dests,
	}
}

// GET /api/admin/queue/messages/{id} — one queued message: recipients, attempt log, headers.
func (a *App) handleAdminQueueMessage(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	var m struct {
		ID        int64  `json:"id"`
		Sender    string `json:"sender"`
		Subject   string `json:"subject"`
		MessageID string `json:"message_id"`
		Size      int64  `json:"size"`
		CreatedAt int64  `json:"created_at"`
		UserEmail string `json:"user_email"`
		Headers   string `json:"headers"`
	}
	var blob string
	err := a.DB.QueryRow(`SELECT q.id, q.sender, q.subject, q.message_id, q.size, q.created_at, q.blob, COALESCE(u.email,'')
		FROM queue q LEFT JOIN users u ON u.id=q.user_id WHERE q.id=?`, id).Scan(&m.ID, &m.Sender, &m.Subject, &m.MessageID, &m.Size, &m.CreatedAt, &blob, &m.UserEmail)
	if err != nil {
		httpError(w, http.StatusNotFound, "That message is no longer in the queue")
		return
	}
	if blob != "" {
		if f, err := os.Open(filepath.Join(a.Cfg.DataDir, "queue", blob)); err == nil {
			buf := make([]byte, 16<<10)
			n, _ := f.Read(buf)
			f.Close()
			head := buf[:n]
			if i := bytes.Index(head, []byte("\r\n\r\n")); i >= 0 {
				head = head[:i]
			} else if i := bytes.Index(head, []byte("\n\n")); i >= 0 {
				head = head[:i]
			}
			m.Headers = strings.ToValidUTF8(string(head), "?")
		}
	}
	type rcpt struct {
		ID          int64  `json:"id"`
		Rcpt        string `json:"rcpt"`
		Status      string `json:"status"`
		Sending     bool   `json:"sending"`
		Attempts    int    `json:"attempts"`
		NextAttempt int64  `json:"next_attempt"`
		LastError   string `json:"last_error"`
	}
	rcpts := []rcpt{}
	rows, err := a.DB.Query(`SELECT id, rcpt, status, attempts, next_attempt, last_error FROM queue_rcpts WHERE queue_id=? ORDER BY id`, id)
	if err == nil {
		for rows.Next() {
			var x rcpt
			rows.Scan(&x.ID, &x.Rcpt, &x.Status, &x.Attempts, &x.NextAttempt, &x.LastError)
			rcpts = append(rcpts, x)
		}
		rows.Close()
	}
	a.qd.mu.Lock()
	for i := range rcpts {
		rcpts[i].Sending = a.qd.inflight[rcpts[i].ID]
	}
	a.qd.mu.Unlock()
	type attempt struct {
		At     int64  `json:"at"`
		Rcpt   string `json:"rcpt"`
		Host   string `json:"host"`
		Result string `json:"result"`
		Detail string `json:"detail"`
		Ms     int64  `json:"ms"`
	}
	attempts := []attempt{}
	rows, err = a.DB.Query(`SELECT at, rcpt, host, result, detail, ms FROM queue_attempts WHERE queue_id=? ORDER BY at DESC, id DESC LIMIT 200`, id)
	if err == nil {
		for rows.Next() {
			var x attempt
			rows.Scan(&x.At, &x.Rcpt, &x.Host, &x.Result, &x.Detail, &x.Ms)
			attempts = append(attempts, x)
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": m, "recipients": rcpts, "attempts": attempts})
}

// POST /api/admin/queue/{id}/retry — try one waiting recipient now.
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

// POST /api/admin/queue/retry-all — retry every waiting recipient now (e.g. after an outage).
func (a *App) handleAdminQueueRetryAll(w http.ResponseWriter, r *http.Request, u *User) {
	res, err := a.DB.Exec(`UPDATE queue_rcpts SET next_attempt=? WHERE status IN ('pending','deferred')`, nowMs())
	if err != nil {
		a.internalError(w, err)
		return
	}
	n, _ := res.RowsAffected()
	a.KickQueue()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": n})
}

// POST /api/admin/queue/{id}/cancel — stop delivering to one recipient (no bounce is sent).
func (a *App) handleAdminQueueCancel(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	var d dueRcpt
	if err := a.DB.QueryRow(`SELECT id, queue_id, rcpt FROM queue_rcpts WHERE id=? AND status IN ('pending','deferred')`, id).Scan(&d.id, &d.queueID, &d.rcpt); err != nil {
		httpError(w, http.StatusBadRequest, "Only waiting messages can be stopped")
		return
	}
	msg := "Stopped by " + u.Email
	a.DB.Exec(`UPDATE queue_rcpts SET status='failed', last_error=?, updated_at=? WHERE id=? AND status IN ('pending','deferred')`, msg, nowMs(), id)
	a.logAttempt(d, "", "failed", msg, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
