package core

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied in order; each entry is one migration. Never edit an
// existing entry once released — append a new one instead.
var migrations = []string{
	`
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE domains (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE COLLATE NOCASE,
  dkim_selector TEXT NOT NULL,
  dkim_key      TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE users (
  id                   INTEGER PRIMARY KEY,
  domain_id            INTEGER NOT NULL REFERENCES domains(id),
  local                TEXT NOT NULL COLLATE NOCASE,
  email                TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name                 TEXT NOT NULL,
  password_hash        TEXT NOT NULL,
  role                 TEXT NOT NULL DEFAULT 'user',
  status               TEXT NOT NULL DEFAULT 'active',
  quota_bytes          INTEGER NOT NULL DEFAULT 0,
  used_bytes           INTEGER NOT NULL DEFAULT 0,
  title                TEXT NOT NULL DEFAULT '',
  phone                TEXT NOT NULL DEFAULT '',
  avatar_ver           INTEGER NOT NULL DEFAULT 0,
  signature            TEXT NOT NULL DEFAULT '',
  must_change_password INTEGER NOT NULL DEFAULT 0,
  vacation_enabled     INTEGER NOT NULL DEFAULT 0,
  vacation_subject     TEXT NOT NULL DEFAULT '',
  vacation_body        TEXT NOT NULL DEFAULT '',
  vacation_start       INTEGER NOT NULL DEFAULT 0,
  vacation_end         INTEGER NOT NULL DEFAULT 0,
  created_at           INTEGER NOT NULL,
  last_login_at        INTEGER NOT NULL DEFAULT 0,
  password_changed_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE aliases (
  id          INTEGER PRIMARY KEY,
  domain_id   INTEGER NOT NULL REFERENCES domains(id),
  address     TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name        TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);

CREATE TABLE alias_members (
  alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  can_send INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (alias_id, user_id)
);

CREATE TABLE sessions (
  id         INTEGER PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ttl        INTEGER NOT NULL,
  ip         TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE messages (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  folder       TEXT NOT NULL,
  thread_id    INTEGER NOT NULL DEFAULT 0,
  message_id   TEXT NOT NULL DEFAULT '',
  in_reply_to  TEXT NOT NULL DEFAULT '',
  refs         TEXT NOT NULL DEFAULT '',
  from_addr    TEXT NOT NULL DEFAULT '',
  from_name    TEXT NOT NULL DEFAULT '',
  to_json      TEXT NOT NULL DEFAULT '[]',
  cc_json      TEXT NOT NULL DEFAULT '[]',
  bcc_json     TEXT NOT NULL DEFAULT '[]',
  reply_to     TEXT NOT NULL DEFAULT '',
  subject      TEXT NOT NULL DEFAULT '',
  snippet      TEXT NOT NULL DEFAULT '',
  date         INTEGER NOT NULL,
  received_at  INTEGER NOT NULL,
  size         INTEGER NOT NULL,
  has_attach   INTEGER NOT NULL DEFAULT 0,
  seen         INTEGER NOT NULL DEFAULT 0,
  flagged      INTEGER NOT NULL DEFAULT 0,
  answered     INTEGER NOT NULL DEFAULT 0,
  spam_score   REAL NOT NULL DEFAULT 0,
  auth_summary TEXT NOT NULL DEFAULT '',
  prev_folder  TEXT NOT NULL DEFAULT '',
  blob         TEXT NOT NULL
);
CREATE INDEX messages_folder ON messages(user_id, folder, received_at DESC);
CREATE INDEX messages_thread ON messages(user_id, thread_id);
CREATE INDEX messages_msgid  ON messages(user_id, message_id);
CREATE INDEX messages_flag   ON messages(user_id, flagged) WHERE flagged = 1;

CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, addrs, body,
  content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2'
);

CREATE TABLE drafts (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  data       TEXT NOT NULL,
  subject    TEXT NOT NULL DEFAULT '',
  to_text    TEXT NOT NULL DEFAULT '',
  snippet    TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE INDEX drafts_user ON drafts(user_id, updated_at DESC);

CREATE TABLE uploads (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  ctype      TEXT NOT NULL,
  size       INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE contacts (
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  address   TEXT NOT NULL COLLATE NOCASE,
  name      TEXT NOT NULL DEFAULT '',
  uses      INTEGER NOT NULL DEFAULT 0,
  last_used INTEGER NOT NULL,
  PRIMARY KEY (user_id, address)
);

CREATE TABLE queue (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL DEFAULT 0,
  sender      TEXT NOT NULL,
  subject     TEXT NOT NULL DEFAULT '',
  message_id  TEXT NOT NULL DEFAULT '',
  blob        TEXT NOT NULL,
  size        INTEGER NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE TABLE queue_rcpts (
  id           INTEGER PRIMARY KEY,
  queue_id     INTEGER NOT NULL REFERENCES queue(id) ON DELETE CASCADE,
  rcpt         TEXT NOT NULL,
  domain       TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'pending',
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL,
  last_error   TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL
);
CREATE INDEX queue_rcpts_due ON queue_rcpts(status, next_attempt);
CREATE INDEX queue_rcpts_q   ON queue_rcpts(queue_id);

CREATE TABLE mail_log (
  id        INTEGER PRIMARY KEY,
  ts        INTEGER NOT NULL,
  direction TEXT NOT NULL,
  sender    TEXT NOT NULL DEFAULT '',
  rcpt      TEXT NOT NULL DEFAULT '',
  subject   TEXT NOT NULL DEFAULT '',
  status    TEXT NOT NULL,
  detail    TEXT NOT NULL DEFAULT '',
  remote    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX mail_log_ts ON mail_log(ts DESC);

CREATE TABLE vacation_log (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  sender  TEXT NOT NULL COLLATE NOCASE,
  sent_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, sender)
);
`,
	// 2: Trash/Spam auto-delete counts from when a message was moved there, not when it arrived.
	`
ALTER TABLE messages ADD COLUMN trashed_at INTEGER NOT NULL DEFAULT 0;
UPDATE messages SET trashed_at = received_at WHERE folder IN ('trash','spam');
CREATE INDEX messages_trashed ON messages(trashed_at) WHERE trashed_at > 0;
`,
}

// OpenDB opens (and migrates) the SQLite database in dataDir.
func OpenDB(dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, "skienmail.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(10 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }
