package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"
)

var reUploadID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// uploadIdleTimeout is how long a chunk upload may go without receiving any data.
var uploadIdleTimeout = 60 * time.Second

// idleDeadlineReader keeps pushing the connection's read deadline forward while data flows.
type idleDeadlineReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

func (d *idleDeadlineReader) extend() {
	d.last = time.Now()
	d.rc.SetReadDeadline(d.last.Add(d.idle))
}

func (d *idleDeadlineReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if n > 0 && time.Since(d.last) > time.Second { // no need to touch it on every read
		d.extend()
	}
	return n, err
}

// POST /api/import/imap/folders — sign in to the other account and list its folders.
func (a *App) handleImportIMAPFolders(w http.ResponseWriter, r *http.Request, u *User) {
	var in IMAPSource
	if !decodeJSON(w, r, &in) {
		return
	}
	if !a.limiter.Allow(fmt.Sprintf("imapconnect:%d", u.ID), 30, time.Hour) {
		httpError(w, http.StatusTooManyRequests, "Too many connection attempts. Try again in a while.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	folders, err := a.IMAPFolders(ctx, in)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if folders == nil {
		folders = []IMAPFolder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// POST /api/import/imap — start importing the chosen folders.
func (a *App) handleImportIMAPStart(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		IMAPSource
		Folders []IMAPFolder `json:"folders"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !a.limiter.Allow(fmt.Sprintf("imapconnect:%d", u.ID), 30, time.Hour) {
		httpError(w, http.StatusTooManyRequests, "Too many connection attempts. Try again in a while.")
		return
	}
	job, err := a.ImportIMAP(u, in.IMAPSource, in.Folders)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// POST /api/import/uploads — announce a file; returns an id for its chunks.
func (a *App) handleImportUploadBegin(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id, err := a.BeginImportUpload(u.ID, in.Name, in.Size)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "chunk": importChunkMax})
}

// PUT /api/import/uploads/{id}?offset=N — one chunk of the file (raw bytes).
func (a *App) handleImportUploadChunk(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("id")
	if !reUploadID.MatchString(id) {
		httpError(w, http.StatusNotFound, "Upload not found")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		httpError(w, http.StatusBadRequest, "Missing offset")
		return
	}
	// A dead connection mustn't hold the upload "busy" for long, but a slow one that keeps
	// sending is fine: the deadline moves forward whenever data arrives.
	body := &idleDeadlineReader{r: http.MaxBytesReader(w, r.Body, importChunkMax+1), rc: http.NewResponseController(w), idle: uploadIdleTimeout}
	body.extend()
	got, err := a.AppendImportUpload(u.ID, id, offset, body)
	switch {
	case errors.Is(err, os.ErrNotExist):
		httpError(w, http.StatusNotFound, "Upload not found. Start the upload again.")
	case errors.Is(err, errUploadOffset):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset", "received": got})
	case errors.Is(err, errUploadBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "busy", "received": got})
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"received": got})
	}
}

// DELETE /api/import/uploads/{id}
func (a *App) handleImportUploadDelete(w http.ResponseWriter, r *http.Request, u *User) {
	if id := r.PathValue("id"); reUploadID.MatchString(id) {
		a.DiscardImportUpload(u.ID, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/import/file — import an uploaded file.
func (a *App) handleImportFileStart(w http.ResponseWriter, r *http.Request, u *User) {
	var in FileImportOpts
	if !decodeJSON(w, r, &in) {
		return
	}
	if !reUploadID.MatchString(in.UploadID) {
		httpError(w, http.StatusBadRequest, "The uploaded file wasn't found. Upload it again.")
		return
	}
	job, err := a.ImportFile(u, in)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// GET /api/import/jobs
func (a *App) handleImportJobs(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": a.ListImports(u.ID)})
}

// POST /api/import/jobs/{id}/cancel
func (a *App) handleImportCancel(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.CancelImport(u.ID, pathInt(r, "id")) {
		httpError(w, http.StatusNotFound, "That import isn't running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
