// Package backend defines an IMAP server backend interface.
package backend

import (
	"errors"

	"github.com/emersion/go-imap"
)

// ErrInvalidCredentials is returned by Backend.Login when a username or a
// password is incorrect.
var ErrInvalidCredentials = errors.New("Invalid credentials")

// ErrSessionEnded is returned by a backend when a signed-in session may no longer be used
// (password changed, account disabled). Skien Mails patch: IDLE then says BYE and disconnects.
var ErrSessionEnded = errors.New("[UNAVAILABLE] This session has ended. Sign in again.")

// Backend is an IMAP server backend. A backend operation always deals with
// users.
type Backend interface {
	// Login authenticates a user. If the username or the password is incorrect,
	// it returns ErrInvalidCredentials.
	Login(connInfo *imap.ConnInfo, username, password string) (User, error)
}
