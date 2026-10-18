package core

import (
	"database/sql"
	"errors"
	"net/http"
)

// ---------------------------------------------------------------- custom folders

func (a *App) handleListUserFolders(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.ListFolders(u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleCreateUserFolder(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	f, err := a.CreateFolder(u.ID, in.Name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *App) handleRenameUserFolder(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	f, err := a.RenameFolder(u.ID, folderKey(pathInt(r, "id")), in.Name)
	if errors.Is(err, errNoFolder) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (a *App) handleDeleteUserFolder(w http.ResponseWriter, r *http.Request, u *User) {
	err := a.DeleteFolder(u.ID, folderKey(pathInt(r, "id")))
	if errors.Is(err, errNoFolder) {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "counts": a.FolderCounts(u.ID)})
}

// ---------------------------------------------------------------- filters

func (a *App) handleListFilters(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.ListFilters(u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleCreateFilter(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Filter
		Apply bool `json:"apply"` // also run it on mail already here
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	f, err := a.CreateFilter(u.ID, in.Filter)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	applied := 0
	if in.Apply {
		if applied, err = a.ApplyFilter(u.ID, f.ID); err != nil {
			a.Log.Error("apply filter", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"filter": f, "applied": applied})
}

func (a *App) handleUpdateFilter(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Filter
		Apply bool `json:"apply"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.ID = pathInt(r, "id")
	f, err := a.UpdateFilter(u.ID, in.Filter)
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, http.StatusNotFound, "That filter no longer exists")
		return
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	applied := 0
	if in.Apply {
		if applied, err = a.ApplyFilter(u.ID, f.ID); err != nil {
			a.Log.Error("apply filter", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"filter": f, "applied": applied})
}

func (a *App) handleDeleteFilter(w http.ResponseWriter, r *http.Request, u *User) {
	a.DeleteFilter(u.ID, pathInt(r, "id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleReorderFilters(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.ReorderFilters(u.ID, in.IDs); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- blocked / allowed senders

func (a *App) handleListSenders(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.ListSenders(u.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleSetSender(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Pattern      string `json:"pattern"`
		Kind         string `json:"kind"`
		MoveExisting bool   `json:"move_existing"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	e, err := a.SetSender(u.ID, in.Pattern, in.Kind)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	moved := 0
	if in.MoveExisting && in.Kind == "block" {
		if moved, err = a.MoveSenderToSpam(u.ID, e.Pattern); err != nil {
			a.Log.Error("move blocked sender", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e, "moved": moved, "counts": a.FolderCounts(u.ID)})
}

func (a *App) handleRemoveSender(w http.ResponseWriter, r *http.Request, u *User) {
	a.RemoveSender(u.ID, r.URL.Query().Get("pattern"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSenderStatus tells the message view whether a sender is blocked or allowed.
func (a *App) handleSenderStatus(w http.ResponseWriter, r *http.Request, u *User) {
	kind, pattern := a.senderMatch(u.ID, r.URL.Query().Get("email"))
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "pattern": pattern})
}
