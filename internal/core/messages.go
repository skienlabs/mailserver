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
	Flagged      bool  // starred on arrival (imports)
	ReceivedAt   int64 // ms; 0 = now (imports keep the original date)
	Quiet        bool  // don't publish a sync event (imports publish in batches)
	// AllowDuplicate stores the message even if one with the same Message-ID exists (an
	// explicit copy from a mail app must never be dropped as a duplicate).
	AllowDuplicate bool
	Answered       bool
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
	if !o.AllowDuplicate {
		if dup := a.duplicateOf(userID, m, o.Folder); dup > 0 {
			return dup, nil // already delivered (e.g. a sender retry): store nothing
		}
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
	if o.Quiet {
		// caller publishes
	} else if o.Notify {
		a.Events.Publish(userID, Event{Type: "mail", Folder: o.Folder, From: firstNonEmpty(m.From.Name, m.From.Email), Subject: m.Subject})
		if o.Folder == "inbox" && !o.Seen {
			a.push.newMail(userID, id, firstNonEmpty(m.From.Name, m.From.Email), m.Subject)
		}
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

// duplicateOf returns the ID of an identical received message (same Message-ID) already stored
// in any folder but Sent — a sender's retry after the user archived or deleted the first copy
// must not bring it back.
func (a *App) duplicateOf(userID int64, m *Meta, folder string) int64 {
	if m.MessageID == "" || folder == "sent" {
		return 0
	}
	var dup int64
	a.DB.QueryRow(`SELECT id FROM messages WHERE user_id=? AND message_id=? AND folder<>'sent' AND prev_folder<>'sent' LIMIT 1`, userID, m.MessageID).Scan(&dup)
	return dup
}

// normSubject strips reply/forward prefixes so "Re: Fwd: Invoice" matches "Invoice".
func normSubject(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	for {
		t := strings.TrimSpace(s)
		// Leading tags added by gateways and lists: "[EXTERNAL] Re: Budget", "[team] Budget".
		if strings.HasPrefix(t, "[") {
			if j := strings.IndexByte(t, ']'); j > 0 && j <= 40 && strings.TrimSpace(t[j+1:]) != "" {
				s = t[j+1:]
				continue
			}
		}
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

	// Duplicate suppression: the same Message-ID received twice (Sent copies don't count).
	if m.MessageID != "" && o.Folder != "sent" && !o.AllowDuplicate {
		var dup int64
		if err := tx.QueryRow(`SELECT id FROM messages WHERE user_id=? AND message_id=? AND folder<>'sent' AND prev_folder<>'sent' LIMIT 1`, userID, m.MessageID).Scan(&dup); err == nil {
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
		// Newest first; join the first referenced message whose subject matches.
		q := `SELECT thread_id, subject FROM messages WHERE user_id=? AND thread_id>0 AND message_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `) ORDER BY date DESC LIMIT 100`
		args := []any{userID}
		for _, x := range ids {
			args = append(args, x)
		}
		if rows, err := tx.Query(q, args...); err == nil {
			for rows.Next() && thread == 0 {
				var tid int64
				var ts string
				if rows.Scan(&tid, &ts) == nil {
					thread = join(tid, ts)
				}
			}
			rows.Close()
		}
	}
	if thread == 0 && m.MessageID != "" && !strings.ContainsAny(m.MessageID, " \t") {
		// A reply can arrive before its parent: link messages that already reference this one.
		// instr() matches literally (no LIKE wildcards).
		var tid int64
		var ts string
		tx.QueryRow(`SELECT m.thread_id, m.subject FROM message_refs r JOIN messages m ON m.id = r.msg_id WHERE r.user_id=? AND r.ref=? LIMIT 1`,
			userID, m.MessageID).Scan(&tid, &ts)
		thread = join(tid, ts)
	}
	if thread == 0 && m.InReplyTo != "" {
		// Siblings: another reply to the same (not yet received) parent.
		var tid int64
		var ts string
		tx.QueryRow(`SELECT m.thread_id, m.subject FROM message_refs r JOIN messages m ON m.id = r.msg_id
			WHERE r.user_id=? AND r.ref=? AND m.thread_id>0 ORDER BY m.date DESC LIMIT 1`, userID, m.InReplyTo).Scan(&tid, &ts)
		thread = join(tid, ts)
	}

	snippet := Snippet(stripQuoted(m.Text), 180)
	now := nowMs()
	received := now
	if o.ReceivedAt > 0 && o.ReceivedAt <= now+24*3600*1000 {
		received = o.ReceivedAt
	}
	res, err := tx.Exec(`INSERT INTO messages(user_id, folder, thread_id, message_id, in_reply_to, refs, from_addr, from_name, to_json, cc_json, bcc_json,
		reply_to, subject, snippet, date, received_at, size, has_attach, seen, flagged, answered, spam_score, auth_summary, blob, trashed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		userID, o.Folder, thread, m.MessageID, m.InReplyTo, strings.Join(m.Refs, " "), m.From.Email, m.From.Name,
		addrsJSON(m.To), addrsJSON(m.Cc), addrsJSON(o.Bcc), m.ReplyTo, m.Subject, snippet, m.Date.UnixMilli(), received, size,
		m.HasAttach, o.Seen, o.Flagged, o.Answered, o.SpamScore, o.AuthSummary, rel, trashedAt(o.Folder, now))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if thread == 0 {
		if _, err := tx.Exec(`UPDATE messages SET thread_id=? WHERE id=?`, id, id); err != nil {
			return 0, err
		}
	}
	// Index In-Reply-To/References so a later parent can find its early replies without a scan.
	refSeen := map[string]bool{}
	for _, r := range append(append([]string{}, m.Refs...), m.InReplyTo) {
		if r == "" || strings.ContainsAny(r, " \t") || refSeen[r] {
			continue
		}
		refSeen[r] = true
		if _, err := tx.Exec(`INSERT OR IGNORE INTO message_refs(user_id, ref, msg_id) VALUES(?,?,?)`, userID, r, id); err != nil {
			return 0, err
		}
	}
	body := m.Text
	if len(body) > 200*1024 {
		body = body[:200*1024]
	}
	// Message IDs can be reused after deletes; clear any stale index entry for this ID first.
	if _, err := tx.Exec(`DELETE FROM messages_fts WHERE rowid=?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO messages_fts(rowid, subject, addrs, body) VALUES(?,?,?,?)`, id, m.Subject, addrsText(m, o.Bcc), body); err != nil {
		return 0, err
	}
	if o.EnforceQuota {
		// Checked again inside the write transaction: parallel deliveries can't overshoot the quota.
		res, err := tx.Exec(`UPDATE users SET used_bytes = used_bytes + ? WHERE id=? AND (quota_bytes <= 0 OR used_bytes + ? <= quota_bytes)`, size, userID, size)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, ErrQuota
		}
	} else if _, err := tx.Exec(`UPDATE users SET used_bytes = used_bytes + ? WHERE id=?`, size, userID); err != nil {
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
var errUnknownFolder = errors.New("unknown folder")

func folderCond(folder string) (string, []any, error) {
	switch folder {
	case "starred":
		return `flagged=1 AND folder NOT IN ('trash','spam')`, nil, nil
	case "all":
		return `folder NOT IN ('trash','spam')`, nil, nil
	}
	if realFolders[folder] || isCustomFolder(folder) {
		return `folder=?`, []any{folder}, nil
	}
	return "", nil, errUnknownFolder
}

// searchCond turns a Gmail-style query into SQL.
// Supports: from: to: subject: in: is:unread|read|starred has:attachment after:YYYY-MM-DD before:YYYY-MM-DD and free text.
func searchCond(q string) (string, []any) {
	q = strings.ReplaceAll(q, "\x00", "")
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
			} else if realFolders[v] || isCustomFolder(v) {
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
	if len(conds) == 0 {
		return "1", nil
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
//
// Conversations are ranked by their newest matching message (received_at DESC, id DESC) and
// aggregate only the messages matching the view — the same result as one window query over
// the whole folder, without computing it for every conversation:
//   - folder views walk a covering index newest-first and stop after offset+limit threads;
//   - searches run their (FTS/LIKE) filter once and page the matches in Go;
//   - then only the messages of the page's threads are loaded and aggregated.
func (a *App) ListThreads(userID int64, folder, query string, offset, limit int) ([]ThreadRow, int, error) {
	search := strings.TrimSpace(query) != ""
	var cond string
	var args []any
	if search {
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
	out := []ThreadRow{}

	var total int
	var page []int64
	var pageIDs []int64 // search: the matching messages of the page's threads
	if search {
		rows, err := a.DB.Query(`SELECT id, thread_id FROM messages WHERE `+where+` ORDER BY +received_at DESC, +id DESC`, wargs...) // "+": all matches are read anyway, so let WHERE pick the index
		if err != nil {
			return nil, 0, err
		}
		pos := map[int64]int{} // thread -> rank
		var order []int64
		ids := map[int64][]int64{}
		for rows.Next() {
			var id, tid int64
			if err := rows.Scan(&id, &tid); err != nil {
				rows.Close()
				return nil, 0, err
			}
			r, ok := pos[tid]
			if !ok {
				r = len(order)
				pos[tid] = r
				order = append(order, tid)
			}
			if r >= offset && r < offset+limit {
				ids[tid] = append(ids[tid], id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
		total = len(order)
		if offset < len(order) {
			page = order[offset:min(len(order), offset+limit)]
		}
		for _, t := range page {
			pageIDs = append(pageIDs, ids[t]...)
		}
	} else {
		countQ := `SELECT COUNT(DISTINCT thread_id) FROM messages WHERE ` + where
		if folder == "all" {
			// merge three sorted index ranges instead of de-duplicating ~all rows in a temp B-tree
			countQ = `SELECT COUNT(*) FROM (SELECT thread_id FROM messages WHERE user_id=?1 AND folder='inbox'
				UNION SELECT thread_id FROM messages WHERE user_id=?1 AND folder='archive'
				UNION SELECT thread_id FROM messages WHERE user_id=?1 AND folder='sent'
				UNION SELECT thread_id FROM messages WHERE user_id=?1 AND folder >= 'f:' AND folder < 'f;')`
		}
		if err := a.DB.QueryRow(countQ, wargs...).Scan(&total); err != nil {
			return nil, 0, err
		}
		if offset >= total {
			return out, total, nil
		}
		rows, err := a.DB.Query(`SELECT thread_id FROM messages WHERE `+where+` ORDER BY received_at DESC, id DESC`, wargs...)
		if err != nil {
			return nil, 0, err
		}
		seenT := make(map[int64]bool, offset+limit)
		for rows.Next() {
			var tid int64
			if err := rows.Scan(&tid); err != nil {
				rows.Close()
				return nil, 0, err
			}
			if seenT[tid] {
				continue
			}
			seenT[tid] = true
			if len(seenT) > offset {
				page = append(page, tid)
				if len(page) == limit {
					break
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}
	if len(page) == 0 {
		return out, total, nil
	}

	// Look the owner up before opening rows: a second query while rows is open needs a
	// second pool connection, which deadlocks the pool once MaxOpenConns requests do it.
	var owner string
	a.DB.QueryRow(`SELECT email FROM users WHERE id=?`, userID).Scan(&owner)

	const cols = `SELECT id, thread_id, folder, from_addr, from_name, to_json, subject, snippet, received_at, seen, flagged, has_attach FROM messages `
	var rows *sql.Rows
	var err error
	if search {
		idj, _ := json.Marshal(pageIDs)
		rows, err = a.DB.Query(cols+`WHERE id IN (SELECT value FROM json_each(?)) ORDER BY thread_id, received_at ASC, id ASC`, string(idj))
	} else {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(page)), ",")
		dargs := append([]any{}, wargs...)
		for _, t := range page {
			dargs = append(dargs, t)
		}
		rows, err = a.DB.Query(cols+`WHERE `+where+` AND thread_id IN (`+ph+`) ORDER BY thread_id, received_at ASC, id ASC`, dargs...)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type agg struct {
		r      ThreadRow
		toJSON string
		seen   map[string]bool
	}
	byT := make(map[int64]*agg, len(page))
	for rows.Next() {
		var id, tid, date int64
		var fold, fromAddr, fromName, toJSON, subject, snippet string
		var seen, flagged, att bool
		if err := rows.Scan(&id, &tid, &fold, &fromAddr, &fromName, &toJSON, &subject, &snippet, &date, &seen, &flagged, &att); err != nil {
			return nil, 0, err
		}
		g := byT[tid]
		if g == nil {
			g = &agg{seen: map[string]bool{}}
			g.r.ThreadID = tid
			byT[tid] = g
		}
		r := &g.r
		r.Count++
		r.IDs = append(r.IDs, id)
		r.Unread = r.Unread || !seen
		r.Flagged = r.Flagged || flagged
		r.HasAttach = r.HasAttach || att
		// rows are oldest-first, so the last one wins = newest message
		r.Folder, r.FromEmail, g.toJSON, r.Subject, r.Snippet, r.Date = fold, fromAddr, toJSON, subject, snippet, date
		if la := strings.ToLower(fromAddr); fromAddr != "" && !g.seen[la] {
			g.seen[la] = true
			if strings.EqualFold(fromAddr, owner) {
				r.Participants = append(r.Participants, "me")
			} else {
				r.Participants = append(r.Participants, firstNonEmpty(fromName, fromAddr))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, t := range page {
		if g := byT[t]; g != nil { // nil: changed between the queries
			json.Unmarshal([]byte(g.toJSON), &g.r.To)
			out = append(out, g.r)
		}
	}
	return out, total, nil
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
	_, err := a.ThreadActionIDs(userID, threadIDs, view, action, nil)
	return err
}

// ThreadActionIDs applies action to conversations as seen in view. For moves (archive, trash,
// spam, inbox) it returns the IDs of the messages that actually moved, so an Undo can move back
// exactly those — not messages of the same conversation that were already there. A non-nil
// onlyIDs limits the action to those messages.
func (a *App) ThreadActionIDs(userID int64, threadIDs []int64, view, action string, onlyIDs []int64) ([]int64, error) {
	if len(threadIDs) == 0 {
		return nil, nil
	}
	if len(threadIDs) > 1000 || len(onlyIDs) > 100000 {
		return nil, errors.New("too many conversations")
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
	if onlyIDs != nil {
		b, _ := json.Marshal(onlyIDs)
		base += ` AND id IN (SELECT value FROM json_each(?))`
		bargs = append(bargs, string(b))
	}
	inScope := base + ` AND ` + scope
	iargs := append(append([]any{}, bargs...), sargs...)

	exec := func(q string, args ...any) error {
		_, err := a.DB.Exec(q, args...)
		return err
	}
	var moved []int64
	move := func(q string, args ...any) error {
		rows, err := a.DB.Query(q+` RETURNING id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			moved = append(moved, id)
		}
		return rows.Err()
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
		if isCustomFolder(view) {
			// From one of your folders: its messages remember where they came from (for Undo),
			// and the conversation's Inbox messages are archived too.
			err = move(`UPDATE messages SET folder='archive', prev_folder = CASE WHEN prev_folder='sent' THEN 'sent' ELSE ? END
				WHERE `+base+` AND folder=?`, append(append([]any{view}, bargs...), view)...)
			if err == nil {
				err = move(`UPDATE messages SET folder='archive' WHERE `+base+` AND folder='inbox'`, bargs...)
			}
		} else {
			err = move(`UPDATE messages SET folder='archive' WHERE `+base+` AND folder='inbox'`, bargs...)
		}
	case "unarchive":
		// Undo of Archive: each message goes back to the folder it was archived from (Inbox
		// unless it came from one of your folders).
		if view != "archive" {
			return nil, errors.New("unknown action")
		}
		err = move(`UPDATE messages SET folder = CASE
				WHEN prev_folder IN (SELECT 'f:' || id FROM user_folders WHERE user_id=?) THEN prev_folder
				WHEN prev_folder='sent' THEN 'sent'
				ELSE 'inbox' END, prev_folder = CASE WHEN prev_folder='sent' THEN 'sent' ELSE '' END
			WHERE `+inScope, append([]any{userID}, iargs...)...)
	case "inbox":
		// Restore: messages go back where they came from (sent stays sent).
		// From Trash, messages return to the folder they were deleted from.
		err = move(`UPDATE messages SET folder = CASE
				WHEN folder='trash' AND (prev_folder IN ('inbox','archive','sent') OR prev_folder IN (SELECT 'f:' || id FROM user_folders WHERE user_id=?)) THEN prev_folder
				WHEN prev_folder='sent' THEN 'sent'
				WHEN folder='trash' AND prev_folder='' AND from_addr IN (SELECT email FROM users WHERE id=?) THEN 'sent'
				ELSE 'inbox' END, prev_folder='', trashed_at=0
			WHERE `+inScope, append([]any{userID, userID}, iargs...)...)
	case "trash":
		err = move(`UPDATE messages SET prev_folder=folder, folder='trash', trashed_at=? WHERE `+inScope+` AND folder <> 'trash'`, append([]any{nowMs()}, iargs...)...)
	case "spam":
		err = move(`UPDATE messages SET prev_folder=folder, folder='spam', trashed_at=? WHERE `+inScope+` AND folder NOT IN ('sent','spam')`, append([]any{nowMs()}, iargs...)...)
	case "delete":
		if view != "trash" && view != "spam" {
			return nil, errors.New("Only messages in Trash or Spam can be deleted forever")
		}
		err = a.deleteMessages(`SELECT id, size, blob FROM messages WHERE `+inScope, iargs...)
	default:
		target, isMove := strings.CutPrefix(action, "move:")
		if !isMove || !a.validMoveTarget(userID, target) {
			return nil, errors.New("unknown action")
		}
		// Sent mail filed into a folder stays recognisable as sent (prev_folder) so a later
		// "Move to Inbox" puts it back in Sent.
		err = move(`UPDATE messages SET prev_folder = CASE WHEN folder='sent' OR prev_folder='sent' THEN 'sent' ELSE '' END,
			folder=?, trashed_at=0 WHERE `+inScope+` AND folder <> ?`, append(append([]any{target}, iargs...), target)...)
	}
	if err != nil {
		return nil, err
	}
	a.Events.Publish(userID, Event{Type: "sync"})
	return moved, nil
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
	owners := map[int64]bool{}
	for _, v := range vs {
		var uid int64
		if err := tx.QueryRow(`DELETE FROM messages WHERE id=? RETURNING user_id`, v.id).Scan(&uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		owners[uid] = true
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
	for uid := range owners { // e.g. Trash emptied by the janitor: open apps refresh
		a.Events.Publish(uid, Event{Type: "sync"})
	}
	return nil
}
