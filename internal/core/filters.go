package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Filters (rules) run on mail as it arrives: "from X → folder Y, mark as read, star".
// Blocked senders go to Spam; always-allowed senders skip the spam filter, but only when the
// sender's domain is verified (SPF or DKIM aligned with it, and DMARC not failing), so a forged
// copy of an allowed address still lands in Spam.

const (
	maxFilters       = 200
	maxFilterField   = 500
	maxSenderEntries = 2000
)

// Filter is one rule. Conditions are AND-ed; within a condition, comma-separated
// alternatives are OR-ed ("amazon.in, flipkart.com"). Matching is case-insensitive.
type Filter struct {
	ID       int64  `json:"id"`
	Enabled  bool   `json:"enabled"`
	From     string `json:"from"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Words    string `json:"words"`
	Folder   string `json:"folder"` // "" (leave where it is), archive, trash or a custom folder key
	MarkRead bool   `json:"mark_read"`
	Star     bool   `json:"star"`
	Hits     int64  `json:"hits"`
	LastHit  int64  `json:"last_hit"`
}

func (a *App) ListFilters(userID int64) ([]Filter, error) {
	rows, err := a.DB.Query(`SELECT id, enabled, match_from, match_to, match_subject, match_words, action_folder, mark_read, star, hits, last_hit
		FROM filters WHERE user_id=? ORDER BY position, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Filter{}
	for rows.Next() {
		var f Filter
		if err := rows.Scan(&f.ID, &f.Enabled, &f.From, &f.To, &f.Subject, &f.Words, &f.Folder, &f.MarkRead, &f.Star, &f.Hits, &f.LastHit); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// cleanAlternatives normalises a comma-separated list: trimmed, lower-case, no empties.
func cleanAlternatives(s string) (string, []string) {
	var keep []string
	for _, p := range strings.Split(strings.ToValidUTF8(s, ""), ",") {
		p = strings.ToLower(strings.Join(strings.Fields(p), " "))
		if p != "" && len(keep) < 50 {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ", "), keep
}

func (a *App) validateFilter(userID int64, f *Filter) error {
	for _, v := range []*string{&f.From, &f.To, &f.Subject, &f.Words} {
		if len(*v) > maxFilterField {
			return fmt.Errorf("Each condition can be at most %d characters", maxFilterField)
		}
		*v, _ = cleanAlternatives(*v)
	}
	if f.From == "" && f.To == "" && f.Subject == "" && f.Words == "" {
		return errors.New("Add at least one condition (from, to, subject or words)")
	}
	switch f.Folder {
	case "", "archive", "trash":
	default:
		if _, err := a.folderByKey(userID, f.Folder); err != nil {
			return errors.New("Choose an existing folder")
		}
	}
	if f.Folder == "" && !f.MarkRead && !f.Star {
		return errors.New("Choose what the filter should do")
	}
	return nil
}

func (a *App) CreateFilter(userID int64, f Filter) (Filter, error) {
	if err := a.validateFilter(userID, &f); err != nil {
		return f, err
	}
	var n, pos int
	a.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(position), 0) FROM filters WHERE user_id=?`, userID).Scan(&n, &pos)
	if n >= maxFilters {
		return f, fmt.Errorf("You can have at most %d filters", maxFilters)
	}
	res, err := a.DB.Exec(`INSERT INTO filters(user_id, position, enabled, match_from, match_to, match_subject, match_words, action_folder, mark_read, star, created_at)
		VALUES(?,?,1,?,?,?,?,?,?,?,?)`, userID, pos+1, f.From, f.To, f.Subject, f.Words, f.Folder, f.MarkRead, f.Star, nowMs())
	if err != nil {
		return f, err
	}
	f.ID, _ = res.LastInsertId()
	f.Enabled = true
	return f, nil
}

func (a *App) UpdateFilter(userID int64, f Filter) (Filter, error) {
	if err := a.validateFilter(userID, &f); err != nil {
		return f, err
	}
	res, err := a.DB.Exec(`UPDATE filters SET enabled=?, match_from=?, match_to=?, match_subject=?, match_words=?, action_folder=?, mark_read=?, star=?
		WHERE id=? AND user_id=?`, f.Enabled, f.From, f.To, f.Subject, f.Words, f.Folder, f.MarkRead, f.Star, f.ID, userID)
	if err != nil {
		return f, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return f, sql.ErrNoRows
	}
	return f, nil
}

func (a *App) DeleteFilter(userID, id int64) {
	a.DB.Exec(`DELETE FROM filters WHERE id=? AND user_id=?`, id, userID)
}

// ReorderFilters sets the order of the user's filters (ids not listed keep their place after).
func (a *App) ReorderFilters(userID int64, ids []int64) error {
	if len(ids) > maxFilters {
		return errors.New("too many filters")
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE filters SET position=? WHERE id=? AND user_id=?`, i+1, id, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mailFacts is what filters look at.
type mailFacts struct {
	from, to, subject, words string // lower-case
}

func factsOf(m *Meta, rcpts []string) mailFacts {
	var to strings.Builder
	for _, x := range append(append([]Addr{}, m.To...), m.Cc...) {
		to.WriteString(x.Name + " " + x.Email + " ")
	}
	for _, r := range rcpts {
		to.WriteString(r + " ")
	}
	text := m.Text
	if len(text) > 200*1024 {
		text = text[:200*1024]
	}
	// Conditions are stored with single spaces; compare against text spaced the same way.
	norm := func(x string) string { return strings.ToLower(strings.Join(strings.Fields(x), " ")) }
	return mailFacts{
		from:    norm(m.From.Name + " " + m.From.Email),
		to:      norm(to.String()),
		subject: norm(m.Subject),
		words:   norm(m.Subject + "\n" + text),
	}
}

func anyIn(target, alternatives string) bool {
	if alternatives == "" {
		return true
	}
	_, alts := cleanAlternatives(alternatives)
	for _, x := range alts {
		if strings.Contains(target, x) {
			return true
		}
	}
	return false
}

func (f *Filter) matches(x mailFacts) bool {
	return f.Enabled && anyIn(x.from, f.From) && anyIn(x.to, f.To) && anyIn(x.subject, f.Subject) && anyIn(x.words, f.Words)
}

// arrival is how a new message should be stored.
type arrival struct {
	Folder  string
	Seen    bool
	Flagged bool
}

// decideArrival applies the user's sender lists and filters to an incoming message.
// base is the folder chosen so far ("inbox", or "spam" when the spam check flagged it);
// fromAuth says the sender's domain was verified (InboundVerdict.FromAuth, or internal mail).
func (a *App) decideArrival(userID int64, m *Meta, rcpts []string, base string, fromAuth bool) arrival {
	out := arrival{Folder: base}
	switch a.senderVerdict(userID, m.From.Email) {
	case "block":
		return arrival{Folder: "spam"}
	case "allow":
		if base == "spam" && fromAuth {
			out.Folder = "inbox"
		}
	}
	if out.Folder == "spam" {
		return out
	}
	filters, err := a.ListFilters(userID)
	if err != nil || len(filters) == 0 {
		return out
	}
	facts := factsOf(m, rcpts)
	var hit []int64
	moved := false
	for i := range filters {
		f := &filters[i]
		if !f.matches(facts) {
			continue
		}
		hit = append(hit, f.ID)
		if f.Folder != "" && !moved {
			if f.Folder == "trash" || f.Folder == "archive" {
				out.Folder, moved = f.Folder, true
			} else if _, err := a.folderByKey(userID, f.Folder); err == nil {
				out.Folder, moved = f.Folder, true
			}
		}
		out.Seen = out.Seen || f.MarkRead
		out.Flagged = out.Flagged || f.Star
	}
	if len(hit) > 0 {
		b, _ := json.Marshal(hit)
		a.DB.Exec(`UPDATE filters SET hits=hits+1, last_hit=? WHERE user_id=? AND id IN (SELECT value FROM json_each(?))`, nowMs(), userID, string(b))
	}
	return out
}

// ApplyFilter runs a filter over mail already in the mailbox (not Sent, Spam or Trash) and
// returns how many messages it changed.
func (a *App) ApplyFilter(userID, filterID int64) (int, error) {
	var f Filter
	err := a.DB.QueryRow(`SELECT id, match_from, match_to, match_subject, match_words, action_folder, mark_read, star FROM filters WHERE id=? AND user_id=?`,
		filterID, userID).Scan(&f.ID, &f.From, &f.To, &f.Subject, &f.Words, &f.Folder, &f.MarkRead, &f.Star)
	if err != nil {
		return 0, err
	}
	f.Enabled = true
	if f.Folder != "" && f.Folder != "archive" && f.Folder != "trash" {
		if _, err := a.folderByKey(userID, f.Folder); err != nil {
			f.Folder = ""
		}
	}
	q := `SELECT id, from_addr, from_name, to_json, cc_json, subject, blob, folder FROM messages
		WHERE user_id=? AND folder NOT IN ('sent','trash','spam') AND prev_folder<>'sent'`
	args := []any{userID}
	if f.Words != "" {
		// Only messages whose text contains one of the words (full-text index), checked exactly below.
		_, alts := cleanAlternatives(f.Words)
		var terms []string
		for _, w := range alts {
			terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
		}
		q += ` AND id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`
		args = append(args, strings.Join(terms, " OR "))
	}
	q += ` ORDER BY received_at DESC LIMIT 20000`
	rows, err := a.DB.Query(q, args...)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id                    int64
		from, to, cc, subject string
		blob, folder          string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var fromName string
		if err := rows.Scan(&c.id, &c.from, &fromName, &c.to, &c.cc, &c.subject, &c.blob, &c.folder); err != nil {
			rows.Close()
			return 0, err
		}
		c.from = fromName + " " + c.from
		cands = append(cands, c)
	}
	rows.Close()
	var ids []int64
	for _, c := range cands {
		var to, cc []Addr
		json.Unmarshal([]byte(c.to), &to)
		json.Unmarshal([]byte(c.cc), &cc)
		m := &Meta{From: Addr{Email: c.from}, To: to, Cc: cc, Subject: c.subject}
		if f.Words != "" {
			if raw, err := a.readBlob(c.blob); err == nil {
				m.Text = ParseBody(raw).Text
			}
		}
		if f.matches(factsOf(m, nil)) {
			ids = append(ids, c.id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	b, _ := json.Marshal(ids)
	set := []string{}
	sargs := []any{}
	if f.MarkRead {
		set = append(set, "seen=1")
	}
	if f.Star {
		set = append(set, "flagged=1")
	}
	switch f.Folder {
	case "":
	case "trash":
		set = append(set, "prev_folder=folder", "folder='trash'", "trashed_at=?")
		sargs = append(sargs, nowMs())
	default:
		set = append(set, "folder=?")
		sargs = append(sargs, f.Folder)
	}
	if len(set) == 0 {
		return 0, nil
	}
	sargs = append(sargs, userID, string(b))
	res, err := a.DB.Exec(`UPDATE messages SET `+strings.Join(set, ", ")+` WHERE user_id=? AND id IN (SELECT value FROM json_each(?))`, sargs...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	a.Events.Publish(userID, Event{Type: "sync"})
	return int(n), nil
}

// ---------------------------------------------------------------- blocked / allowed senders

// SenderEntry is one address or domain on the user's lists.
type SenderEntry struct {
	Pattern   string `json:"pattern"` // "name@example.com" or "@example.com"
	Kind      string `json:"kind"`    // block | allow
	CreatedAt int64  `json:"created_at"`
}

// cleanSenderPattern accepts an address or a domain ("example.com", "@example.com").
func cleanSenderPattern(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.Trim(p, "<>")
	if p == "" {
		return "", errors.New("Enter an email address or a domain")
	}
	if strings.HasPrefix(p, "@") || !strings.Contains(p, "@") {
		dom := strings.TrimPrefix(p, "@")
		if !ValidDomainName(dom) || !strings.Contains(dom, ".") {
			return "", errors.New("That isn't a valid domain")
		}
		return "@" + dom, nil
	}
	local, dom, ok := SplitAddress(p)
	if !ok || !dotAtom(local) || !ValidDomainName(dom) || !strings.Contains(dom, ".") || len(p) > 254 {
		return "", errors.New("That isn't a valid email address")
	}
	return local + "@" + dom, nil
}

func (a *App) ListSenders(userID int64) ([]SenderEntry, error) {
	rows, err := a.DB.Query(`SELECT pattern, kind, created_at FROM sender_lists WHERE user_id=? ORDER BY kind, pattern`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SenderEntry{}
	for rows.Next() {
		var e SenderEntry
		if err := rows.Scan(&e.Pattern, &e.Kind, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetSender puts a pattern on the block or allow list (replacing any earlier entry for it).
func (a *App) SetSender(userID int64, pattern, kind string) (SenderEntry, error) {
	if kind != "block" && kind != "allow" {
		return SenderEntry{}, errors.New("unknown list")
	}
	p, err := cleanSenderPattern(pattern)
	if err != nil {
		return SenderEntry{}, err
	}
	if u, err := a.UserByID(userID); err == nil && kind == "block" && strings.EqualFold(p, u.Email) {
		return SenderEntry{}, errors.New("You can't block your own address")
	}
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM sender_lists WHERE user_id=? AND pattern<>?`, userID, p).Scan(&n)
	if n >= maxSenderEntries {
		return SenderEntry{}, fmt.Errorf("Your lists can hold at most %d entries", maxSenderEntries)
	}
	now := nowMs()
	if _, err := a.DB.Exec(`INSERT INTO sender_lists(user_id, pattern, kind, created_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id, pattern) DO UPDATE SET kind=excluded.kind, created_at=excluded.created_at`, userID, p, kind, now); err != nil {
		return SenderEntry{}, err
	}
	return SenderEntry{Pattern: p, Kind: kind, CreatedAt: now}, nil
}

func (a *App) RemoveSender(userID int64, pattern string) {
	p, err := cleanSenderPattern(pattern)
	if err != nil {
		return
	}
	a.DB.Exec(`DELETE FROM sender_lists WHERE user_id=? AND pattern=?`, userID, p)
}

// senderVerdict returns "block", "allow" or "" for a From address: an entry for the exact
// address wins over one for its domain, a domain over its parent domains.
func (a *App) senderVerdict(userID int64, from string) string {
	kind, _ := a.senderMatch(userID, from)
	return kind
}

// senderMatch returns the list entry that decides for this sender: its kind and its pattern
// (the address itself, "@domain" or "@parent.domain").
func (a *App) senderMatch(userID int64, from string) (kind, pattern string) {
	from = strings.ToLower(strings.TrimSpace(from))
	local, dom, ok := SplitAddress(from)
	if !ok || local == "" {
		return "", ""
	}
	cands := []string{from}
	labels := strings.Split(dom, ".")
	for i := 0; i < len(labels)-1; i++ {
		cands = append(cands, "@"+strings.Join(labels[i:], "."))
	}
	b, _ := json.Marshal(cands)
	rows, err := a.DB.Query(`SELECT pattern, kind FROM sender_lists WHERE user_id=? AND pattern IN (SELECT value FROM json_each(?))`, userID, string(b))
	if err != nil {
		return "", ""
	}
	found := map[string]string{}
	for rows.Next() {
		var p, k string
		if rows.Scan(&p, &k) == nil {
			found[p] = k
		}
	}
	rows.Close()
	for _, c := range cands { // most specific first
		if k, ok := found[c]; ok {
			return k, c
		}
	}
	return "", ""
}

// MoveSenderToSpam moves existing mail from a blocked pattern to Spam; returns the count.
func (a *App) MoveSenderToSpam(userID int64, pattern string) (int, error) {
	p, err := cleanSenderPattern(pattern)
	if err != nil {
		return 0, err
	}
	cond, arg := `lower(from_addr)=?`, p
	if strings.HasPrefix(p, "@") {
		cond = `(lower(from_addr) LIKE ? ESCAPE '\' OR lower(from_addr) LIKE ? ESCAPE '\')`
	}
	q := `UPDATE messages SET prev_folder=folder, folder='spam', trashed_at=? WHERE user_id=? AND folder NOT IN ('sent','spam','trash') AND prev_folder<>'sent' AND ` + cond
	var res sql.Result
	if strings.HasPrefix(p, "@") {
		res, err = a.DB.Exec(q, nowMs(), userID, "%"+likeEscape(p), "%."+likeEscape(p[1:]))
	} else {
		res, err = a.DB.Exec(q, nowMs(), userID, arg)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		a.Events.Publish(userID, Event{Type: "sync"})
	}
	return int(n), nil
}
