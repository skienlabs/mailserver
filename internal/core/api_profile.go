package core

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) handleUpdateProfile(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name      string `json:"name"`
		Title     string `json:"title"`
		Phone     string `json:"phone"`
		Signature string `json:"signature"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name = sanitizeHeaderValue(strings.TrimSpace(in.Name))
	if in.Name == "" {
		httpError(w, http.StatusBadRequest, "Enter your name")
		return
	}
	if len(in.Title) > 120 || len(in.Phone) > 60 || len(in.Signature) > 20000 {
		httpError(w, http.StatusBadRequest, "That's too long")
		return
	}
	sig := SanitizeHTML(in.Signature, 0)
	if _, err := a.DB.Exec(`UPDATE users SET name=?, title=?, phone=?, signature=? WHERE id=?`, in.Name, strings.TrimSpace(in.Title), strings.TrimSpace(in.Phone), sig, u.ID); err != nil {
		a.internalError(w, err)
		return
	}
	nu, _ := a.UserByID(u.ID)
	writeJSON(w, http.StatusOK, nu)
}

func (a *App) handleUploadAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	a.saveAvatar(w, r, u.ID)
}

func (a *App) saveAvatar(w http.ResponseWriter, r *http.Request, userID int64) {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusBadRequest, "Choose an image up to 2 MB")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		httpError(w, http.StatusBadRequest, "Choose an image up to 2 MB")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") || cfg.Width > 4096 || cfg.Height > 4096 {
		httpError(w, http.StatusBadRequest, "Use a JPG or PNG image")
		return
	}
	if err := writeFileSync(filepath.Join(a.Cfg.DataDir, "avatars", fmt.Sprint(userID)), data); err != nil {
		a.internalError(w, err)
		return
	}
	a.DB.Exec(`UPDATE users SET avatar_ver=? WHERE id=?`, nowMs(), userID)
	nu, _ := a.UserByID(userID)
	writeJSON(w, http.StatusOK, nu)
}

func (a *App) handleDeleteAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	os.Remove(filepath.Join(a.Cfg.DataDir, "avatars", fmt.Sprint(u.ID)))
	a.DB.Exec(`UPDATE users SET avatar_ver=0 WHERE id=?`, u.ID)
	nu, _ := a.UserByID(u.ID)
	writeJSON(w, http.StatusOK, nu)
}

func (a *App) handleAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathInt(r, "id")
	data, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, "avatars", fmt.Sprint(id)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") || strings.Contains(ct, "svg") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(data)
}

func (a *App) handleGetVacation(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": u.VacationEnabled, "subject": u.VacationSubject, "body": u.VacationBody,
		"start": u.VacationStart, "end": u.VacationEnd,
	})
}

func (a *App) handleSetVacation(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Enabled bool   `json:"enabled"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Start   int64  `json:"start"`
		End     int64  `json:"end"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Body) > 20000 || len(in.Subject) > 300 {
		httpError(w, http.StatusBadRequest, "That's too long")
		return
	}
	if in.Enabled && strings.TrimSpace(in.Body) == "" {
		httpError(w, http.StatusBadRequest, "Write a message for the auto-reply")
		return
	}
	if in.End > 0 && in.Start > 0 && in.End < in.Start {
		httpError(w, http.StatusBadRequest, "The end date is before the start date")
		return
	}
	if _, err := a.DB.Exec(`UPDATE users SET vacation_enabled=?, vacation_subject=?, vacation_body=?, vacation_start=?, vacation_end=? WHERE id=?`,
		in.Enabled, sanitizeHeaderValue(in.Subject), in.Body, in.Start, in.End, u.ID); err != nil {
		a.internalError(w, err)
		return
	}
	if in.Enabled {
		a.DB.Exec(`DELETE FROM vacation_log WHERE user_id=?`, u.ID) // new period, reply to everyone again
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
