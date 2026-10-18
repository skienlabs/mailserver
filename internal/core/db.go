package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// 3: performance — covering indexes for the conversation list, unread counts and threading.
	`
DROP INDEX messages_folder;
CREATE INDEX messages_list   ON messages(user_id, folder, received_at, thread_id);
CREATE INDEX messages_recent ON messages(user_id, received_at, folder, thread_id);
CREATE INDEX messages_tcount ON messages(user_id, folder, thread_id);
CREATE INDEX messages_unread ON messages(user_id, folder, thread_id) WHERE seen = 0;

CREATE TABLE message_refs (
  user_id INTEGER NOT NULL,
  ref     TEXT NOT NULL,
  msg_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, ref, msg_id)
) WITHOUT ROWID;
CREATE INDEX message_refs_msg ON message_refs(msg_id);
WITH RECURSIVE split(msg_id, user_id, rest, ref) AS (
  SELECT id, user_id, refs || ' ', '' FROM messages WHERE refs <> ''
  UNION ALL
  SELECT msg_id, user_id, substr(rest, instr(rest, ' ') + 1), substr(rest, 1, instr(rest, ' ') - 1) FROM split WHERE rest <> ''
)
INSERT OR IGNORE INTO message_refs(user_id, ref, msg_id)
  SELECT user_id, ref, msg_id FROM split WHERE ref <> ''
  UNION SELECT user_id, in_reply_to, id FROM messages WHERE in_reply_to <> '';

DROP INDEX mail_log_ts;
CREATE INDEX mail_log_ts ON mail_log(ts, direction, status);
`,
	// 4: importing mail from other providers (IMAP or exported files).
	`
CREATE TABLE imports (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  source      TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,
  total       INTEGER NOT NULL DEFAULT 0,
  imported    INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  current     TEXT NOT NULL DEFAULT '',
  message     TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX imports_user ON imports(user_id, id DESC);
CREATE TABLE import_hashes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash    BLOB NOT NULL,
  PRIMARY KEY (user_id, hash)
) WITHOUT ROWID;
`,
	// 5: delivery attempt log for the admin's queue view.
	`
CREATE TABLE queue_attempts (
  id       INTEGER PRIMARY KEY,
  queue_id INTEGER NOT NULL REFERENCES queue(id) ON DELETE CASCADE,
  rcpt_id  INTEGER NOT NULL,
  rcpt     TEXT NOT NULL,
  at       INTEGER NOT NULL,
  host     TEXT NOT NULL DEFAULT '',
  result   TEXT NOT NULL,
  detail   TEXT NOT NULL DEFAULT '',
  ms       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX queue_attempts_q  ON queue_attempts(queue_id, at);
CREATE INDEX queue_attempts_at ON queue_attempts(at, result);
CREATE INDEX queue_rcpts_upd   ON queue_rcpts(updated_at);
`,
	// 6 (1.1.0): custom folders, filters, blocked/allowed senders, mail-app (IMAP) state, push.
	// Messages in a custom folder have folder = 'f:<id>'.
	`
CREATE TABLE user_folders (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, name COLLATE NOCASE)
);

CREATE TABLE filters (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL DEFAULT 0,
  enabled       INTEGER NOT NULL DEFAULT 1,
  match_from    TEXT NOT NULL DEFAULT '',
  match_to      TEXT NOT NULL DEFAULT '',
  match_subject TEXT NOT NULL DEFAULT '',
  match_words   TEXT NOT NULL DEFAULT '',
  action_folder TEXT NOT NULL DEFAULT '',
  mark_read     INTEGER NOT NULL DEFAULT 0,
  star          INTEGER NOT NULL DEFAULT 0,
  hits          INTEGER NOT NULL DEFAULT 0,
  last_hit      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE INDEX filters_user ON filters(user_id, position, id);

CREATE TABLE sender_lists (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  pattern    TEXT NOT NULL,
  kind       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, pattern)
) WITHOUT ROWID;

CREATE TABLE imap_folders (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  folder      TEXT NOT NULL,
  uidvalidity INTEGER NOT NULL,
  uidnext     INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (user_id, folder)
) WITHOUT ROWID;

CREATE TABLE imap_uids (
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  folder   TEXT NOT NULL,
  uid      INTEGER NOT NULL,
  msg_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE, -- ids can be reused after deletes
  deleted  INTEGER NOT NULL DEFAULT 0,
  keywords TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, folder, uid)
) WITHOUT ROWID;
CREATE INDEX imap_uids_msg ON imap_uids(msg_id);
CREATE UNIQUE INDEX imap_uids_fmsg ON imap_uids(user_id, folder, msg_id);

CREATE TABLE imap_drafts (
  id          INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused, so a stale mail-app view can't hit another draft
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  uid         INTEGER NOT NULL,
  blob        TEXT NOT NULL,
  size        INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  seen        INTEGER NOT NULL DEFAULT 1,
  flagged     INTEGER NOT NULL DEFAULT 0,
  answered    INTEGER NOT NULL DEFAULT 0,
  deleted     INTEGER NOT NULL DEFAULT 0,
  keywords    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX imap_drafts_uid ON imap_drafts(user_id, uid);

-- Web Push subscriptions (one per browser/device). Tied to the sign-in session that made it,
-- so signing out, a password change or a disabled account stops notifications on that device.
CREATE TABLE push_subs (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  label      TEXT NOT NULL DEFAULT '',
  preview    INTEGER NOT NULL DEFAULT 1, -- show sender and subject
  created_at INTEGER NOT NULL,
  last_ok    INTEGER NOT NULL DEFAULT 0,
  last_err   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX push_subs_user ON push_subs(user_id);
CREATE INDEX push_subs_session ON push_subs(session_id);
`,
}

// OpenDB opens (and migrates) the SQLite database in dataDir.
func OpenDB(dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, "skienmail.db")
	// Page cache per connection (SM_DB_CACHE_MB, default 64 MB; up to 8 connections) and a
	// memory-mapped read window. Bigger servers can afford more; tiny ones can turn it down.
	cacheMB := 64
	if v, err := strconv.Atoi(os.Getenv("SM_DB_CACHE_MB")); err == nil && v >= 2 && v <= 4096 {
		cacheMB = v
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"+
		"&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)&_txlock=immediate", path, cacheMB*1024, int64(cacheMB)*4<<20)
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
