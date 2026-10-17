package core

import (
	"database/sql"
	"strconv"
	"strings"
	"sync"
)

// Setting keys.
const (
	SetOrgName        = "org_name"
	SetHostname       = "hostname"
	SetSetupDone      = "setup_done"
	SetRelayHost      = "relay_host"
	SetRelayPort      = "relay_port"
	SetRelayUser      = "relay_user"
	SetRelayPass      = "relay_pass"
	SetRelayTLS       = "relay_tls" // starttls | tls
	SetMaxMessageMB   = "max_message_mb"
	SetSpamThreshold  = "spam_threshold"
	SetDNSBL          = "dnsbl"
	SetDefaultQuotaMB = "default_quota_mb"
	SetTrashDays      = "trash_days"
	SetAccent         = "accent"
)

var settingDefaults = map[string]string{
	SetOrgName:        "",
	SetHostname:       "",
	SetRelayHost:      "",
	SetRelayPort:      "587",
	SetRelayUser:      "",
	SetRelayPass:      "",
	SetRelayTLS:       "starttls",
	SetMaxMessageMB:   "25",
	SetSpamThreshold:  "5",
	SetDNSBL:          "zen.spamhaus.org",
	SetDefaultQuotaMB: "5120",
	SetTrashDays:      "30",
}

// Settings is a write-through cache over the settings table.
type Settings struct {
	db *sql.DB
	mu sync.RWMutex
	m  map[string]string
}

func NewSettings(db *sql.DB) (*Settings, error) {
	s := &Settings{db: db, m: map[string]string{}}
	rows, err := db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		s.m[k] = v
	}
	return s, rows.Err()
}

func (s *Settings) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.m[key]; ok {
		return v
	}
	return settingDefaults[key]
}

func (s *Settings) Int(key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s.Get(key)))
	if err != nil {
		n, _ = strconv.Atoi(settingDefaults[key])
	}
	return n
}

func (s *Settings) Float(key string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s.Get(key)), 64)
	if err != nil {
		f, _ = strconv.ParseFloat(settingDefaults[key], 64)
	}
	return f
}

func (s *Settings) Set(values map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for k, v := range values {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for k, v := range values {
		s.m[k] = v
	}
	s.mu.Unlock()
	return nil
}
