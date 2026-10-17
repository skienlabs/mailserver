package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Folders that physically hold messages.
var realFolders = map[string]bool{"inbox": true, "sent": true, "archive": true, "spam": true, "trash": true}

// ErrQuota is returned when a mailbox is full.
var ErrQuota = errors.New("mailbox full")

// DeliverOpts controls how a message is stored in a mailbox.
type DeliverOpts struct {
	Folder       string
	Seen         bool
	SpamScore    float64
	AuthSummary  string
	Bcc          []Addr
	Meta         *Meta
	Notify       bool
	EnforceQuota bool
}

func (a *App) blobPath(rel string) string { return filepath.Join(a.Cfg.DataDir, "mail", rel) }

func (a *App) readBlob(rel string) ([]byte, error) { return os.ReadFile(a.blobPath(rel)) }

func writeFileSync(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func addrsJSON(list []Addr) string {
	if list == nil {
		list = []Addr{}
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func addrsText(m *Meta, bcc []Addr) string {
	var sb strings.Builder
	add := func(x Addr) { sb.WriteString(x.Name); sb.WriteByte(' '); sb.WriteString(x.Email); sb.WriteByte(' ') }
	add(m.From)
	for _, x := range m.To {
		add(x)
	}
	for _, x := range m.Cc {
		add(x)
	}
	for _, x := range bcc {
		add(x)
	}
	return sb.String()
}

// Deliver stores raw in a user's mailbox and returns the new message ID.
func (a *App) Deliver(userID int64, raw []byte, o DeliverOpts) (int64, error) {
	if o.Folder == "" {
		o.Folder = "inbox"
	}
	m := o.Meta
	if m == nil {
		mm := ParseMeta(raw)
		m = &mm
	}
	size := int64(len(raw))
	if o.EnforceQuota {
		var quota, used int64
		if err := a.DB.QueryRow(`SELECT quota_bytes, used_bytes FROM users WHERE id=?`, userID).Scan(&quota, &used); err != nil {
			return 0, err
		}
		if quota > 0 && used+size > quota {
			return 0, ErrQuota
		}
	}
	if dup := a.duplicateOf(userID, m, o.Folder); dup > 0 {
		return dup, nil // already delivered (e.g. a sender retry): store nothing
	}
	rel := filepath.Join(fmt.Sprint(userID), time.Now().Format("200601"), RandomHex(12)+".eml")
	if err := writeFileSync(a.blobPath(rel), raw); err != nil {
		return 0, err
	}
	id, err := a.insertMessage(userID, rel, size, m, o)
	if errors.Is(err, errDuplicate) {
		os.Remove(a.blobPath(rel)) // lost a race with an identical delivery
		return id, nil
	}
	if err != nil {
		os.Remove(a.blobPath(rel))
		return 0, err
	}
	if o.Notify {
		a.Events.Publish(userID, Event{Type: "mail", Folder: o.Folder, From: firstNonEmpty(m.From.Name, m.From.Email), Subject: m.Subject})
	} else {
		a.Events.Publish(userID, Event{Type: "sync", Folder: o.Folder})
	}
	return id, nil
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

var errDuplicate = errors.New("duplicate message")

// mailboxFull reports whether a message of size bytes (0 = unknown) can't fit in the user's quota.
func (a *App) mailboxFull(userID, size int64) bool {
	var quota, used int64
	if err := a.DB.QueryRow(`SELECT quota_bytes, used_bytes FROM users WHERE id=?`, userID).Scan(&quota, &used); err != nil {
		return false
	}
	return quota > 0 && (used >= quota || used+size > quota)
}

// duplicateOf returns the ID of an identical message (same Message-ID, same folder) already stored.
func (a *App) duplicateOf(userID int64, m *Meta, folder string) int64 {
	if m.MessageID == "" || folder == "sent" {
		return 0
	}
	var dup int64
	a.DB.QueryRow(`SELECT id FROM messages WHERE user_id=? AND message_id=? AND folder=? LIMIT 1`, userID, m.MessageID, folder).Scan(&dup)
	return dup
}

// normSubject strips reply/forward prefixes so "Re: Fwd: Invoice" matches "Invoice".
func normSubject(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	for {
		t := strings.TrimSpace(s)
		i := strings.IndexByte(t, ':')
		if i <= 0 || i > 8 {
			return t
		}
		p := strings.TrimSpace(t[:i])
		if j := strings.IndexByte(p, '['); j > 0 { // "Re[2]"
			p = p[:j]
		}
		switch p {
		case "re", "fw", "fwd", "aw", "sv", "vs", "tr", "rif", "antw", "wg", "r", "res", "enc":
			s = t[i+1:]
		default:
			return t
		}
	}
}

func (a *App) insertMessage(userID int64, rel string, size int64, m *Meta, o DeliverOpts) (int64, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Duplicate suppression: the same Message-ID delivered twice to the same folder.
	if m.MessageID != "" && o.Folder != "sent" {
		var dup int64
		if err := tx.QueryRow(`SELECT id FROM messages WHERE user_id=? AND message_id=? AND folder=? LIMIT 1`, userID, m.MessageID, o.Folder).Scan(&dup); err == nil {
			return dup, errDuplicate
		}
	}

	// Threading via In-Reply-To / References / same Message-ID. A message only joins an
	// existing conversation when its subject (minus Re:/Fwd:) also matches, so a stranger
	// who learns a Message-ID can't slip a message into someone else's thread.
	var thread int64
	subj := normSubject(m.Subject)
	// Our own delivery reports may thread regardless of subject: nobody can send as
	// mailer-daemon@ a local domain (spoofed inbound copies are rejected).
	systemMsg := false
	if local, dom, ok := SplitAddress(m.From.Email); ok && local == "mailer-daemon" && a.IsLocalDomain(dom) {
		systemMsg = true
	}
	join := func(tid int64, threadSubject string) int64 {
		if tid > 0 && (systemMsg || normSubject(threadSubject) == subj) {
			return tid
		}
		return 0
	}
	ids := append([]string{}, m.Refs...)
	if m.InReplyTo != "" {
		ids = append(ids, m.InReplyTo)
	}
	if m.MessageID != "" {
		ids = append(ids, m.MessageID)
	}
	if len(ids) > 0 {
		if len(ids) > 50 {
			ids = ids[len(ids)-50:]
		}
		q := `SELECT thread_id, subject FROM messages WHERE user_id=? AND thread_id>0 AND message_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `) ORDER BY date DESC LIMIT 1`
		args := []any{userID}
		for _, x := range ids {
			args = append(args, x)
		}
		var tid int64
		var ts string
		tx.QueryRow(q, args...).Scan(&tid, &ts)
		thread = join(tid, ts)
	}
	if thread == 0 && m.MessageID != "" && !strings.ContainsAny(m.MessageID, " \t") {
		// A reply can arrive before its parent: link messages that already reference this one.
		// instr() matches literally (no LIKE wildcards).
		var tid int64
		var ts string
		tx.QueryRow(`SELECT thread_id, subject FROM messages WHERE user_id=? AND (in_reply_to=? OR instr(' ' || refs || ' ', ?) > 0) LIMIT 1`,
			userID, m.MessageID, " "+m.MessageID+" ").Scan(&tid, &ts)
		thread = join(tid, ts)
	}

	snippet := Snippet(stripQuoted(m.Text), 180)
	now := nowMs()
	res, err := tx.Exec(`INSERT INTO messages(user_id, folder, thread_id, message_id, in_reply_to, refs, from_addr, from_name, to_json, cc_json, bcc_json,
		reply_to, subject, snippet, date, received_at, size, has_attach, seen, spam_score, auth_summary, blob, trashed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		userID, o.Folder, thread, m.MessageID, m.InReplyTo, strings.Join(m.Refs, " "), m.From.Email, m.From.Name,
		addrsJSON(m.To), addrsJSON(m.Cc), addrsJSON(o.Bcc), m.ReplyTo, m.Subject, snippet, m.Date.UnixMilli(), now, size,
		m.HasAttach, o.Seen, o.SpamScore, o.AuthSummary, rel, trashedAt(o.Folder, now))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if thread == 0 {
		if _, err := tx.Exec(`UPDATE messages SET thread_id=? WHERE id=?`, id, id); err != nil {
			return 0, err
		}
	}
	body := m.Text
	if len(body) > 200*1024 {
		body = body[:200*1024]
	}
	if _, err := tx.Exec(`INSERT INTO messages_fts(rowid, subject, addrs, body) VALUES(?,?,?,?)`, id, m.Subject, addrsText(m, o.Bcc), body); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE users SET used_bytes = used_bytes + ? WHERE id=?`, size, userID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// trashedAt starts the auto-delete clock for messages that land in Trash or Spam.
func trashedAt(folder string, now int64) int64 {
	if folder == "trash" || folder == "spam" {
		return now
	}
	return 0
}

// stripQuoted removes quoted reply text so snippets show the new content.
func stripQuoted(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, ">") {
			continue
		}
		if strings.HasPrefix(t, "On ") && strings.HasSuffix(t, "wrote:") {
			break
		}
		if t == "-----Original Message-----" {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- listing

// ThreadRow is one row of the message list.
type ThreadRow struct {
	ThreadID     int64    `json:"thread_id"`
	IDs          []int64  `json:"ids"`
	Count        int      `json:"count"`
	Participants []string `json:"participants"`
	To           []Addr   `json:"to"`
	Subject      string   `json:"subject"`
	Snippet      string   `json:"snippet"`
	Date         int64    `json:"date"`
	Unread       bool     `json:"unread"`
	Flagged      bool     `json:"flagged"`
	HasAttach    bool     `json:"has_attach"`
	Folder       string   `json:"folder"`
	FromEmail    string   `json:"from_email"`
}

// folderCond returns the SQL condition selecting messages for a view.
func folderCond(folder string) (string, []any, error) {
	switch folder {
	case "starred":
		return `flagged=1 AND folder NOT IN ('trash','spam')`, nil, nil
	case "all":
		return `folder NOT IN ('trash','spam')`, nil, nil
	}
	if realFolders[folder] {
		return `folder=?`, []any{folder}, nil
	}
	return "", nil, errors.New("unknown folder")
}

// searchCond turns a Gmail-style query into SQL.
// Supports: from: to: subject: in: is:unread|read|starred has:attachment after:YYYY-MM-DD before:YYYY-MM-DD and free text.
func searchCond(q string) (string, []any) {
	var conds []string
	var args []any
	var free []string
	inFolder := ""
	for _, tok := range tokenizeQuery(q) {
		k, v, hasColon := strings.Cut(tok, ":")
		if !hasColon || v == "" {
			free = append(free, tok)
			continue
		}
		k = strings.ToLower(k)
		like := "%" + strings.ToLower(v) + "%"
		switch k {
		case "from":
			conds = append(conds, `(lower(from_addr) LIKE ? OR lower(from_name) LIKE ?)`)
			args = append(args, like, like)
		case "to":
			conds = append(conds, `(lower(to_json) LIKE ? OR lower(cc_json) LIKE ? OR lower(bcc_json) LIKE ?)`)
			args = append(args, like, like, like)
		case "subject":
			conds = append(conds, `lower(subject) LIKE ?`)
			args = append(args, like)
		case "in":
			v = strings.ToLower(v)
			if v == "anywhere" {
				inFolder = "*"
			} else if realFolders[v] {
				inFolder = v
			}
		case "is":
			switch strings.ToLower(v) {
			case "unread":
				conds = append(conds, `seen=0`)
			case "read":
				conds = append(conds, `seen=1`)
			case "starred":
				conds = append(conds, `flagged=1`)
			}
		case "has":
			if strings.HasPrefix(strings.ToLower(v), "attach") {
				conds = append(conds, `has_attach=1`)
			}
		case "after", "before":
			if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
				if k == "after" {
					conds = append(conds, `received_at >= ?`)
				} else {
					conds = append(conds, `received_at < ?`)
				}
				args = append(args, t.UnixMilli())
			}
		default:
			free = append(free, tok)
		}
	}
	switch inFolder {
	case "":
		conds = append(conds, `folder NOT IN ('trash','spam')`)
	case "*":
	default:
		conds = append(conds, `folder=?`)
		args = append(args, inFolder)
	}
	if len(free) > 0 {
		var terms []string
		for _, f := range free {
			f = strings.ReplaceAll(f, `"`, `""`)
			if strings.TrimSpace(f) == "" {
				continue
			}
			terms = append(terms, `"`+f+`"*`)
		}
		if len(terms) > 0 {
			conds = append(conds, `id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`)
			args = append(args, strings.Join(terms, " "))
		}
	}
	return strings.Join(conds, " AND "), args
}

func tokenizeQuery(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ListThreads returns a page of conversations for a folder or search query.
func (a *App) ListThreads(userID int64, folder, query string, offset, limit int) ([]ThreadRow, int, error) {
	var cond string
	var args []any
	if strings.TrimSpace(query) != "" {
		cond, args = searchCond(query)
	} else {
		c, ar, err := folderCond(folder)
		if err != nil {
			return nil, 0, err
		}
		cond, args = c, ar
	}
	where := `user_id=? AND ` + cond
	wargs := append([]any{userID}, args...)

	var total int
	if err := a.DB.QueryRow(`SELECT COUNT(DISTINCT thread_id) FROM messages WHERE `+where, wargs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `WITH f AS (
	  SELECT id, thread_id, folder, from_addr, from_name, to_json, subject, snippet, received_at, seen, flagged, has_attach,
	    ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY received_at DESC, id DESC) AS rn,
	    COUNT(*) OVER w AS cnt, SUM(1 - seen) OVER w AS unread, MAX(flagged) OVER w AS fl, MAX(has_attach) OVER w AS att,
	    GROUP_CONCAT(id, ',') OVER w AS ids,
	    GROUP_CONCAT(from_name || char(31) || from_addr, char(30)) OVER w AS people
	  FROM messages WHERE ` + where + `
	  WINDOW w AS (PARTITION BY thread_id ORDER BY received_at ASC, id ASC ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
	) SELECT thread_id, folder, from_addr, to_json, subject, snippet, received_at, cnt, unread, fl, att, ids, people
	  FROM f WHERE rn = 1 ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := a.DB.Query(q, append(wargs, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var owner string
	a.DB.QueryRow(`SELECT email FROM users WHERE id=?`, userID).Scan(&owner)
	out := []ThreadRow{}
	for rows.Next() {
		var r ThreadRow
		var toJSON, ids, people string
		var unread int
		if err := rows.Scan(&r.ThreadID, &r.Folder, &r.FromEmail, &toJSON, &r.Subject, &r.Snippet, &r.Date, &r.Count, &unread, &r.Flagged, &r.HasAttach, &ids, &people); err != nil {
			return nil, 0, err
		}
		r.Unread = unread > 0
		json.Unmarshal([]byte(toJSON), &r.To)
		for _, s := range strings.Split(ids, ",") {
			var id int64
			fmt.Sscan(s, &id)
			if id > 0 {
				r.IDs = append(r.IDs, id)
			}
		}
		seen := map[string]bool{}
		for _, p := range strings.Split(people, "\x1e") {
			name, addr, _ := strings.Cut(p, "\x1f")
			if addr == "" || seen[strings.ToLower(addr)] {
				continue
			}
			seen[strings.ToLower(addr)] = true
			if strings.EqualFold(addr, owner) {
				r.Participants = append(r.Participants, "me")
			} else {
				r.Participants = append(r.Participants, firstNonEmpty(name, addr))
			}
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// FolderCounts returns unread conversation counts (and draft totals).
func (a *App) FolderCounts(userID int64) map[string]int {
	out := map[string]int{"inbox": 0, "spam": 0, "drafts": 0, "starred": 0}
	rows, err := a.DB.Query(`SELECT folder, COUNT(DISTINCT thread_id) FROM messages WHERE user_id=? AND seen=0 GROUP BY folder`, userID)
	if err == nil {
		for rows.Next() {
			var f string
			var n int
			rows.Scan(&f, &n)
			out[f] = n
		}
		rows.Close()
	}
	var d int
	a.DB.QueryRow(`SELECT COUNT(*) FROM drafts WHERE user_id=?`, userID).Scan(&d)
	out["drafts"] = d
	return out
}

// ---------------------------------------------------------------- reading

// MessageView is a fully rendered message.
type MessageView struct {
	ID          int64        `json:"id"`
	ThreadID    int64        `json:"thread_id"`
	Folder      string       `json:"folder"`
	MessageID   string       `json:"message_id"`
	References  []string     `json:"references"`
	From        Addr         `json:"from"`
	To          []Addr       `json:"to"`
	Cc          []Addr       `json:"cc"`
	Bcc         []Addr       `json:"bcc"`
	ReplyTo     string       `json:"reply_to"`
	Subject     string       `json:"subject"`
	Snippet     string       `json:"snippet"`
	Date        int64        `json:"date"`
	Size        int64        `json:"size"`
	Seen        bool         `json:"seen"`
	Flagged     bool         `json:"flagged"`
	SpamScore   float64      `json:"spam_score"`
	AuthSummary string       `json:"auth_summary"`
	HTML        string       `json:"html"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
	HasRemote   bool         `json:"has_remote_images"`
}

type msgRow struct {
	id, thread                      int64
	folder, msgID, inReplyTo, refs  string
	fromAddr, fromName, to, cc, bcc string
	replyTo, subject, snippet       string
	date, size                      int64
	seen, flagged                   bool
	spam                            float64
	auth, blob                      string
}

const msgCols = `id, thread_id, folder, message_id, in_reply_to, refs, from_addr, from_name, to_json, cc_json, bcc_json, reply_to,
 subject, snippet, received_at, size, seen, flagged, spam_score, auth_summary, blob`

func scanMsg(row interface{ Scan(...any) error }) (*msgRow, error) {
	r := &msgRow{}
	err := row.Scan(&r.id, &r.thread, &r.folder, &r.msgID, &r.inReplyTo, &r.refs, &r.fromAddr, &r.fromName, &r.to, &r.cc, &r.bcc,
		&r.replyTo, &r.subject, &r.snippet, &r.date, &r.size, &r.seen, &r.flagged, &r.spam, &r.auth, &r.blob)
	return r, err
}

func (a *App) messageRow(userID, id int64) (*msgRow, error) {
	return scanMsg(a.DB.QueryRow(`SELECT `+msgCols+` FROM messages WHERE user_id=? AND id=?`, userID, id))
}

var reRemoteImg = func() func(string) bool {
	return func(s string) bool {
		ls := strings.ToLower(s)
		return strings.Contains(ls, `src="http`) || strings.Contains(ls, "url(http") || strings.Contains(ls, `background="http`)
	}
}()

func (a *App) renderMessage(r *msgRow, withBody bool) MessageView {
	v := MessageView{ID: r.id, ThreadID: r.thread, Folder: r.folder, MessageID: r.msgID, From: Addr{Name: r.fromName, Email: r.fromAddr},
		ReplyTo: r.replyTo, Subject: r.subject, Snippet: r.snippet, Date: r.date, Size: r.size, Seen: r.seen, Flagged: r.flagged,
		SpamScore: r.spam, AuthSummary: r.auth, Attachments: []Attachment{}}
	json.Unmarshal([]byte(r.to), &v.To)
	json.Unmarshal([]byte(r.cc), &v.Cc)
	json.Unmarshal([]byte(r.bcc), &v.Bcc)
	v.References = strings.Fields(r.refs)
	if !withBody {
		return v
	}
	raw, err := a.readBlob(r.blob)
	if err != nil {
		v.Text = "(This message could not be loaded.)"
		return v
	}
	b := ParseBody(raw)
	if b.HTML != "" {
		v.HTML = SanitizeHTML(b.HTML, r.id)
		v.HasRemote = reRemoteImg(v.HTML)
	}
	v.Text = b.Text
	v.Attachments = b.Attachments
	return v
}

// ThreadView returns every message of a conversation.
func (a *App) ThreadView(userID, threadID int64, context string, markSeen bool) ([]MessageView, error) {
	q := `SELECT ` + msgCols + ` FROM messages WHERE user_id=? AND thread_id=? AND (folder NOT IN ('trash','spam') OR folder=?) ORDER BY received_at ASC, id ASC`
	rows, err := a.DB.Query(q, userID, threadID, context)
	if err != nil {
		return nil, err
	}
	var list []*msgRow
	for rows.Next() {
		r, err := scanMsg(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	out := make([]MessageView, 0, len(list))
	for _, r := range list {
		out = append(out, a.renderMessage(r, true))
	}
	if markSeen {
		if res, err := a.DB.Exec(`UPDATE messages SET seen=1 WHERE user_id=? AND thread_id=? AND seen=0`, userID, threadID); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				a.Events.Publish(userID, Event{Type: "sync"})
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- actions

// ThreadAction applies an action to conversations as seen from a view.
func (a *App) ThreadAction(userID int64, threadIDs []int64, view, action string) error {
	if len(threadIDs) == 0 {
		return nil
	}
	if len(threadIDs) > 1000 {
		return errors.New("too many conversations")
	}
	scope, sargs, err := folderCond(view)
	if err != nil {
		scope, sargs = `folder NOT IN ('trash','spam')`, nil // search results
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(threadIDs)), ",")
	base := `user_id=? AND thread_id IN (` + ph + `)`
	bargs := []any{userID}
	for _, t := range threadIDs {
		bargs = append(bargs, t)
	}
	inScope := base + ` AND ` + scope
	iargs := append(append([]any{}, bargs...), sargs...)

	exec := func(q string, args ...any) error {
		_, err := a.DB.Exec(q, args...)
		return err
	}
	switch action {
	case "read":
		err = exec(`UPDATE messages SET seen=1 WHERE `+inScope, iargs...)
	case "unread":
		err = exec(`UPDATE messages SET seen=0 WHERE `+inScope, iargs...)
	case "star":
		// star the latest message of each conversation
		for _, t := range threadIDs {
			q := `UPDATE messages SET flagged=1 WHERE id = (SELECT id FROM messages WHERE user_id=? AND thread_id=? AND ` + scope + ` ORDER BY received_at DESC LIMIT 1)`
			if err = exec(q, append([]any{userID, t}, sargs...)...); err != nil {
				break
			}
		}
	case "unstar":
		err = exec(`UPDATE messages SET flagged=0 WHERE `+base, bargs...)
	case "archive":
		err = exec(`UPDATE messages SET folder='archive' WHERE `+base+` AND folder='inbox'`, bargs...)
	case "inbox":
		// Restore: messages go back where they came from (sent stays sent).
		// From Trash, messages return to the folder they were deleted from.
		err = exec(`UPDATE messages SET folder = CASE
				WHEN folder='trash' AND prev_folder IN ('inbox','archive','sent') THEN prev_folder
				WHEN prev_folder='sent' THEN 'sent'
				WHEN folder='trash' AND prev_folder='' AND from_addr IN (SELECT email FROM users WHERE id=?) THEN 'sent'
				ELSE 'inbox' END, prev_folder='', trashed_at=0
			WHERE `+inScope, append([]any{userID}, iargs...)...)
	case "trash":
		err = exec(`UPDATE messages SET prev_folder=folder, folder='trash', trashed_at=? WHERE `+inScope+` AND folder <> 'trash'`, append([]any{nowMs()}, iargs...)...)
	case "spam":
		err = exec(`UPDATE messages SET prev_folder=folder, folder='spam', trashed_at=? WHERE `+inScope+` AND folder NOT IN ('sent','spam')`, append([]any{nowMs()}, iargs...)...)
	case "delete":
		if view != "trash" && view != "spam" {
			return errors.New("Only messages in Trash or Spam can be deleted forever")
		}
		err = a.deleteMessages(`SELECT id, size, blob FROM messages WHERE `+inScope, iargs...)
	default:
		return errors.New("unknown action")
	}
	if err != nil {
		return err
	}
	a.Events.Publish(userID, Event{Type: "sync"})
	return nil
}

// SetFlag stars / unstars a single message.
func (a *App) SetFlag(userID, msgID int64, flagged bool) error {
	_, err := a.DB.Exec(`UPDATE messages SET flagged=? WHERE user_id=? AND id=?`, flagged, userID, msgID)
	a.Events.Publish(userID, Event{Type: "sync"})
	return err
}

// deleteMessages permanently removes the messages selected by q (columns id, size, blob).
func (a *App) deleteMessages(q string, args ...any) error {
	rows, err := a.DB.Query(q, args...)
	if err != nil {
		return err
	}
	type victim struct {
		id, size int64
		blob     string
	}
	var vs []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.size, &v.blob); err != nil {
			rows.Close()
			return err
		}
		vs = append(vs, v)
	}
	rows.Close()
	if len(vs) == 0 {
		return nil
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range vs {
		var uid int64
		if err := tx.QueryRow(`DELETE FROM messages WHERE id=? RETURNING user_id`, v.id).Scan(&uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if _, err := tx.Exec(`DELETE FROM messages_fts WHERE rowid=?`, v.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET used_bytes = MAX(0, used_bytes - ?) WHERE id=?`, v.size, uid); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, v := range vs {
		os.Remove(a.blobPath(v.blob))
	}
	return nil
}
