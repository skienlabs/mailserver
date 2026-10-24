package core

import (
	"archive/zip"
	"compress/flate"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Mailbox export: a .zip with one folder per mail folder and one .eml file per message,
// streamed straight to the browser (nothing is staged on disk). The layout is the one the
// importer understands, so an export can be imported here or elsewhere.

const exportAppDrafts = "imap-drafts" // export key for drafts saved by mail apps (imap_drafts)

var exportState = struct {
	sync.Mutex
	users  map[int64]bool
	active int
}{users: map[int64]bool{}}

const exportMaxConcurrent = 2

var errExportBusy = errors.New("busy")

func exportBegin(userID int64) error {
	exportState.Lock()
	defer exportState.Unlock()
	if exportState.users[userID] || exportState.active >= exportMaxConcurrent {
		return errExportBusy
	}
	exportState.users[userID] = true
	exportState.active++
	return nil
}

func exportEnd(userID int64) {
	exportState.Lock()
	delete(exportState.users, userID)
	exportState.active--
	exportState.Unlock()
}

// exportFolder is one folder offered for export.
type exportFolder struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Messages int    `json:"messages"`
	Bytes    int64  `json:"bytes"`
	Default  bool   `json:"default"` // checked by default (everything but Spam and Trash)
}

func (a *App) exportFolders(userID int64) ([]exportFolder, error) {
	counts := map[string][2]int64{}
	rows, err := a.DB.Query(`SELECT folder, COUNT(*), COALESCE(SUM(size),0) FROM messages WHERE user_id=? GROUP BY folder`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n, b int64
		if rows.Scan(&k, &n, &b) == nil {
			counts[k] = [2]int64{n, b}
		}
	}
	rows.Close()
	out := []exportFolder{}
	for _, k := range []string{"inbox", "sent", "archive"} {
		c := counts[k]
		out = append(out, exportFolder{Key: k, Name: a.folderLabel(userID, k), Messages: int(c[0]), Bytes: c[1], Default: true})
	}
	custom, err := a.ListFolders(userID)
	if err != nil {
		return nil, err
	}
	for _, f := range custom {
		c := counts[f.Key]
		out = append(out, exportFolder{Key: f.Key, Name: f.Name, Messages: int(c[0]), Bytes: c[1], Default: true})
	}
	// Drafts saved by mail apps (and messages moved into Drafts there) live apart.
	var dn, db int64
	a.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM imap_drafts WHERE user_id=?`, userID).Scan(&dn, &db)
	if dn > 0 {
		out = append(out, exportFolder{Key: exportAppDrafts, Name: "Drafts (mail apps)", Messages: int(dn), Bytes: db, Default: true})
	}
	for _, k := range []string{"spam", "trash"} {
		c := counts[k]
		out = append(out, exportFolder{Key: k, Name: a.folderLabel(userID, k), Messages: int(c[0]), Bytes: c[1]})
	}
	return out, nil
}

// GET /api/export/folders
func (a *App) handleExportFolders(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.exportFolders(u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// exportTop is the folder that holds everything in an export: "Softop Mails export".
func (a *App) exportTop() string { return zipSafe(a.AppName() + " export") }

// zipSafe makes one path level safe on every operating system (Windows is the strictest).
func zipSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) || r == 0x7f {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(s, "_"))
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	if s == "" {
		s = "_"
	}
	switch strings.ToUpper(strings.SplitN(s, ".", 2)[0]) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return truncateRunes(s, 80)
}

// GET /api/export?folders=inbox,sent,f:3 — streams the .zip.
func (a *App) handleExport(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.exportFolders(u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	want := map[string]bool{}
	for _, k := range strings.Split(r.URL.Query().Get("folders"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			want[k] = true
		}
	}
	var chosen []exportFolder
	for _, f := range list {
		if (len(want) == 0 && f.Default) || want[f.Key] {
			chosen = append(chosen, f)
		}
	}
	if len(chosen) == 0 {
		httpError(w, http.StatusBadRequest, "Choose at least one folder")
		return
	}
	if r.URL.Query().Get("check") == "1" { // the app asks first, so errors show as messages, not as downloaded files
		exportState.Lock()
		busy := exportState.users[u.ID] || exportState.active >= exportMaxConcurrent
		exportState.Unlock()
		if busy {
			httpError(w, http.StatusTooManyRequests, "An export is already being prepared. Wait for it to finish, then try again.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := exportBegin(u.ID); err != nil {
		httpError(w, http.StatusTooManyRequests, "An export is already being prepared. Wait for it to finish, then try again.")
		return
	}
	defer exportEnd(u.ID)

	name := fmt.Sprintf("%s mail %s.zip", strings.SplitN(u.Email, "@", 2)[0], time.Now().Format("2006-01-02"))
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", contentDisposition("attachment", name))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	rc := http.NewResponseController(w)

	top := a.exportTop()
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, flate.BestSpeed) })
	var index strings.Builder
	cw := csv.NewWriter(&index)
	cw.Write([]string{"folder", "file", "received", "from", "subject", "read", "starred"})
	missing, written := 0, 0
	used := map[string]bool{}
	ctx := r.Context()
	for _, f := range chosen {
		dir := top + "/"
		for _, part := range strings.Split(f.Name, "/") {
			dir += zipSafe(part) + "/"
		}
		// Two folders can clean up to the same name ("a:b" and "a_b"): keep them apart.
		for base, i := dir, 2; used[strings.ToLower(dir)]; i++ {
			dir = strings.TrimSuffix(base, "/") + fmt.Sprintf(" (%d)/", i)
		}
		used[strings.ToLower(dir)] = true
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: dir, Method: zip.Store, Modified: time.Now()}); err != nil {
			return // the client went away
		}
		var lastID int64
		for {
			// Pages by id, so no query stays open while the (slow) download runs.
			q := `SELECT id, received_at, from_addr, subject, seen, flagged, blob FROM messages
				WHERE user_id=? AND folder=? AND id>? ORDER BY id LIMIT 500`
			args := []any{u.ID, f.Key, lastID}
			if f.Key == exportAppDrafts {
				q = `SELECT id, received_at, '', '', seen, flagged, blob FROM imap_drafts WHERE user_id=? AND id>? ORDER BY id LIMIT 500`
				args = []any{u.ID, lastID}
			}
			rows, err := a.DB.Query(q, args...)
			if err != nil {
				a.Log.Error("export", "err", err)
				return
			}
			type item struct {
				id, received  int64
				from, subject string
				seen, flagged bool
				blob          string
			}
			var page []item
			for rows.Next() {
				var it item
				if err := rows.Scan(&it.id, &it.received, &it.from, &it.subject, &it.seen, &it.flagged, &it.blob); err == nil {
					page = append(page, it)
				}
			}
			rows.Close()
			if len(page) == 0 {
				break
			}
			for _, it := range page {
				lastID = it.id
				if ctx.Err() != nil {
					return
				}
				raw, err := a.readBlob(it.blob)
				if err != nil {
					missing++
					continue
				}
				if f.Key == exportAppDrafts {
					meta := ParseMeta(raw)
					it.from, it.subject = meta.From.Email, meta.Subject
				}
				when := time.UnixMilli(it.received)
				subj := strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) {
						return ' '
					}
					return r
				}, it.subject)
				file := fmt.Sprintf("%s %s-%d.eml", when.Format("2006-01-02 1504"), zipSafe(truncateRunes(strings.TrimSpace(subj), 50)), it.id)
				rc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
				fw, err := zw.CreateHeader(&zip.FileHeader{Name: dir + file, Method: zip.Deflate, Modified: when})
				if err != nil {
					return
				}
				if _, err := fw.Write(raw); err != nil {
					return
				}
				written++
				cw.Write([]string{f.Name, strings.TrimPrefix(dir+file, top+"/"), when.Format(time.RFC3339), it.from, it.subject, yesNo(it.seen), yesNo(it.flagged)})
			}
		}
	}
	cw.Flush()
	if fw, err := zw.CreateHeader(&zip.FileHeader{Name: top + "/messages.csv", Method: zip.Deflate, Modified: time.Now()}); err == nil {
		io.WriteString(fw, index.String())
	}
	readme := fmt.Sprintf("Mail exported from %s for %s on %s.\r\n\r\n"+
		"Each folder holds one .eml file per message (the original message, exactly as received or sent).\r\n"+
		"Open .eml files with Outlook, Apple Mail or Thunderbird, or import this .zip into another mail service.\r\n"+
		"To bring it back into %s: Settings > Import & export > Import from a file.\r\n"+
		"messages.csv lists every message with its folder, date, sender, subject and read/starred status.\r\n\r\n"+
		"Messages: %d\r\n", a.OrgName(), u.Email, time.Now().Format("2 January 2006 15:04"), a.AppName(), written)
	if missing > 0 {
		readme += fmt.Sprintf("Messages whose file could not be read on the server (left out): %d\r\n", missing)
	}
	if fw, err := zw.CreateHeader(&zip.FileHeader{Name: top + "/README.txt", Method: zip.Deflate, Modified: time.Now()}); err == nil {
		io.WriteString(fw, readme)
	}
	if err := zw.Close(); err != nil {
		return
	}
	a.Log.Info("mailbox exported", "user", u.Email, "messages", written, "folders", len(chosen), "unreadable", missing)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
