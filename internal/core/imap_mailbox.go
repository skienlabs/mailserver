package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-imap/responses"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

// imapMailbox is a folder as one connection sees it. While selected it keeps that connection's
// view (the message list in UID order = sequence numbers) and only changes it while telling the
// app: EXPUNGE / EXISTS / FETCH FLAGS on NOOP, IDLE and CHECK, and after its own MOVE.
type imapMailbox struct {
	s     *imapUser
	key   string
	name  string
	attrs []string

	mu       sync.Mutex
	write    func(imap.WriterTo) error // set while selected on a connection
	ro       bool                      // opened with EXAMINE
	loaded   bool
	gen      uint64
	validity uint32
	uidNext  uint32
	entries  []imapEntry
}

var permanentFlags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag, `\*`}
var mailboxFlags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}

func (m *imapMailbox) Name() string { return m.name }

func (m *imapMailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{Attributes: m.attrs, Delimiter: folderDelimiter, Name: m.name}, nil
}

// BindResponseWriter is called by SELECT/EXAMINE (writer set) and when the mailbox is left.
func (m *imapMailbox) BindResponseWriter(w func(imap.WriterTo) error) {
	m.mu.Lock()
	m.write = w
	m.mu.Unlock()
	m.s.mu.Lock()
	if w != nil {
		m.s.selected = m
	} else if m.s.selected == m {
		m.s.selected = nil
	}
	m.s.mu.Unlock()
}

// selectedTwin returns the connection's selected mailbox if it's this folder but another object
// (APPEND / STATUS get their own objects): counts must come from the view the app has.
func (m *imapMailbox) selectedTwin() *imapMailbox {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if sel := m.s.selected; sel != nil && sel != m && sel.key == m.key {
		return sel
	}
	return nil
}

// loadLocked (re)reads the folder into the view. Callers hold m.mu.
func (m *imapMailbox) loadLocked() error {
	gen := m.s.a.Events.Gen(m.s.u.ID)
	es, validity, next, err := m.s.a.imapSync(m.s.u.ID, m.key)
	if err != nil {
		return err
	}
	m.entries, m.validity, m.uidNext, m.gen, m.loaded = es, validity, next, gen, true
	return nil
}

func (m *imapMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	if err := m.s.alive(); err != nil {
		return nil, err
	}
	src := m
	if twin := m.selectedTwin(); twin != nil {
		src = twin
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.loaded || src != m {
		if src == m || !src.loaded {
			if err := src.loadLocked(); err != nil {
				return nil, err
			}
		}
	}
	st := imap.NewMailboxStatus(m.name, items)
	st.Flags = mailboxFlags
	st.PermanentFlags = permanentFlags
	unseen := 0
	var firstUnseen uint32
	for i, e := range src.entries {
		if !e.fl.seen {
			unseen++
			if firstUnseen == 0 {
				firstUnseen = uint32(i + 1)
			}
		}
	}
	st.UnseenSeqNum = firstUnseen
	for _, it := range items {
		switch it {
		case imap.StatusMessages:
			st.Messages = uint32(len(src.entries))
		case imap.StatusRecent:
			st.Recent = 0
		case imap.StatusUidNext:
			next := src.uidNext
			if n := len(src.entries); n > 0 && src.entries[n-1].uid >= next {
				next = src.entries[n-1].uid + 1
			}
			st.UidNext = next
		case imap.StatusUidValidity:
			st.UidValidity = src.validity
		case imap.StatusUnseen:
			st.Unseen = uint32(unseen)
		}
	}
	return st, nil
}

func (m *imapMailbox) SetSubscribed(bool) error { return nil }

// Check is also a moment to report changes (RFC 3501 allows it).
func (m *imapMailbox) Check() error { return m.Poll() }

// ---------------------------------------------------------------- sequence sets

// setMatcher returns a membership test for set, with "*" meaning max (RFC 3501: "*" is the
// largest number in use, and n:* with n > max still contains max).
func setMatcher(set *imap.SeqSet, max uint32) func(uint32) bool {
	type rng struct{ a, b uint32 }
	var rs []rng
	if set != nil {
		for _, s := range set.Set {
			a, b := s.Start, s.Stop
			if a == 0 {
				a = max
			}
			if b == 0 {
				b = max
			}
			if a > b {
				a, b = b, a
			}
			rs = append(rs, rng{a, b})
		}
	}
	return func(q uint32) bool {
		for _, r := range rs {
			if q >= r.a && q <= r.b {
				return true
			}
		}
		return false
	}
}

// pick returns the indexes of the view's entries selected by a sequence or UID set.
func (m *imapMailbox) pick(uid bool, set *imap.SeqSet) []int {
	if len(m.entries) == 0 {
		return nil
	}
	var max uint32
	if uid {
		max = m.entries[len(m.entries)-1].uid
	} else {
		max = uint32(len(m.entries))
	}
	in := setMatcher(set, max)
	var out []int
	for i, e := range m.entries {
		q := uint32(i + 1)
		if uid {
			q = e.uid
		}
		if in(q) {
			out = append(out, i)
		}
	}
	return out
}

// ---------------------------------------------------------------- FETCH

func needsBody(items []imap.FetchItem) bool {
	for _, it := range items {
		switch it {
		case imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchUid:
		default:
			return true
		}
	}
	return false
}

func (m *imapMailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	if err := m.s.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			return err
		}
	}
	idx := m.pick(uid, seqset)
	if len(idx) == 0 {
		return nil
	}
	a := m.s.a
	userID := m.s.u.ID
	// Current flags from the database (another app or the web may have changed them).
	cur := m.currentFlags(idx)
	body := needsBody(items)
	wantFlags := false
	for _, it := range items {
		if it == imap.FetchFlags {
			wantFlags = true
		}
	}
	var markSeen []imapEntry
	for _, i := range idx {
		e := &m.entries[i]
		f, ok := cur[e.id]
		if !ok {
			continue // gone meanwhile; the next NOOP reports it
		}
		if wantFlags {
			e.fl = f // reported in this response, so the view may follow
		}
		var raw []byte
		if body {
			var err error
			if raw, err = a.imapRaw(userID, m.key, e.id); err != nil {
				continue
			}
		}
		msg := imap.NewMessage(uint32(i+1), items)
		setsSeen := false
		for _, it := range items {
			switch it {
			case imap.FetchEnvelope:
				hdr, _, _ := headerAndBody(raw)
				msg.Envelope, _ = backendutil.FetchEnvelope(hdr)
			case imap.FetchBody, imap.FetchBodyStructure:
				hdr, rd, _ := headerAndBody(raw)
				msg.BodyStructure, _ = backendutil.FetchBodyStructure(hdr, rd, it == imap.FetchBodyStructure)
			case imap.FetchFlags:
				msg.Flags = e.fl.list()
			case imap.FetchInternalDate:
				msg.InternalDate = time.UnixMilli(e.date)
			case imap.FetchRFC822Size:
				msg.Size = uint32(e.size)
				if raw != nil {
					msg.Size = uint32(len(raw))
				}
			case imap.FetchUid:
				msg.Uid = e.uid
			default:
				section, err := imap.ParseBodySectionName(it)
				if err != nil {
					break
				}
				hdr, rd, err := headerAndBody(raw)
				if err != nil {
					break
				}
				l, _ := backendutil.FetchBodySection(hdr, rd, section)
				if l == nil {
					l = bytes.NewReader(nil) // a missing part is sent as empty, not left out
				}
				msg.Body[section] = l
				if !section.Peek {
					setsSeen = true
				}
			}
		}
		if setsSeen && !f.seen && !m.readOnly() {
			f.seen = true
			e.fl = f
			markSeen = append(markSeen, *e)
			msg.Flags = e.fl.list() // RFC 3501: report the implicit \Seen
			if _, ok := msg.Items[imap.FetchFlags]; !ok {
				msg.Items[imap.FetchFlags] = nil
			}
		}
		ch <- msg
	}
	if len(markSeen) > 0 {
		// Our own change: the view already shows it, so the next poll finds nothing to report.
		a.imapSetFlags(userID, m.key, markSeen)
	}
	return nil
}

// readOnly: opened with EXAMINE. The server refuses STORE/EXPUNGE there; FETCH must not set
// \Seen either. Callers hold m.mu.
func (m *imapMailbox) readOnly() bool { return m.ro }

func (m *imapMailbox) SetReadOnly(ro bool) {
	m.mu.Lock()
	m.ro = ro
	m.mu.Unlock()
}

func headerAndBody(raw []byte) (textproto.Header, io.Reader, error) {
	r := bufio.NewReader(bytes.NewReader(raw))
	h, err := textproto.ReadHeader(r)
	return h, r, err
}

// currentFlags reads the current flags of the given view entries.
// Messages are looked up by their UID in this folder and must still be the message the view
// knows under it and still be in the folder: a message moved away, deleted, or deleted and its
// row number reused by new mail (SQLite reuses the highest id) is left out, and callers skip it.
func (m *imapMailbox) currentFlags(idx []int) map[int64]imapFlags {
	out := make(map[int64]imapFlags, len(idx))
	want := make(map[uint32]int64, len(idx))
	uids := make([]uint32, 0, len(idx))
	for _, i := range idx {
		e := m.entries[i]
		want[e.uid] = e.id
		uids = append(uids, e.uid)
	}
	a := m.s.a
	for start := 0; start < len(uids); start += 500 {
		chunk := uids[start:min(len(uids), start+500)]
		b, _ := json.Marshal(chunk)
		var q string
		var args []any
		if m.key == imapDraftsKey {
			q = `SELECT uid, id, seen, flagged, answered, deleted, keywords FROM imap_drafts WHERE user_id=? AND uid IN (SELECT value FROM json_each(?))`
			args = []any{m.s.u.ID, string(b)}
		} else {
			q = `SELECT u.uid, m.id, m.seen, m.flagged, m.answered, u.deleted, u.keywords FROM imap_uids u
				JOIN messages m ON m.id=u.msg_id AND m.folder=u.folder
				WHERE u.user_id=? AND u.folder=? AND u.uid IN (SELECT value FROM json_each(?))`
			args = []any{m.s.u.ID, m.key, string(b)}
		}
		rows, err := a.DB.Query(q, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var uid uint32
			var id int64
			var f imapFlags
			if rows.Scan(&uid, &id, &f.seen, &f.flagged, &f.answered, &f.deleted, &f.keywords) == nil && want[uid] == id {
				f.draft = m.key == imapDraftsKey
				out[id] = f
			}
		}
		rows.Close()
	}
	return out
}

// ---------------------------------------------------------------- SEARCH

func searchNeedsContent(c *imap.SearchCriteria) bool {
	if c == nil {
		return false
	}
	if !c.SentBefore.IsZero() || !c.SentSince.IsZero() || len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 {
		return true
	}
	for _, n := range c.Not {
		if searchNeedsContent(n) {
			return true
		}
	}
	for _, o := range c.Or {
		if searchNeedsContent(o[0]) || searchNeedsContent(o[1]) {
			return true
		}
	}
	return false
}

// normalizeCriteria replaces "*" in sequence/UID sets by the largest number in use, so
// SeqSet.Contains gives RFC 3501 answers (n:* with n beyond the end still matches the last).
func normalizeCriteria(c *imap.SearchCriteria, maxSeq, maxUID uint32) *imap.SearchCriteria {
	if c == nil {
		return nil
	}
	cp := *c
	fix := func(s *imap.SeqSet, max uint32) *imap.SeqSet {
		if s == nil {
			return nil
		}
		out := new(imap.SeqSet)
		for _, r := range s.Set {
			a, b := r.Start, r.Stop
			if a == 0 {
				a = max
			}
			if b == 0 {
				b = max
			}
			if a > b {
				a, b = b, a
			}
			if b > 0 {
				out.AddRange(max0(a), b)
			}
		}
		if len(out.Set) == 0 {
			out.AddNum(0xffffffff) // nothing in use: matches nothing real
		}
		return out
	}
	cp.SeqNum = fix(c.SeqNum, maxSeq)
	cp.Uid = fix(c.Uid, maxUID)
	cp.Not = nil
	for _, n := range c.Not {
		cp.Not = append(cp.Not, normalizeCriteria(n, maxSeq, maxUID))
	}
	cp.Or = nil
	for _, o := range c.Or {
		cp.Or = append(cp.Or, [2]*imap.SearchCriteria{normalizeCriteria(o[0], maxSeq, maxUID), normalizeCriteria(o[1], maxSeq, maxUID)})
	}
	return &cp
}

func max0(a uint32) uint32 {
	if a == 0 {
		return 1
	}
	return a
}

// matchMeta evaluates criteria that need no message content.
func matchMeta(c *imap.SearchCriteria, seq uint32, e *imapEntry) bool {
	if c.SeqNum != nil && !c.SeqNum.Contains(seq) {
		return false
	}
	if c.Uid != nil && !c.Uid.Contains(e.uid) {
		return false
	}
	if !c.Since.IsZero() || !c.Before.IsZero() {
		t := time.UnixMilli(e.date)
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if !c.Since.IsZero() && d.Before(c.Since) {
			return false
		}
		if !c.Before.IsZero() && !d.Before(c.Before) {
			return false
		}
	}
	if c.Larger > 0 && uint32(e.size) <= c.Larger {
		return false
	}
	if c.Smaller > 0 && uint32(e.size) >= c.Smaller {
		return false
	}
	if c.WithFlags != nil || c.WithoutFlags != nil {
		have := map[string]bool{}
		for _, f := range e.fl.list() {
			have[strings.ToLower(f)] = true
		}
		for _, f := range c.WithFlags {
			if !have[strings.ToLower(imap.CanonicalFlag(f))] {
				return false
			}
		}
		for _, f := range c.WithoutFlags {
			if have[strings.ToLower(imap.CanonicalFlag(f))] {
				return false
			}
		}
	}
	for _, n := range c.Not {
		if matchMeta(n, seq, e) {
			return false
		}
	}
	for _, o := range c.Or {
		if !matchMeta(o[0], seq, e) && !matchMeta(o[1], seq, e) {
			return false
		}
	}
	return true
}

func (m *imapMailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	if err := m.s.alive(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			return nil, err
		}
	}
	if len(m.entries) == 0 {
		return nil, nil
	}
	all := make([]int, len(m.entries))
	for i := range all {
		all[i] = i
	}
	cur := m.currentFlags(all)
	c := normalizeCriteria(criteria, uint32(len(m.entries)), m.entries[len(m.entries)-1].uid)
	content := searchNeedsContent(c)
	var out []uint32
	for i := range m.entries {
		e := m.entries[i] // a copy: the view's flags change only when the app is told
		f, ok := cur[e.id]
		if !ok {
			continue
		}
		e.fl = f
		seq := uint32(i + 1)
		var match bool
		if !content {
			match = matchMeta(c, seq, &e)
		} else {
			raw, err := m.s.a.imapRaw(m.s.u.ID, m.key, e.id)
			if err != nil {
				continue
			}
			ent, err := message.Read(bytes.NewReader(raw))
			if err != nil && ent == nil {
				continue
			}
			if !matchMeta(&imap.SearchCriteria{SeqNum: c.SeqNum, Uid: c.Uid, WithFlags: c.WithFlags, WithoutFlags: c.WithoutFlags}, seq, &e) {
				continue
			}
			// The search parser lowercases keywords (CanonicalFlag); compare in that form.
			fl := e.fl.list()
			for i := range fl {
				fl[i] = imap.CanonicalFlag(fl[i])
			}
			match, _ = backendutil.Match(ent, seq, e.uid, time.UnixMilli(e.date), fl, c)
		}
		if match {
			if uid {
				out = append(out, e.uid)
			} else {
				out = append(out, seq)
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- STORE

func (m *imapMailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	if err := m.s.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			return err
		}
	}
	idx := m.pick(uid, seqset)
	cur := m.currentFlags(idx)
	var changed []imapEntry
	for _, i := range idx {
		e := &m.entries[i]
		f, ok := cur[e.id]
		if !ok {
			continue
		}
		nf := applyFlags(f, op, flags)
		nf.draft = m.key == imapDraftsKey // Drafts keep \Draft; elsewhere it isn't stored
		e.fl = nf
		if nf != f {
			changed = append(changed, *e)
		}
	}
	return m.s.a.imapSetFlags(m.s.u.ID, m.key, changed)
}

// ---------------------------------------------------------------- COPY / MOVE

func (m *imapMailbox) CopyMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	if err := m.s.alive(); err != nil {
		return err
	}
	destKey, _, err := m.s.resolve(dest)
	if err != nil {
		return imapTryCreate(err)
	}
	m.mu.Lock()
	idx := m.pick(uid, seqset)
	es := make([]imapEntry, 0, len(idx))
	for _, i := range idx {
		es = append(es, m.entries[i])
	}
	cur := m.currentFlags(idx)
	m.mu.Unlock()
	if _, err := m.s.a.imapCopy(m.s.u.ID, m.key, destKey, es, cur); err != nil {
		return err
	}
	if destKey == m.key {
		m.Poll() // new messages here: EXISTS is allowed after COPY
	}
	return nil
}

// imapTryCreate turns "no such mailbox" into the response that makes apps offer to create it.
func imapTryCreate(err error) error {
	if errors.Is(err, backend.ErrNoSuchMailbox) {
		return imapNo("TRYCREATE", "No such folder")
	}
	return err
}

// imapNo is a NO reply with a response code ("NO [OVERQUOTA] Your mailbox is full").
func imapNo(code imap.StatusRespCode, info string) error {
	return &imap.ErrStatusResp{Resp: &imap.StatusResp{Type: imap.StatusRespNo, Code: code, Info: info}}
}

// imapCopy stores copies of es (from folder src) in dest. A copy is a new message; copies of
// the same message never count as duplicates of each other. It returns the source ids that
// were copied (messages that are gone or unreadable are skipped).
func (a *App) imapCopy(userID int64, src, dest string, es []imapEntry, cur map[int64]imapFlags) ([]int64, error) {
	var ids, copied []int64
	kws := map[int64]string{}
	defer func() {
		a.imapMapNow(userID, dest, ids, kws)
		a.Events.Publish(userID, Event{Type: "sync"})
	}()
	for _, e := range es {
		f, ok := cur[e.id]
		if !ok {
			continue
		}
		raw, err := a.imapRaw(userID, src, e.id)
		if err != nil {
			continue
		}
		if dest == imapDraftsKey {
			f.draft = true
			if err := a.imapSaveDraft(userID, raw, f, e.date); err != nil {
				return copied, imapStoreErr(err)
			}
			copied = append(copied, e.id)
			continue
		}
		id, err := a.Deliver(userID, raw, DeliverOpts{Folder: dest, Seen: f.seen, Flagged: f.flagged, Answered: f.answered, ReceivedAt: e.date,
			AllowDuplicate: true, EnforceQuota: true, Quiet: true})
		if err != nil {
			return copied, imapStoreErr(err)
		}
		ids = append(ids, id)
		kws[id] = f.keywords
		copied = append(copied, e.id)
	}
	return copied, nil
}

func imapStoreErr(err error) error {
	if errors.Is(err, ErrQuota) {
		return imapNo("OVERQUOTA", "Your mailbox is full")
	}
	return err
}

func (m *imapMailbox) MoveMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	if err := m.s.alive(); err != nil {
		return err
	}
	destKey, _, err := m.s.resolve(dest)
	if err != nil {
		return imapTryCreate(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readOnly() {
		return imapserver.ErrMailboxReadOnly // opened with EXAMINE: nothing may leave it
	}
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			return err
		}
	}
	idx := m.pick(uid, seqset)
	if len(idx) == 0 || destKey == m.key {
		return nil
	}
	a := m.s.a
	userID := m.s.u.ID
	cur := m.currentFlags(idx)
	// Only messages that are still what the view says they are (see currentFlags).
	live := idx[:0:0]
	for _, i := range idx {
		if _, ok := cur[m.entries[i].id]; ok {
			live = append(live, i)
		}
	}
	gone := map[int]bool{}
	if m.key == imapDraftsKey || destKey == imapDraftsKey {
		// Between Drafts and a regular folder: copy, then remove the originals that were copied.
		var es []imapEntry
		pos := map[int64]int{}
		for _, i := range live {
			es = append(es, m.entries[i])
			pos[m.entries[i].id] = i
		}
		copied, err := a.imapCopy(userID, m.key, destKey, es, cur)
		if err != nil {
			return err // nothing removed: at worst the copies that were made exist twice
		}
		if len(copied) > 0 {
			if m.key == imapDraftsKey {
				err = a.imapDeleteDrafts(userID, copied)
			} else {
				err = a.deleteMessages(`SELECT id, size, blob FROM messages WHERE user_id=? AND folder=? AND id IN (SELECT value FROM json_each(?))`, userID, m.key, jsonIDs(copied))
			}
			if err != nil {
				return err
			}
		}
		for _, id := range copied {
			gone[pos[id]] = true
		}
	} else {
		var ids []int64
		kws := map[int64]string{}
		pos := map[int64]int{}
		for _, i := range live {
			id := m.entries[i].id
			ids = append(ids, id)
			kws[id] = cur[id].keywords
			pos[id] = i
		}
		moved, err := a.imapMoveMessages(userID, ids, m.key, destKey, kws)
		if err != nil {
			return err
		}
		for _, id := range moved {
			gone[pos[id]] = true
		}
	}
	m.removeAndReport(gone)
	return nil
}

func jsonIDs(ids []int64) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

// removeAndReport drops view entries and sends EXPUNGE for each, highest number first.
// Callers hold m.mu.
func (m *imapMailbox) removeAndReport(gone map[int]bool) {
	if len(gone) == 0 {
		return
	}
	idx := make([]int, 0, len(gone))
	for i := range gone {
		idx = append(idx, i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idx)))
	if m.write != nil {
		ch := make(chan uint32, len(idx))
		for _, i := range idx {
			ch <- uint32(i + 1)
		}
		close(ch)
		m.write(&responses.Expunge{SeqNums: ch})
	}
	for _, i := range idx {
		m.entries = append(m.entries[:i], m.entries[i+1:]...)
	}
}

// ---------------------------------------------------------------- EXPUNGE

// Expunge removes messages marked \Deleted. Outside Trash and Spam they go to Trash (a mail app
// that deletes this way can't lose mail); in Trash, Spam and Drafts they're deleted for good.
// What is removed is decided from the flags right now. CLOSE calls Expunge (no reports: the
// folder is no longer selected); EXPUNGE calls ExpungeAndReport.
func (m *imapMailbox) Expunge() error { return m.expunge(false) }

// ExpungeAndReport expunges and sends the EXPUNGE responses for exactly what it removed.
func (m *imapMailbox) ExpungeAndReport() error { return m.expunge(true) }

func (m *imapMailbox) expunge(report bool) error {
	if err := m.s.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			return err
		}
	}
	a := m.s.a
	userID := m.s.u.ID
	all := make([]int, len(m.entries))
	for i := range all {
		all[i] = i
	}
	cur := m.currentFlags(all)
	var ids []int64
	gone := map[int]bool{}
	for i, e := range m.entries {
		if f, ok := cur[e.id]; ok && f.deleted {
			ids = append(ids, e.id)
			gone[i] = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// The change first; the view (and what the app is told) only follows when it worked.
	var err error
	switch m.key {
	case imapDraftsKey:
		err = a.imapDeleteDrafts(userID, ids)
	case "trash", "spam":
		err = a.deleteMessages(`SELECT id, size, blob FROM messages WHERE user_id=? AND folder=? AND id IN (SELECT value FROM json_each(?))`, userID, m.key, jsonIDs(ids))
	default:
		var moved []int64
		moved, err = a.imapMoveMessages(userID, ids, m.key, "trash", nil)
		if err == nil && len(moved) != len(ids) {
			ok := map[int64]bool{}
			for _, id := range moved {
				ok[id] = true
			}
			for i := range gone {
				if !ok[m.entries[i].id] {
					delete(gone, i) // changed meanwhile (moved elsewhere): the next poll reports it
				}
			}
		}
	}
	if err != nil {
		return err
	}
	if report {
		m.removeAndReport(gone)
	} else {
		keep := m.entries[:0]
		for i, e := range m.entries {
			if !gone[i] {
				keep = append(keep, e)
			}
		}
		m.entries = keep
	}
	return nil
}

// ---------------------------------------------------------------- APPEND

func (m *imapMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if err := m.s.alive(); err != nil {
		return err
	}
	a := m.s.a
	userID := m.s.u.ID
	limit := a.MaxMessageBytes()
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return backend.ErrTooBig
	}
	if len(raw) == 0 {
		return errors.New("Empty message")
	}
	fl := applyFlags(imapFlags{}, imap.SetFlags, flags)
	received := nowMs()
	if !date.IsZero() && date.Year() >= 1980 {
		received = date.UnixMilli()
	}
	if m.key == imapDraftsKey {
		fl.draft = true
		if err := a.imapSaveDraft(userID, raw, fl, received); err != nil {
			return imapStoreErr(err)
		}
	} else {
		meta := ParseMeta(raw)
		// Apps upload a copy of what they just sent through our SMTP, which already saved one
		// in Sent: keep just that one (matched by Message-ID, or by content when the app left
		// the Message-ID out and our SMTP added it).
		if m.key == "sent" && a.sentCopyOf(userID, raw, &meta, false) > 0 {
			m.afterAppend()
			return nil
		}
		id, err := a.Deliver(userID, raw, DeliverOpts{Folder: m.key, Meta: &meta, Seen: fl.seen || m.key == "sent", Flagged: fl.flagged, Answered: fl.answered,
			ReceivedAt: received, AllowDuplicate: true, EnforceQuota: true})
		if err != nil {
			return imapStoreErr(err)
		}
		kw := map[int64]string{id: fl.keywords}
		a.imapMapNow(userID, m.key, []int64{id}, kw)
		if fl.deleted {
			a.DB.Exec(`UPDATE imap_uids SET deleted=1 WHERE user_id=? AND folder=? AND msg_id=?`, userID, m.key, id)
		}
	}
	m.afterAppend()
	return nil
}

// afterAppend: if this folder is selected on the connection, tell it about the new message now
// (the server then sends EXISTS with the count from that same view).
func (m *imapMailbox) afterAppend() {
	if twin := m.selectedTwin(); twin != nil {
		twin.Poll()
	}
}

// ---------------------------------------------------------------- NOOP / IDLE

// Poll compares the view with the folder and tells the app what changed: EXPUNGE for messages
// that left (highest first), EXISTS for new ones, FETCH FLAGS for changed flags.
func (m *imapMailbox) Poll() error {
	if err := m.s.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded || m.write == nil {
		return nil
	}
	a := m.s.a
	gen := a.Events.Gen(m.s.u.ID)
	if gen == m.gen {
		return nil
	}
	fresh, _, next, err := a.imapSync(m.s.u.ID, m.key)
	if err != nil {
		return err
	}
	m.gen = gen
	m.uidNext = next
	byUID := make(map[uint32]imapEntry, len(fresh))
	for _, e := range fresh {
		byUID[e.uid] = e
	}
	gone := map[int]bool{}
	for i, e := range m.entries {
		if _, ok := byUID[e.uid]; !ok {
			gone[i] = true
		}
	}
	m.removeAndReport(gone)
	var changed []*imap.Message
	known := make(map[uint32]bool, len(m.entries))
	for i := range m.entries {
		e := &m.entries[i]
		known[e.uid] = true
		if f := byUID[e.uid]; f.fl != e.fl {
			e.fl = f.fl
			msg := imap.NewMessage(uint32(i+1), []imap.FetchItem{imap.FetchUid, imap.FetchFlags})
			msg.Uid, msg.Flags = e.uid, e.fl.list()
			changed = append(changed, msg)
		}
	}
	var last uint32
	if n := len(m.entries); n > 0 {
		last = m.entries[n-1].uid
	}
	added := 0
	for _, e := range fresh {
		if !known[e.uid] && e.uid > last {
			m.entries = append(m.entries, e)
			added++
		}
	}
	if added > 0 {
		st := imap.NewMailboxStatus(m.name, []imap.StatusItem{imap.StatusMessages})
		st.Messages = uint32(len(m.entries))
		m.write(&responses.Select{Mailbox: st})
	}
	if len(changed) > 0 {
		ch := make(chan *imap.Message, len(changed))
		for _, msg := range changed {
			ch <- msg
		}
		close(ch)
		m.write(&responses.Fetch{Messages: ch})
	}
	return nil
}

// IdleNotify wakes IDLE as soon as anything changes in this user's mailbox.
func (m *imapMailbox) IdleNotify() (<-chan struct{}, func()) {
	a := m.s.a
	userID := m.s.u.ID
	events := a.Events.Subscribe(userID)
	wake := make(chan struct{}, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-events:
				// Let a burst of changes (a filter run, a big move) settle into one poll.
				t := time.NewTimer(150 * time.Millisecond)
			drain:
				for {
					select {
					case <-events:
					case <-t.C:
						break drain
					case <-stop:
						t.Stop()
						return
					}
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	return wake, func() {
		once.Do(func() {
			close(stop)
			a.Events.Unsubscribe(userID, events)
		})
	}
}
