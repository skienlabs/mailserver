package core

import (
	"bytes"
	"time"
)

// Mail apps send through our SMTP (which saves a copy in Sent) and also upload their own copy
// to Sent over IMAP, in either order. Only one copy should be kept.

const sentCopyWindow = 30 * time.Minute

// messageBody returns what follows the header block, with line endings normalised, so a
// message compares equal to itself after headers were added in front of it (Received,
// Message-ID, Date) or it travelled with LF instead of CRLF line endings.
func messageBody(raw []byte) []byte {
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	j := bytes.Index(raw, []byte("\n\n"))
	switch {
	case i >= 0 && (j < 0 || i < j):
		raw = raw[i+4:]
	case j >= 0:
		raw = raw[j+2:]
	default:
		return nil
	}
	return bytes.TrimRight(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), "\n \t")
}

// sentCopyOf finds the copy of this message already in the user's Sent folder: the same
// Message-ID, or, when the copy being compared has no Message-ID, one saved recently with the
// same subject and the same body. wantNoID limits the fallback to copies without a Message-ID
// (the app's upload, when our SMTP had to add one).
func (a *App) sentCopyOf(userID int64, raw []byte, meta *Meta, wantNoID bool) int64 {
	var id int64
	if meta.MessageID != "" {
		a.DB.QueryRow(`SELECT id FROM messages WHERE user_id=? AND message_id=? AND (folder='sent' OR prev_folder='sent') LIMIT 1`,
			userID, meta.MessageID).Scan(&id)
		if id > 0 || !wantNoID {
			return id
		}
	}
	body := messageBody(raw)
	if body == nil {
		return 0
	}
	q := `SELECT id, blob FROM messages WHERE user_id=? AND folder='sent' AND received_at>? AND subject=?`
	if wantNoID {
		q += ` AND message_id=''`
	}
	rows, err := a.DB.Query(q+` ORDER BY id DESC LIMIT 20`, userID, nowMs()-sentCopyWindow.Milliseconds(), meta.Subject)
	if err != nil {
		return 0
	}
	type cand struct {
		id   int64
		blob string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.blob) == nil {
			cs = append(cs, c)
		}
	}
	rows.Close()
	for _, c := range cs {
		if b, err := a.readBlob(c.blob); err == nil && bytes.Equal(messageBody(b), body) {
			return c.id
		}
	}
	return 0
}
