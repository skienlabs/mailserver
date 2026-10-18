package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	imap "github.com/emersion/go-imap"
)

// Mail apps (IMAP) number messages per folder with UIDs that only ever grow. Messages here move
// between folders by changing one column, so each folder keeps its own UID map (imap_uids):
// a message that arrives in a folder gets the folder's next UID, one that leaves loses its UID.
// The map is brought up to date lazily, when a mail app looks and something has changed.
// Drafts saved by mail apps live in their own table (imap_drafts) and never show in the web app.

const imapDraftsKey = "drafts"

// imapFlags are a message's flags as a mail app sees them.
type imapFlags struct {
	seen, flagged, answered, deleted, draft bool
	keywords                                string // space-separated
}

const maxKeywords = 20

func (f imapFlags) list() []string {
	out := make([]string, 0, 5)
	if f.seen {
		out = append(out, imap.SeenFlag)
	}
	if f.answered {
		out = append(out, imap.AnsweredFlag)
	}
	if f.flagged {
		out = append(out, imap.FlaggedFlag)
	}
	if f.deleted {
		out = append(out, imap.DeletedFlag)
	}
	if f.draft {
		out = append(out, imap.DraftFlag)
	}
	if f.keywords != "" {
		out = append(out, strings.Fields(f.keywords)...)
	}
	return out
}

// validKeyword: an IMAP atom without wildcards or the system-flag backslash.
func validKeyword(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`(){%*"\]`, c) >= 0 {
			return false
		}
	}
	return true
}

// applyFlags changes f as STORE asks (add, remove or replace).
func applyFlags(f imapFlags, op imap.FlagsOp, flags []string) imapFlags {
	set := func(v bool, name string) bool {
		switch name {
		case imap.SeenFlag:
			f.seen = v
		case imap.FlaggedFlag:
			f.flagged = v
		case imap.AnsweredFlag:
			f.answered = v
		case imap.DeletedFlag:
			f.deleted = v
		case imap.DraftFlag:
			f.draft = v
		default:
			return false
		}
		return true
	}
	kw := map[string]bool{}
	var order []string
	if op != imap.SetFlags {
		for _, k := range strings.Fields(f.keywords) {
			kw[strings.ToLower(k)] = true
			order = append(order, k)
		}
	} else {
		f.seen, f.flagged, f.answered, f.deleted, f.draft = false, false, false, false, false
	}
	for _, name := range flags {
		name = imap.CanonicalFlagKeepCase(name)
		if name == imap.RecentFlag {
			continue
		}
		on := op != imap.RemoveFlags
		if set(on, name) || strings.HasPrefix(name, `\`) {
			continue // a system flag, or an unknown \flag we don't store
		}
		if !validKeyword(name) {
			continue
		}
		low := strings.ToLower(name)
		if on && !kw[low] && len(order) < maxKeywords {
			kw[low] = true
			order = append(order, name)
		} else if !on && kw[low] {
			delete(kw, low)
			keep := order[:0]
			for _, k := range order {
				if strings.ToLower(k) != low {
					keep = append(keep, k)
				}
			}
			order = keep
		}
	}
	f.keywords = strings.Join(order, " ")
	return f
}

// imapEntry is one message as a mail app sees it in a folder.
type imapEntry struct {
	uid  uint32
	id   int64 // messages.id, or imap_drafts.id in Drafts
	fl   imapFlags
	size int64
	date int64 // ms (INTERNALDATE)
}

// imapFolderState returns the folder's UIDVALIDITY and next UID, creating the record if needed.
func imapFolderState(tx *sql.Tx, userID int64, key string) (validity, next uint32, err error) {
	err = tx.QueryRow(`SELECT uidvalidity, uidnext FROM imap_folders WHERE user_id=? AND folder=?`, userID, key).Scan(&validity, &next)
	if errors.Is(err, sql.ErrNoRows) {
		validity = uint32(time.Now().Unix() & 0x7fffffff)
		if validity == 0 {
			validity = 1
		}
		next = 1
		_, err = tx.Exec(`INSERT INTO imap_folders(user_id, folder, uidvalidity, uidnext) VALUES(?,?,?,?)`, userID, key, validity, next)
	}
	return
}

// imapSync brings the folder's UID map up to date (when something changed since last time) and
// returns the folder's messages in UID order.
func (a *App) imapSync(userID int64, key string) ([]imapEntry, uint32, uint32, error) {
	gen := a.Events.Gen(userID)
	mk := fmt.Sprintf("%d|%s", userID, key)
	tx, err := a.DB.Begin()
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()
	validity, next, err := imapFolderState(tx, userID, key)
	if err != nil {
		return nil, 0, 0, err
	}
	if key != imapDraftsKey {
		if last, ok := a.imapMaint.Load(mk); !ok || last.(uint64) != gen {
			// Messages that left the folder lose their UIDs; new arrivals get the next ones,
			// oldest first.
			if _, err := tx.Exec(`DELETE FROM imap_uids WHERE user_id=? AND folder=? AND msg_id NOT IN (SELECT id FROM messages WHERE user_id=? AND folder=?)`,
				userID, key, userID, key); err != nil {
				return nil, 0, 0, err
			}
			res, err := tx.Exec(`INSERT INTO imap_uids(user_id, folder, uid, msg_id)
				SELECT ?, ?, ? + ROW_NUMBER() OVER (ORDER BY received_at, id) - 1, id FROM messages m
				WHERE m.user_id=? AND m.folder=? AND NOT EXISTS (SELECT 1 FROM imap_uids u WHERE u.user_id=? AND u.folder=? AND u.msg_id=m.id)`,
				userID, key, next, userID, key, userID, key)
			if err != nil {
				return nil, 0, 0, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				next += uint32(n)
				if _, err := tx.Exec(`UPDATE imap_folders SET uidnext=? WHERE user_id=? AND folder=?`, next, userID, key); err != nil {
					return nil, 0, 0, err
				}
			}
		}
	}
	var rows *sql.Rows
	if key == imapDraftsKey {
		rows, err = tx.Query(`SELECT uid, id, seen, flagged, answered, deleted, keywords, size, received_at FROM imap_drafts WHERE user_id=? ORDER BY uid`, userID)
	} else {
		rows, err = tx.Query(`SELECT u.uid, u.msg_id, m.seen, m.flagged, m.answered, u.deleted, u.keywords, m.size, m.received_at
			FROM imap_uids u JOIN messages m ON m.id=u.msg_id AND m.folder=u.folder WHERE u.user_id=? AND u.folder=? ORDER BY u.uid`, userID, key)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	var out []imapEntry
	for rows.Next() {
		var e imapEntry
		if err := rows.Scan(&e.uid, &e.id, &e.fl.seen, &e.fl.flagged, &e.fl.answered, &e.fl.deleted, &e.fl.keywords, &e.size, &e.date); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		e.fl.draft = key == imapDraftsKey
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, 0, err
	}
	if key != imapDraftsKey {
		a.imapMaint.Store(mk, gen)
	}
	return out, validity, next, nil
}

// imapMapNow gives messages that just arrived in a folder (from a mail app) their UIDs right
// away, so the app's own view and the map agree.
func (a *App) imapMapNow(userID int64, key string, ids []int64, keywords map[int64]string) error {
	if len(ids) == 0 || key == imapDraftsKey {
		return nil
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, next, err := imapFolderState(tx, userID, key)
	if err != nil {
		return err
	}
	sorted := append([]int64{}, ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, id := range sorted {
		res, err := tx.Exec(`INSERT OR IGNORE INTO imap_uids(user_id, folder, uid, msg_id, keywords) VALUES(?,?,?,?,?)`, userID, key, next, id, keywords[id])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			next++
		}
	}
	if _, err := tx.Exec(`UPDATE imap_folders SET uidnext=? WHERE user_id=? AND folder=?`, next, userID, key); err != nil {
		return err
	}
	return tx.Commit()
}

// imapSetFlags stores new flags for messages of one folder.
func (a *App) imapSetFlags(userID int64, key string, es []imapEntry) error {
	if len(es) == 0 {
		return nil
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range es {
		if key == imapDraftsKey {
			if _, err := tx.Exec(`UPDATE imap_drafts SET seen=?, flagged=?, answered=?, deleted=?, keywords=? WHERE id=? AND user_id=?`,
				e.fl.seen, e.fl.flagged, e.fl.answered, e.fl.deleted, e.fl.keywords, e.id, userID); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE messages SET seen=?, flagged=?, answered=? WHERE id=? AND user_id=?`, e.fl.seen, e.fl.flagged, e.fl.answered, e.id, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE imap_uids SET deleted=?, keywords=? WHERE user_id=? AND folder=? AND uid=?`, e.fl.deleted, e.fl.keywords, userID, key, e.uid); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.Events.Publish(userID, Event{Type: "sync"})
	return nil
}

// imapMoveMessages moves messages between two regular folders (keeping the web app's Trash and
// "was sent" bookkeeping) and gives them UIDs in the destination. Returns the moved ids.
func (a *App) imapMoveMessages(userID int64, ids []int64, from, to string, keywords map[int64]string) ([]int64, error) {
	if len(ids) == 0 || from == to {
		return nil, nil
	}
	b, _ := json.Marshal(ids)
	rows, err := a.DB.Query(`UPDATE messages SET
			prev_folder = CASE WHEN ?1 IN ('trash','spam') THEN folder WHEN ?1 = 'sent' THEN '' WHEN folder='sent' OR prev_folder='sent' THEN 'sent' ELSE '' END,
			trashed_at = CASE WHEN ?1 IN ('trash','spam') THEN ?2 ELSE 0 END,
			folder = ?1
		WHERE user_id=?3 AND folder=?4 AND id IN (SELECT value FROM json_each(?5)) RETURNING id`, to, nowMs(), userID, from, string(b))
	if err != nil {
		return nil, err
	}
	var moved []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			moved = append(moved, id)
		}
	}
	rows.Close()
	if len(moved) > 0 {
		mb, _ := json.Marshal(moved)
		a.DB.Exec(`DELETE FROM imap_uids WHERE user_id=? AND folder=? AND msg_id IN (SELECT value FROM json_each(?))`, userID, from, string(mb))
		if err := a.imapMapNow(userID, to, moved, keywords); err != nil {
			return moved, err
		}
		a.Events.Publish(userID, Event{Type: "sync"})
	}
	return moved, nil
}

// imapRaw returns the stored message for an entry of folder key.
func (a *App) imapRaw(userID int64, key string, id int64) ([]byte, error) {
	var blob string
	var err error
	if key == imapDraftsKey {
		err = a.DB.QueryRow(`SELECT blob FROM imap_drafts WHERE id=? AND user_id=?`, id, userID).Scan(&blob)
	} else {
		err = a.DB.QueryRow(`SELECT blob FROM messages WHERE id=? AND user_id=?`, id, userID).Scan(&blob)
	}
	if err != nil {
		return nil, err
	}
	return a.readBlob(blob)
}

// imapSaveDraft stores a draft saved by a mail app and returns its UID.
func (a *App) imapSaveDraft(userID int64, raw []byte, fl imapFlags, received int64) error {
	size := int64(len(raw))
	rel := filepath.Join(fmt.Sprint(userID), time.Now().Format("200601"), RandomHex(12)+".eml")
	if err := writeFileSync(a.blobPath(rel), raw); err != nil {
		return err
	}
	err := func() error {
		tx, err := a.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.Exec(`UPDATE users SET used_bytes = used_bytes + ? WHERE id=? AND (quota_bytes <= 0 OR used_bytes + ? <= quota_bytes)`, size, userID, size)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrQuota
		}
		_, next, err := imapFolderState(tx, userID, imapDraftsKey)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO imap_drafts(user_id, uid, blob, size, received_at, seen, flagged, answered, deleted, keywords) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			userID, next, rel, size, received, fl.seen, fl.flagged, fl.answered, fl.deleted, fl.keywords); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE imap_folders SET uidnext=? WHERE user_id=? AND folder=?`, next+1, userID, imapDraftsKey); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		os.Remove(a.blobPath(rel))
		return err
	}
	a.Events.Publish(userID, Event{Type: "sync"})
	return nil
}

// imapDeleteDrafts permanently removes mail-app drafts.
func (a *App) imapDeleteDrafts(userID int64, ids []int64) error {
	var blobs []string
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var blob string
		var size int64
		if err := tx.QueryRow(`DELETE FROM imap_drafts WHERE id=? AND user_id=? RETURNING blob, size`, id, userID).Scan(&blob, &size); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET used_bytes = MAX(0, used_bytes - ?) WHERE id=?`, size, userID); err != nil {
			return err
		}
		blobs = append(blobs, blob)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, b := range blobs {
		os.Remove(a.blobPath(b))
	}
	a.Events.Publish(userID, Event{Type: "sync"})
	return nil
}
