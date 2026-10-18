package core

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

func (a *App) janitor(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.cleanup()
		t.Reset(time.Hour)
	}
}

func (a *App) cleanup() {
	now := time.Now()
	days := a.Settings.Int(SetTrashDays)
	if days > 0 {
		cut := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		if err := a.deleteMessages(`SELECT id, size, blob FROM messages WHERE folder IN ('trash','spam') AND trashed_at > 0 AND trashed_at < ?`, cut); err != nil {
			a.Log.Error("janitor purge", "err", err)
		}
	}
	a.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.UnixMilli())
	a.cleanImportUploads()
	a.DB.Exec(`DELETE FROM imports WHERE finished_at > 0 AND finished_at < ?`, now.Add(-90*24*time.Hour).UnixMilli())
	a.DB.Exec(`DELETE FROM mail_log WHERE ts < ?`, now.Add(-30*24*time.Hour).UnixMilli())
	a.DB.Exec(`DELETE FROM vacation_log WHERE sent_at < ?`, now.Add(-30*24*time.Hour).UnixMilli())

	// Uploads not attached to any draft after 2 days
	rows, err := a.DB.Query(`SELECT u.id FROM uploads u WHERE u.created_at < ? AND NOT EXISTS
		(SELECT 1 FROM drafts d WHERE d.user_id = u.user_id AND instr(d.data, u.id) > 0)`, now.Add(-48*time.Hour).UnixMilli())
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			a.DB.Exec(`DELETE FROM uploads WHERE id=?`, id)
			os.Remove(filepath.Join(a.Cfg.DataDir, "uploads", id))
		}
	}

	// Finished queue entries: drop the stored copy right away, the record after 7 days.
	qrows, err := a.DB.Query(`SELECT q.id, q.blob, q.created_at FROM queue q WHERE NOT EXISTS
		(SELECT 1 FROM queue_rcpts r WHERE r.queue_id = q.id AND r.status IN ('pending','deferred'))`)
	if err == nil {
		type qe struct {
			id      int64
			blob    string
			created int64
		}
		var list []qe
		for qrows.Next() {
			var e qe
			qrows.Scan(&e.id, &e.blob, &e.created)
			list = append(list, e)
		}
		qrows.Close()
		for _, e := range list {
			if e.blob != "" {
				os.Remove(filepath.Join(a.Cfg.DataDir, "queue", e.blob))
				a.DB.Exec(`UPDATE queue SET blob='' WHERE id=?`, e.id)
			}
			if now.Sub(time.UnixMilli(e.created)) > 7*24*time.Hour {
				a.DB.Exec(`DELETE FROM queue WHERE id=?`, e.id)
			}
		}
	}
	a.DB.Exec(`PRAGMA optimize`)
}

func (a *App) removeUserUploads(userID int64) {
	rows, err := a.DB.Query(`SELECT id FROM uploads WHERE user_id=?`, userID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		os.Remove(filepath.Join(a.Cfg.DataDir, "uploads", id))
	}
	a.DB.Exec(`DELETE FROM uploads WHERE user_id=?`, userID)
}
