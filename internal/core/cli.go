package core

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ResetPasswordCLI sets a temporary password for a user and signs them out.
// Used to recover a locked-out administrator from the server shell.
func ResetPasswordCLI(cfg *Config, email string) (string, error) {
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	email = strings.ToLower(strings.TrimSpace(email))
	var id int64
	if err := db.QueryRow(`SELECT id FROM users WHERE email=?`, email).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no user %s", email)
		}
		return "", err
	}
	pw := GeneratePassword()
	hash, err := HashPassword(pw)
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`UPDATE users SET password_hash=?, must_change_password=1, status='active', password_changed_at=? WHERE id=?`, hash, nowMs(), id); err != nil {
		return "", err
	}
	db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	return pw, nil
}

// BackupCLI writes a transactionally consistent copy of the database.
func BackupCLI(cfg *Config, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`VACUUM INTO ?`, dest)
	return err
}
