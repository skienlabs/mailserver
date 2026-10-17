package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func pathInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

func queryInt(r *http.Request, name string, def, min, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

func contentDisposition(kind, filename string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, url.PathEscape(filename))
}

// inlineSafe lists types we let the browser render directly.
var inlineSafe = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "application/pdf": true,
	"image/bmp": true, "image/avif": true}

// ---------------------------------------------------------------- listing & reading

func (a *App) handleThreads(w http.ResponseWriter, r *http.Request, u *User) {
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = "inbox"
	}
	limit := queryInt(r, "limit", 50, 1, 200)
	offset := queryInt(r, "offset", 0, 0, 1<<30)
	rows, total, err := a.ListThreads(u.ID, folder, r.URL.Query().Get("q"), offset, limit)
	if err != nil {
		httpError(w, http.StatusBadRequest, "Unknown folder")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total, "offset": offset, "limit": limit, "counts": a.FolderCounts(u.ID)})
}

func (a *App) handleThread(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.URL.Query().Get("context")
	list, err := a.ThreadView(u.ID, pathInt(r, "id"), ctx, r.URL.Query().Get("peek") == "")
	if err != nil {
		if isNotFound(err) {
			httpError(w, http.StatusNotFound, "This conversation no longer exists")
			return
		}
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": list, "counts": a.FolderCounts(u.ID)})
}

func (a *App) handleThreadAction(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Threads []int64 `json:"threads"`
		View    string  `json:"view"`
		Action  string  `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.ThreadAction(u.ID, in.Threads, in.View, in.Action); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "counts": a.FolderCounts(u.ID)})
}

func (a *App) handleFlag(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Flagged bool `json:"flagged"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.SetFlag(u.ID, pathInt(r, "id"), in.Flagged); err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStats powers the "Today" and "Storage" cards. since = start of the user's day (ms).
func (a *App) handleStats(w http.ResponseWriter, r *http.Request, u *User) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if since <= 0 || since > nowMs() {
		since = time.Now().Truncate(24 * time.Hour).UnixMilli()
	}
	out := map[string]any{"used": u.UsedBytes, "quota": u.QuotaBytes}
	var recv, read, sent, msgs int64
	a.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(seen),0) FROM messages WHERE user_id=? AND folder IN ('inbox','archive') AND received_at >= ?`, u.ID, since).Scan(&recv, &read)
	a.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND folder='sent' AND received_at >= ?`, u.ID, since).Scan(&sent)
	a.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=?`, u.ID).Scan(&msgs)
	out["today_received"], out["today_read"], out["today_unread"], out["today_sent"], out["messages"] = recv, read, recv-read, sent, msgs
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleFolders(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, a.FolderCounts(u.ID))
}

func (a *App) loadRaw(w http.ResponseWriter, u *User, id int64) (*msgRow, []byte, bool) {
	row, err := a.messageRow(u.ID, id)
	if err != nil {
		httpError(w, http.StatusNotFound, "Message not found")
		return nil, nil, false
	}
	raw, err := a.readBlob(row.blob)
	if err != nil {
		httpError(w, http.StatusNotFound, "Message file not found")
		return nil, nil, false
	}
	return row, raw, true
}

func (a *App) handleRaw(w http.ResponseWriter, r *http.Request, u *User) {
	row, raw, ok := a.loadRaw(w, u, pathInt(r, "id"))
	if !ok {
		return
	}
	name := strings.TrimSpace(row.subject)
	if name == "" {
		name = "message"
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name+".eml"))
	w.Write(raw)
}

func serveAttachment(w http.ResponseWriter, r *http.Request, att Attachment, data []byte, forceDownload bool) {
	ct := strings.ToLower(att.Type)
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; plugin-types application/pdf")
	h.Set("X-Content-Type-Options", "nosniff")
	if !forceDownload && inlineSafe[ct] {
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", contentDisposition("inline", att.Filename))
		if ct == "application/pdf" {
			h.Del("Content-Security-Policy") // the browser PDF viewer refuses sandboxed documents; type is fixed + nosniff
		}
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", contentDisposition("attachment", att.Filename))
	}
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (a *App) handleAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	_, raw, ok := a.loadRaw(w, u, pathInt(r, "id"))
	if !ok {
		return
	}
	idx, _ := strconv.Atoi(r.PathValue("idx"))
	att, data, found := FindAttachment(raw, idx, "")
	if !found {
		httpError(w, http.StatusNotFound, "Attachment not found")
		return
	}
	serveAttachment(w, r, att, data, r.URL.Query().Get("download") != "")
}

func (a *App) handleCID(w http.ResponseWriter, r *http.Request, u *User) {
	_, raw, ok := a.loadRaw(w, u, pathInt(r, "id"))
	if !ok {
		return
	}
	cid, _ := url.PathUnescape(r.PathValue("cid"))
	att, data, found := FindAttachment(raw, -1, cid)
	if !found || !inlineSafe[strings.ToLower(att.Type)] || att.Type == "application/pdf" {
		http.NotFound(w, r)
		return
	}
	serveAttachment(w, r, att, data, false)
}

// handleForwardFiles copies a message's attachments into uploads (for forwarding).
func (a *App) handleForwardFiles(w http.ResponseWriter, r *http.Request, u *User) {
	_, raw, ok := a.loadRaw(w, u, pathInt(r, "id"))
	if !ok {
		return
	}
	body := ParseBody(raw)
	out := []map[string]any{}
	for _, att := range body.Attachments {
		if att.Inline {
			continue
		}
		_, data, found := FindAttachment(raw, att.Index, "")
		if !found {
			continue
		}
		up, err := a.saveUpload(u.ID, att.Filename, att.Type, data)
		if err != nil {
			a.internalError(w, err)
			return
		}
		out = append(out, up)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- live events (SSE)

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request, u *User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})
	ch := a.Events.Subscribe(u.ID)
	defer a.Events.Unsubscribe(u.ID, ch)
	io.WriteString(w, "retry: 5000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.ctx.Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------- uploads

func (a *App) saveUpload(userID int64, name, ctype string, data []byte) (map[string]any, error) {
	id := RandomHex(16)
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		name = "attachment"
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	if ctype == "" {
		ctype = mime.TypeByExtension(filepath.Ext(name))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if mt, _, err := mime.ParseMediaType(ctype); err == nil {
		ctype = mt
	} else {
		ctype = "application/octet-stream"
	}
	if err := writeFileSync(filepath.Join(a.Cfg.DataDir, "uploads", id), data); err != nil {
		return nil, err
	}
	if _, err := a.DB.Exec(`INSERT INTO uploads(id, user_id, name, ctype, size, created_at) VALUES(?,?,?,?,?,?)`, id, userID, name, ctype, len(data), nowMs()); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "name": name, "type": ctype, "size": len(data)}, nil
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request, u *User) {
	max := a.MaxMessageBytes()
	r.Body = http.MaxBytesReader(w, r.Body, max+1<<20)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Attachments can be up to %d MB in total", max>>20))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Attachments can be up to %d MB in total", max>>20))
		return
	}
	up, err := a.saveUpload(u.ID, hdr.Filename, hdr.Header.Get("Content-Type"), data)
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, up)
}

func (a *App) handleGetUpload(w http.ResponseWriter, r *http.Request, u *User) {
	var name, ctype string
	id := r.PathValue("id")
	if err := a.DB.QueryRow(`SELECT name, ctype FROM uploads WHERE id=? AND user_id=?`, id, u.ID).Scan(&name, &ctype); err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, "uploads", id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	serveAttachment(w, r, Attachment{Filename: name, Type: ctype}, data, r.URL.Query().Get("download") != "")
}

func (a *App) handleDeleteUpload(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	res, _ := a.DB.Exec(`DELETE FROM uploads WHERE id=? AND user_id=?`, id, u.ID)
	if n, _ := res.RowsAffected(); n > 0 {
		os.Remove(filepath.Join(a.Cfg.DataDir, "uploads", id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- drafts

// Draft is the editor state persisted for later.
type Draft struct {
	ID          int64            `json:"id"`
	From        string           `json:"from"`
	To          []Addr           `json:"to"`
	Cc          []Addr           `json:"cc"`
	Bcc         []Addr           `json:"bcc"`
	Subject     string           `json:"subject"`
	HTML        string           `json:"html"`
	Attachments []map[string]any `json:"attachments"`
	ReplyToID   int64            `json:"reply_to_id"`
	UpdatedAt   int64            `json:"updated_at"`
}

func (a *App) handleDrafts(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.DB.Query(`SELECT id, subject, to_text, snippet, updated_at FROM drafts WHERE user_id=? ORDER BY updated_at DESC LIMIT 500`, u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID        int64  `json:"id"`
		Subject   string `json:"subject"`
		To        string `json:"to"`
		Snippet   string `json:"snippet"`
		UpdatedAt int64  `json:"updated_at"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		rows.Scan(&it.ID, &it.Subject, &it.To, &it.Snippet, &it.UpdatedAt)
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleGetDraft(w http.ResponseWriter, r *http.Request, u *User) {
	var data string
	var updated int64
	id := pathInt(r, "id")
	if err := a.DB.QueryRow(`SELECT data, updated_at FROM drafts WHERE id=? AND user_id=?`, id, u.ID).Scan(&data, &updated); err != nil {
		httpError(w, http.StatusNotFound, "Draft not found")
		return
	}
	var d Draft
	json.Unmarshal([]byte(data), &d)
	d.ID, d.UpdatedAt = id, updated
	writeJSON(w, http.StatusOK, d)
}

func (a *App) handleSaveDraft(w http.ResponseWriter, r *http.Request, u *User) {
	var d Draft
	if !decodeJSON(w, r, &d) {
		return
	}
	d.HTML = SanitizeHTML(d.HTML, 0)
	b, _ := json.Marshal(d)
	var toNames []string
	for _, t := range d.To {
		toNames = append(toNames, firstNonEmpty(t.Name, t.Email))
	}
	snippet := Snippet(HTMLToText(d.HTML), 140)
	now := nowMs()
	if d.ID > 0 {
		res, err := a.DB.Exec(`UPDATE drafts SET data=?, subject=?, to_text=?, snippet=?, updated_at=? WHERE id=? AND user_id=?`,
			string(b), d.Subject, strings.Join(toNames, ", "), snippet, now, d.ID, u.ID)
		if err != nil {
			a.internalError(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"id": d.ID})
			return
		}
	}
	res, err := a.DB.Exec(`INSERT INTO drafts(user_id, data, subject, to_text, snippet, updated_at) VALUES(?,?,?,?,?,?)`,
		u.ID, string(b), d.Subject, strings.Join(toNames, ", "), snippet, now)
	if err != nil {
		a.internalError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	a.Events.Publish(u.ID, Event{Type: "sync", Folder: "drafts"})
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *App) handleDeleteDraft(w http.ResponseWriter, r *http.Request, u *User) {
	a.deleteDraft(u.ID, pathInt(r, "id"), true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) deleteDraft(userID, id int64, withUploads bool) {
	var data string
	if err := a.DB.QueryRow(`SELECT data FROM drafts WHERE id=? AND user_id=?`, id, userID).Scan(&data); err != nil {
		return
	}
	a.DB.Exec(`DELETE FROM drafts WHERE id=? AND user_id=?`, id, userID)
	if withUploads {
		var d Draft
		json.Unmarshal([]byte(data), &d)
		for _, at := range d.Attachments {
			if uid, ok := at["id"].(string); ok {
				if res, err := a.DB.Exec(`DELETE FROM uploads WHERE id=? AND user_id=?`, uid, userID); err == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						os.Remove(filepath.Join(a.Cfg.DataDir, "uploads", filepath.Base(uid)))
					}
				}
			}
		}
	}
	a.Events.Publish(userID, Event{Type: "sync", Folder: "drafts"})
}

// ---------------------------------------------------------------- send

func normalizeAddrs(list []Addr) ([]Addr, error) {
	out := make([]Addr, 0, len(list))
	seen := map[string]bool{}
	for _, x := range list {
		e := strings.TrimSpace(x.Email)
		if e == "" {
			continue
		}
		pa, err := mail.ParseAddress(e)
		if err != nil || !strings.Contains(pa.Address, "@") {
			return nil, fmt.Errorf("%q is not a valid email address", e)
		}
		addr := strings.ToLower(pa.Address)
		_, dom, _ := SplitAddress(addr)
		if !strings.Contains(dom, ".") {
			return nil, fmt.Errorf("%q is not a valid email address", e)
		}
		if seen[addr] {
			continue
		}
		seen[addr] = true
		name := strings.TrimSpace(x.Name)
		if name == "" {
			name = pa.Name
		}
		if strings.EqualFold(name, addr) {
			name = ""
		}
		out = append(out, Addr{Name: sanitizeHeaderValue(name), Email: addr})
	}
	return out, nil
}

func (a *App) handleSend(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Draft
		DraftID   int64    `json:"draft_id"`
		UploadIDs []string `json:"upload_ids"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !a.limiter.Allow(fmt.Sprintf("send:%d", u.ID), 400, time.Hour) {
		httpError(w, http.StatusTooManyRequests, "You've reached the sending limit. Try again in a while.")
		return
	}
	from, ok := a.CanSendAs(u, firstNonEmpty(in.From, u.Email))
	if !ok {
		httpError(w, http.StatusForbidden, "You can't send from that address")
		return
	}
	to, err1 := normalizeAddrs(in.To)
	cc, err2 := normalizeAddrs(in.Cc)
	bcc, err3 := normalizeAddrs(in.Bcc)
	if err := errors.Join(err1, err2, err3); err != nil {
		httpError(w, http.StatusBadRequest, strings.Split(err.Error(), "\n")[0])
		return
	}
	total := len(to) + len(cc) + len(bcc)
	if total == 0 {
		httpError(w, http.StatusBadRequest, "Add at least one recipient")
		return
	}
	if total > 100 {
		httpError(w, http.StatusBadRequest, "You can send to at most 100 recipients at once")
		return
	}
	c := Compose{
		From:    Addr{Name: from.Name, Email: from.Email},
		To:      to,
		Cc:      cc,
		Subject: sanitizeHeaderValue(strings.TrimSpace(in.Subject)),
		HTML:    SanitizeHTML(in.HTML, 0),
	}
	// Attachments
	var used []string
	var totalSize int64
	for _, id := range in.UploadIDs {
		var name, ctype string
		if err := a.DB.QueryRow(`SELECT name, ctype FROM uploads WHERE id=? AND user_id=?`, id, u.ID).Scan(&name, &ctype); err != nil {
			httpError(w, http.StatusBadRequest, "An attachment is missing — please attach it again")
			return
		}
		data, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, "uploads", filepath.Base(id)))
		if err != nil {
			httpError(w, http.StatusBadRequest, "An attachment is missing — please attach it again")
			return
		}
		totalSize += int64(len(data))
		c.Files = append(c.Files, OutgoingPart{Filename: name, Type: ctype, Data: data})
		used = append(used, id)
	}
	if totalSize*4/3 > a.MaxMessageBytes() {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("Attachments are too large (limit %d MB)", a.MaxMessageBytes()>>20))
		return
	}
	// Threading
	if in.ReplyToID > 0 {
		if row, err := a.messageRow(u.ID, in.ReplyToID); err == nil && row.msgID != "" {
			c.InReplyTo = row.msgID
			refs := strings.Fields(row.refs)
			if len(refs) > 20 {
				refs = refs[len(refs)-20:]
			}
			c.References = append(refs, row.msgID)
			a.DB.Exec(`UPDATE messages SET answered=1 WHERE id=? AND user_id=?`, in.ReplyToID, u.ID)
		}
	}
	raw, _, err := BuildMessage(c, a.Hostname())
	if err != nil {
		a.internalError(w, err)
		return
	}
	if int64(len(raw)) > a.MaxMessageBytes() {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("Message is too large (limit %d MB)", a.MaxMessageBytes()>>20))
		return
	}
	meta := ParseMeta(raw)
	var rcpts []string
	for _, l := range [][]Addr{to, cc, bcc} {
		for _, x := range l {
			rcpts = append(rcpts, x.Email)
		}
	}
	err = a.Submit(SubmitReq{User: u, EnvFrom: from.Email, Raw: raw, Rcpts: rcpts, Meta: &meta, Bcc: bcc, SaveSent: true, Via: "web"})
	var re *RecipientError
	if errors.As(err, &re) {
		httpError(w, http.StatusBadRequest, re.Error())
		return
	}
	if err != nil {
		a.internalError(w, err)
		return
	}
	if in.DraftID > 0 {
		a.deleteDraft(u.ID, in.DraftID, false)
	}
	for _, id := range used {
		if res, err := a.DB.Exec(`DELETE FROM uploads WHERE id=? AND user_id=?`, id, u.ID); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				os.Remove(filepath.Join(a.Cfg.DataDir, "uploads", filepath.Base(id)))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- people

func (a *App) handleContacts(w http.ResponseWriter, r *http.Request, u *User) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	like := "%" + q + "%"
	type contact struct {
		Name      string `json:"name"`
		Email     string `json:"email"`
		Kind      string `json:"kind"`
		ID        int64  `json:"id,omitempty"`
		AvatarVer int64  `json:"avatar_ver,omitempty"`
	}
	out := []contact{}
	seen := map[string]bool{}
	add := func(c contact) {
		if !seen[strings.ToLower(c.Email)] && len(out) < 10 {
			seen[strings.ToLower(c.Email)] = true
			out = append(out, c)
		}
	}
	rows, err := a.DB.Query(`SELECT id, name, email, avatar_ver FROM users WHERE status='active' AND (lower(name) LIKE ? OR email LIKE ?) ORDER BY name LIMIT 10`, like, like)
	if err == nil {
		for rows.Next() {
			var c contact
			rows.Scan(&c.ID, &c.Name, &c.Email, &c.AvatarVer)
			c.Kind = "user"
			add(c)
		}
		rows.Close()
	}
	rows, err = a.DB.Query(`SELECT name, address FROM aliases WHERE lower(name) LIKE ? OR address LIKE ? ORDER BY address LIMIT 10`, like, like)
	if err == nil {
		for rows.Next() {
			var c contact
			rows.Scan(&c.Name, &c.Email)
			c.Kind = "group"
			add(c)
		}
		rows.Close()
	}
	rows, err = a.DB.Query(`SELECT name, address FROM contacts WHERE user_id=? AND (lower(name) LIKE ? OR address LIKE ?) ORDER BY uses DESC, last_used DESC LIMIT 10`, u.ID, like, like)
	if err == nil {
		for rows.Next() {
			var c contact
			rows.Scan(&c.Name, &c.Email)
			c.Kind = "recent"
			add(c)
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleDirectory(w http.ResponseWriter, r *http.Request, u *User) {
	users, err := a.ListUsers()
	if err != nil {
		a.internalError(w, err)
		return
	}
	type person struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		Title     string `json:"title"`
		Phone     string `json:"phone"`
		AvatarVer int64  `json:"avatar_ver"`
	}
	out := []person{}
	for _, x := range users {
		if x.Status == "active" {
			out = append(out, person{x.ID, x.Name, x.Email, x.Title, x.Phone, x.AvatarVer})
		}
	}
	aliases, _ := a.ListAliases()
	type group struct {
		Address     string `json:"address"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Members     int    `json:"members"`
	}
	gs := []group{}
	for _, al := range aliases {
		gs = append(gs, group{al.Address, al.Name, al.Description, len(al.Members)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out, "groups": gs})
}
