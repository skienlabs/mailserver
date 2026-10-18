package core

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Custom folders. A message in a custom folder has folder = "f:<id>"; the folder's name (with
// "/" between levels, e.g. "Clients/Acme") lives in user_folders. Parents are created
// automatically, renaming a folder renames everything under it, and deleting one moves its
// mail to Archive — nothing is ever lost by folder operations.

const (
	maxUserFolders  = 1000
	maxFolderName   = 200 // bytes, whole path
	maxFolderDepth  = 8
	folderDelimiter = "/"
)

var reFolderKey = regexp.MustCompile(`^f:[1-9][0-9]{0,17}$`)

// isCustomFolder reports whether key has the form of a custom folder key ("f:12").
func isCustomFolder(key string) bool { return reFolderKey.MatchString(key) }

func folderKey(id int64) string { return "f:" + strconv.FormatInt(id, 10) }

// systemFolderNames are reserved: a custom folder can't be called like a built-in one.
var systemFolderNames = map[string]string{
	"inbox": "inbox", "sent": "sent", "archive": "archive", "spam": "spam", "trash": "trash",
	"drafts": "drafts", "starred": "starred", "all mail": "all", "junk": "spam",
}

// UserFolder is a custom folder as shown in the app.
type UserFolder struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`   // full path
	Leaf   string `json:"leaf"`   // last level
	Depth  int    `json:"depth"`  // 0 = top level
	Unread int    `json:"unread"` // unread conversations
	Total  int    `json:"total"`  // messages
}

var (
	errFolderName   = errors.New("Folder names can't be empty, start or end with \"/\", or contain \\ % * or control characters")
	errFolderTaken  = errors.New("A folder with that name already exists")
	errFolderSystem = errors.New("That name is reserved for a built-in folder")
	errFolderCount  = fmt.Errorf("You can have at most %d folders", maxUserFolders)
	errNoFolder     = errors.New("That folder doesn't exist")
)

// cleanFolderName normalises a folder path: trims each level, collapses spaces, and checks
// the characters. Returns the cleaned name or an error.
func cleanFolderName(name string) (string, error) {
	name = strings.ToValidUTF8(name, "")
	parts := strings.Split(name, folderDelimiter)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" || p == "." || p == ".." {
			return "", errFolderName
		}
		for _, r := range p {
			if unicode.IsControl(r) || r == '\\' || r == '%' || r == '*' || r == '"' || r == utf8.RuneError {
				return "", errFolderName
			}
		}
		out = append(out, p)
	}
	if len(out) == 0 || len(out) > maxFolderDepth {
		return "", errFolderName
	}
	clean := strings.Join(out, folderDelimiter)
	if len(clean) > maxFolderName {
		return "", fmt.Errorf("Folder names can be at most %d characters", maxFolderName)
	}
	if _, reserved := systemFolderNames[strings.ToLower(out[0])]; reserved {
		return "", errFolderSystem // also no folders inside built-in ones ("Spam/x")
	}
	if strings.EqualFold(out[0], "INBOX") {
		return "", errFolderSystem // IMAP treats INBOX/… specially
	}
	return clean, nil
}

// ListFolders returns the user's custom folders sorted as a tree, with counts.
func (a *App) ListFolders(userID int64) ([]UserFolder, error) {
	rows, err := a.DB.Query(`SELECT id, name FROM user_folders WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	var out []UserFolder
	for rows.Next() {
		var f UserFolder
		if err := rows.Scan(&f.ID, &f.Name); err != nil {
			rows.Close()
			return nil, err
		}
		f.Key = folderKey(f.ID)
		parts := strings.Split(f.Name, folderDelimiter)
		f.Leaf, f.Depth = parts[len(parts)-1], len(parts)-1
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		counts := map[string][2]int{}
		crow, err := a.DB.Query(`SELECT folder, COUNT(*), COUNT(DISTINCT CASE WHEN seen=0 THEN thread_id END) FROM messages
			WHERE user_id=? AND folder >= 'f:' AND folder < 'f;' GROUP BY folder`, userID)
		if err == nil {
			for crow.Next() {
				var k string
				var total, unread int
				if crow.Scan(&k, &total, &unread) == nil {
					counts[k] = [2]int{total, unread}
				}
			}
			crow.Close()
		}
		for i := range out {
			c := counts[out[i].Key]
			out[i].Total, out[i].Unread = c[0], c[1]
		}
	}
	sortFolders(out)
	if out == nil {
		out = []UserFolder{}
	}
	return out, nil
}

// sortFolders orders folders as a tree: parents before children, siblings by name.
func sortFolders(fs []UserFolder) {
	key := func(n string) string { return strings.ToLower(strings.ReplaceAll(n, folderDelimiter, "\x00")) }
	sort.SliceStable(fs, func(i, j int) bool { return key(fs[i].Name) < key(fs[j].Name) })
}

// folderByKey returns the user's folder for a key ("f:12"), or errNoFolder.
func (a *App) folderByKey(userID int64, key string) (UserFolder, error) {
	var f UserFolder
	if !isCustomFolder(key) {
		return f, errNoFolder
	}
	id, _ := strconv.ParseInt(key[2:], 10, 64)
	err := a.DB.QueryRow(`SELECT id, name FROM user_folders WHERE id=? AND user_id=?`, id, userID).Scan(&f.ID, &f.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return f, errNoFolder
	}
	f.Key = folderKey(f.ID)
	return f, err
}

// folderByName finds a custom folder by its full path (case-insensitive).
func (a *App) folderByName(userID int64, name string) (UserFolder, error) {
	var f UserFolder
	err := a.DB.QueryRow(`SELECT id, name FROM user_folders WHERE user_id=? AND name=? COLLATE NOCASE`, userID, name).Scan(&f.ID, &f.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return f, errNoFolder
	}
	f.Key = folderKey(f.ID)
	return f, err
}

// CreateFolder makes a folder (and any missing parents). Creating one that exists is an error.
func (a *App) CreateFolder(userID int64, name string) (UserFolder, error) {
	clean, err := cleanFolderName(name)
	if err != nil {
		return UserFolder{}, err
	}
	if _, err := a.folderByName(userID, clean); err == nil {
		return UserFolder{}, errFolderTaken
	}
	f, err := a.ensureFolder(userID, clean)
	if err == nil {
		a.Events.Publish(userID, Event{Type: "folders"})
	}
	return f, err
}

// ensureFolder returns the folder with this (already cleaned) path, creating it and its
// parents if needed.
func (a *App) ensureFolder(userID int64, clean string) (UserFolder, error) {
	parts := strings.Split(clean, folderDelimiter)
	tx, err := a.DB.Begin()
	if err != nil {
		return UserFolder{}, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_folders WHERE user_id=?`, userID).Scan(&n); err != nil {
		return UserFolder{}, err
	}
	var f UserFolder
	now := nowMs()
	for i := range parts {
		path := strings.Join(parts[:i+1], folderDelimiter)
		err := tx.QueryRow(`SELECT id, name FROM user_folders WHERE user_id=? AND name=? COLLATE NOCASE`, userID, path).Scan(&f.ID, &f.Name)
		if errors.Is(err, sql.ErrNoRows) {
			if n >= maxUserFolders {
				return UserFolder{}, errFolderCount
			}
			res, err := tx.Exec(`INSERT INTO user_folders(user_id, name, created_at) VALUES(?,?,?)`, userID, path, now)
			if err != nil {
				return UserFolder{}, err
			}
			f.ID, _ = res.LastInsertId()
			f.Name = path
			n++
		} else if err != nil {
			return UserFolder{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return UserFolder{}, err
	}
	f.Key = folderKey(f.ID)
	ps := strings.Split(f.Name, folderDelimiter)
	f.Leaf, f.Depth = ps[len(ps)-1], len(ps)-1
	return f, nil
}

// RenameFolder renames (or moves) a folder; folders under it move along.
func (a *App) RenameFolder(userID int64, key, newName string) (UserFolder, error) {
	f, err := a.folderByKey(userID, key)
	if err != nil {
		return UserFolder{}, err
	}
	clean, err := cleanFolderName(newName)
	if err != nil {
		return UserFolder{}, err
	}
	if clean == f.Name {
		return f, nil
	}
	lowOld, lowNew := strings.ToLower(f.Name), strings.ToLower(clean)
	if strings.HasPrefix(lowNew+folderDelimiter, lowOld+folderDelimiter) && lowNew != lowOld {
		return UserFolder{}, errors.New("A folder can't be moved inside itself")
	}
	if other, err := a.folderByName(userID, clean); err == nil && other.ID != f.ID {
		return UserFolder{}, errFolderTaken
	}
	// Parents of the new location must exist.
	if i := strings.LastIndex(clean, folderDelimiter); i > 0 {
		if _, err := a.ensureFolder(userID, clean[:i]); err != nil {
			return UserFolder{}, err
		}
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return UserFolder{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, name FROM user_folders WHERE user_id=? AND (id=? OR name LIKE ? ESCAPE '\')`, userID, f.ID, likeEscape(f.Name)+folderDelimiter+"%")
	if err != nil {
		return UserFolder{}, err
	}
	type ren struct {
		id   int64
		name string
	}
	var list []ren
	for rows.Next() {
		var r ren
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			return UserFolder{}, err
		}
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		name := clean + r.name[len(f.Name):]
		if len(name) > maxFolderName || strings.Count(name, folderDelimiter)+1 > maxFolderDepth {
			return UserFolder{}, errors.New("The new name makes a folder inside it too long or too deep")
		}
		if _, err := tx.Exec(`UPDATE user_folders SET name=? WHERE id=?`, name, r.id); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return UserFolder{}, errFolderTaken
			}
			return UserFolder{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return UserFolder{}, err
	}
	a.Events.Publish(userID, Event{Type: "folders"})
	return a.folderByKey(userID, key)
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// DeleteFolder removes a folder and the folders under it. Their mail moves to Archive and
// filters that pointed at them stop moving mail (they keep their other actions).
func (a *App) DeleteFolder(userID int64, key string) error {
	f, err := a.folderByKey(userID, key)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM user_folders WHERE user_id=? AND (id=? OR name LIKE ? ESCAPE '\')`, userID, f.ID, likeEscape(f.Name)+folderDelimiter+"%")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		k := folderKey(id)
		if _, err := tx.Exec(`UPDATE messages SET folder='archive' WHERE user_id=? AND folder=?`, userID, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE messages SET prev_folder='' WHERE user_id=? AND prev_folder=?`, userID, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE filters SET action_folder='' WHERE user_id=? AND action_folder=?`, userID, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM imap_folders WHERE user_id=? AND folder=?`, userID, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM imap_uids WHERE user_id=? AND folder=?`, userID, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM user_folders WHERE id=?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.Events.Publish(userID, Event{Type: "folders"})
	a.Events.Publish(userID, Event{Type: "sync"})
	return nil
}

// validMoveTarget reports whether messages may be moved to target with the "move" action.
// Sent, Spam and Trash have their own actions (with their own bookkeeping).
func (a *App) validMoveTarget(userID int64, target string) bool {
	switch target {
	case "inbox", "archive":
		return true
	}
	_, err := a.folderByKey(userID, target)
	return err == nil
}

// folderLabel returns a folder's display name for messages and logs.
func (a *App) folderLabel(userID int64, key string) string {
	switch key {
	case "inbox":
		return "Inbox"
	case "sent":
		return "Sent"
	case "archive":
		return "Archive"
	case "spam":
		return "Spam"
	case "trash":
		return "Trash"
	}
	if f, err := a.folderByKey(userID, key); err == nil {
		return f.Name
	}
	return key
}
