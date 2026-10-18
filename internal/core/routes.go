package core

import "net/http"

func (a *App) routes(m *http.ServeMux, version string) {
	m.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": Version, "build": version, "product": ProductName})
	})
	// setup & auth
	m.HandleFunc("GET /api/setup", a.handleSetupStatus)
	m.HandleFunc("POST /api/setup", a.handleSetup)
	m.HandleFunc("POST /api/auth/login", a.handleLogin)
	m.HandleFunc("POST /api/auth/logout", a.authed(a.handleLogout))
	m.HandleFunc("GET /api/me", a.authed(a.handleMe))
	m.HandleFunc("GET /api/events", a.authed(a.handleEvents))

	// mail
	m.HandleFunc("GET /api/folders", a.authed(a.handleFolders))
	m.HandleFunc("GET /api/stats", a.authed(a.handleStats))
	m.HandleFunc("GET /api/threads", a.authed(a.handleThreads))
	m.HandleFunc("GET /api/threads/{id}", a.authed(a.handleThread))
	m.HandleFunc("POST /api/threads/action", a.authed(a.handleThreadAction))
	m.HandleFunc("POST /api/messages/{id}/flag", a.authed(a.handleFlag))
	m.HandleFunc("GET /api/messages/{id}/raw", a.authed(a.handleRaw))
	m.HandleFunc("GET /api/messages/{id}/attachments/{idx}", a.authed(a.handleAttachment))
	m.HandleFunc("GET /api/messages/{id}/cid/{cid}", a.authed(a.handleCID))
	m.HandleFunc("POST /api/messages/{id}/forward-files", a.authed(a.handleForwardFiles))

	// compose
	m.HandleFunc("POST /api/uploads", a.authed(a.handleUpload))
	m.HandleFunc("GET /api/uploads/{id}", a.authed(a.handleGetUpload))
	m.HandleFunc("DELETE /api/uploads/{id}", a.authed(a.handleDeleteUpload))
	m.HandleFunc("GET /api/drafts", a.authed(a.handleDrafts))
	m.HandleFunc("GET /api/drafts/{id}", a.authed(a.handleGetDraft))
	m.HandleFunc("PUT /api/drafts", a.authed(a.handleSaveDraft))
	m.HandleFunc("DELETE /api/drafts/{id}", a.authed(a.handleDeleteDraft))
	m.HandleFunc("POST /api/send", a.authed(a.handleSend))
	m.HandleFunc("GET /api/contacts", a.authed(a.handleContacts))
	m.HandleFunc("GET /api/directory", a.authed(a.handleDirectory))

	// profile & security
	m.HandleFunc("PUT /api/profile", a.authed(a.handleUpdateProfile))
	m.HandleFunc("POST /api/profile/avatar", a.authed(a.handleUploadAvatar))
	m.HandleFunc("DELETE /api/profile/avatar", a.authed(a.handleDeleteAvatar))
	m.HandleFunc("GET /api/avatar/{id}", a.authed(a.handleAvatar))
	m.HandleFunc("POST /api/profile/password", a.authed(a.handleChangePassword))
	m.HandleFunc("GET /api/sessions", a.authed(a.handleSessions))
	m.HandleFunc("DELETE /api/sessions/{id}", a.authed(a.handleRevokeSession))
	m.HandleFunc("POST /api/sessions/revoke-others", a.authed(a.handleRevokeOthers))
	m.HandleFunc("GET /api/vacation", a.authed(a.handleGetVacation))
	m.HandleFunc("PUT /api/vacation", a.authed(a.handleSetVacation))

	// importing mail from other providers
	m.HandleFunc("POST /api/import/imap/folders", a.authed(a.handleImportIMAPFolders))
	m.HandleFunc("POST /api/import/imap", a.authed(a.handleImportIMAPStart))
	m.HandleFunc("POST /api/import/uploads", a.authed(a.handleImportUploadBegin))
	m.HandleFunc("PUT /api/import/uploads/{id}", a.authed(a.handleImportUploadChunk))
	m.HandleFunc("DELETE /api/import/uploads/{id}", a.authed(a.handleImportUploadDelete))
	m.HandleFunc("POST /api/import/file", a.authed(a.handleImportFileStart))
	m.HandleFunc("GET /api/import/jobs", a.authed(a.handleImportJobs))
	m.HandleFunc("POST /api/import/jobs/{id}/cancel", a.authed(a.handleImportCancel))

	// administration
	m.HandleFunc("GET /api/admin/overview", a.admin(a.handleAdminOverview))
	m.HandleFunc("GET /api/admin/users", a.admin(a.handleAdminUsers))
	m.HandleFunc("POST /api/admin/users", a.admin(a.handleAdminCreateUser))
	m.HandleFunc("PUT /api/admin/users/{id}", a.admin(a.handleAdminUpdateUser))
	m.HandleFunc("POST /api/admin/users/{id}/password", a.admin(a.handleAdminResetPassword))
	m.HandleFunc("POST /api/admin/users/{id}/avatar", a.admin(a.handleAdminUserAvatar))
	m.HandleFunc("DELETE /api/admin/users/{id}", a.admin(a.handleAdminDeleteUser))
	m.HandleFunc("GET /api/admin/aliases", a.admin(a.handleAdminAliases))
	m.HandleFunc("POST /api/admin/aliases", a.admin(a.handleAdminSaveAlias))
	m.HandleFunc("PUT /api/admin/aliases/{id}", a.admin(a.handleAdminSaveAlias))
	m.HandleFunc("DELETE /api/admin/aliases/{id}", a.admin(a.handleAdminDeleteAlias))
	m.HandleFunc("GET /api/admin/domains", a.admin(a.handleAdminDomains))
	m.HandleFunc("POST /api/admin/domains", a.admin(a.handleAdminCreateDomain))
	m.HandleFunc("GET /api/admin/domains/{id}/check", a.admin(a.handleAdminCheckDomain))
	m.HandleFunc("DELETE /api/admin/domains/{id}", a.admin(a.handleAdminDeleteDomain))
	m.HandleFunc("GET /api/admin/queue", a.admin(a.handleAdminQueue))
	m.HandleFunc("POST /api/admin/queue/{id}/retry", a.admin(a.handleAdminQueueRetry))
	m.HandleFunc("POST /api/admin/queue/{id}/cancel", a.admin(a.handleAdminQueueCancel))
	m.HandleFunc("POST /api/admin/queue/retry-all", a.admin(a.handleAdminQueueRetryAll))
	m.HandleFunc("GET /api/admin/queue/messages/{id}", a.admin(a.handleAdminQueueMessage))
	m.HandleFunc("GET /api/admin/log", a.admin(a.handleAdminLog))
	m.HandleFunc("GET /api/admin/settings", a.admin(a.handleAdminGetSettings))
	m.HandleFunc("PUT /api/admin/settings", a.admin(a.handleAdminSetSettings))
	m.HandleFunc("POST /api/admin/test-port25", a.admin(a.handleAdminTestPort25))

	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpError(w, http.StatusNotFound, "Not found")
	})
}
