package core

import (
	"crypto/rsa"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Domain is a mail domain served by this server.
type Domain struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Selector  string `json:"selector"`
	CreatedAt int64  `json:"created_at"`
	keyPEM    string
	key       *rsa.PrivateKey
}

// User is a mailbox owner.
type User struct {
	ID                 int64  `json:"id"`
	DomainID           int64  `json:"domain_id"`
	Local              string `json:"local"`
	Email              string `json:"email"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	Status             string `json:"status"`
	QuotaBytes         int64  `json:"quota_bytes"`
	UsedBytes          int64  `json:"used_bytes"`
	Title              string `json:"title"`
	Phone              string `json:"phone"`
	AvatarVer          int64  `json:"avatar_ver"`
	Signature          string `json:"signature"`
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          int64  `json:"created_at"`
	LastLoginAt        int64  `json:"last_login_at"`
	passwordHash       string
	VacationEnabled    bool   `json:"vacation_enabled"`
	VacationSubject    string `json:"vacation_subject"`
	VacationBody       string `json:"vacation_body"`
	VacationStart      int64  `json:"vacation_start"`
	VacationEnd        int64  `json:"vacation_end"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

var (
	reLocal  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	reDomain = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:[a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)
)

// ValidLocalPart checks the left-hand side of an address we create.
func ValidLocalPart(s string) bool {
	return reLocal.MatchString(s) && !strings.Contains(s, "..")
}

// ValidDomainName checks a DNS domain name.
func ValidDomainName(s string) bool { return len(s) <= 253 && reDomain.MatchString(s) }

// SplitAddress lower-cases and splits an address into local and domain parts.
func SplitAddress(addr string) (local, domain string, ok bool) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	addr = strings.TrimSuffix(strings.TrimPrefix(addr, "<"), ">")
	i := strings.LastIndexByte(addr, '@')
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// ---------------------------------------------------------------- domains

func (a *App) LoadDomains() error {
	rows, err := a.DB.Query(`SELECT id, name, dkim_selector, dkim_key, created_at FROM domains`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]*Domain{}
	for rows.Next() {
		d := &Domain{}
		if err := rows.Scan(&d.ID, &d.Name, &d.Selector, &d.keyPEM, &d.CreatedAt); err != nil {
			return err
		}
		k, err := ParseDKIMKey(d.keyPEM)
		if err != nil {
			a.Log.Error("invalid DKIM key", "domain", d.Name, "err", err)
		}
		d.key = k
		m[strings.ToLower(d.Name)] = d
	}
	if err := rows.Err(); err != nil {
		return err
	}
	a.domainsMu.Lock()
	a.domains = m
	a.domainsMu.Unlock()
	return nil
}

func (a *App) Domain(name string) *Domain {
	a.domainsMu.RLock()
	defer a.domainsMu.RUnlock()
	return a.domains[strings.ToLower(strings.TrimSuffix(name, "."))]
}

func (a *App) DomainByID(id int64) *Domain {
	a.domainsMu.RLock()
	defer a.domainsMu.RUnlock()
	for _, d := range a.domains {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (a *App) Domains() []*Domain {
	a.domainsMu.RLock()
	defer a.domainsMu.RUnlock()
	out := make([]*Domain, 0, len(a.domains))
	for _, d := range a.domains {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *App) IsLocalDomain(name string) bool { return a.Domain(name) != nil }

// PrimaryDomain is the first domain created during setup.
func (a *App) PrimaryDomain() *Domain {
	ds := a.Domains()
	if len(ds) == 0 {
		return nil
	}
	return ds[0]
}

func (a *App) CreateDomain(name string) (*Domain, error) {
	name = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(name, ".")))
	if !ValidDomainName(name) {
		return nil, errors.New("Enter a valid domain name, like example.com")
	}
	if a.Domain(name) != nil {
		return nil, errors.New("That domain already exists")
	}
	key, err := GenerateDKIMKey()
	if err != nil {
		return nil, err
	}
	selector := "sm" + time.Now().Format("200601")
	if _, err := a.DB.Exec(`INSERT INTO domains(name, dkim_selector, dkim_key, created_at) VALUES(?,?,?,?)`, name, selector, key, nowMs()); err != nil {
		return nil, err
	}
	if err := a.LoadDomains(); err != nil {
		return nil, err
	}
	return a.Domain(name), nil
}

func (a *App) DeleteDomain(id int64) error {
	var n int
	a.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE domain_id=?) + (SELECT COUNT(*) FROM aliases WHERE domain_id=?)`, id, id).Scan(&n)
	if n > 0 {
		return errors.New("Remove the users and groups on this domain first")
	}
	if len(a.Domains()) <= 1 {
		return errors.New("You can't remove the only domain")
	}
	if _, err := a.DB.Exec(`DELETE FROM domains WHERE id=?`, id); err != nil {
		return err
	}
	return a.LoadDomains()
}

// ---------------------------------------------------------------- users

const userCols = `id, domain_id, local, email, name, password_hash, role, status, quota_bytes, used_bytes, title, phone,
 avatar_ver, signature, must_change_password, created_at, last_login_at, vacation_enabled, vacation_subject, vacation_body,
 vacation_start, vacation_end`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.DomainID, &u.Local, &u.Email, &u.Name, &u.passwordHash, &u.Role, &u.Status, &u.QuotaBytes,
		&u.UsedBytes, &u.Title, &u.Phone, &u.AvatarVer, &u.Signature, &u.MustChangePassword, &u.CreatedAt, &u.LastLoginAt,
		&u.VacationEnabled, &u.VacationSubject, &u.VacationBody, &u.VacationStart, &u.VacationEnd)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (a *App) UserByID(id int64) (*User, error) {
	return scanUser(a.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (a *App) UserByEmail(email string) (*User, error) {
	return scanUser(a.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(email))))
}

func (a *App) ListUsers() ([]*User, error) {
	rows, err := a.DB.Query(`SELECT ` + userCols + ` FROM users ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (a *App) addressTaken(email string) bool {
	var n int
	a.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE email=?) + (SELECT COUNT(*) FROM aliases WHERE address=?)`, email, email).Scan(&n)
	return n > 0
}

// reservedLocals cannot be used for mailboxes; they are handled by the server.
var reservedLocals = map[string]bool{"mailer-daemon": true}

type NewUser struct {
	Name       string
	Local      string
	DomainID   int64
	Password   string
	Role       string
	QuotaBytes int64
	Title      string
	MustChange bool
}

func (a *App) CreateUser(n NewUser) (*User, error) {
	n.Name = strings.TrimSpace(sanitizeHeaderValue(n.Name))
	n.Local = strings.ToLower(strings.TrimSpace(n.Local))
	if n.Name == "" || len(n.Name) > 120 {
		return nil, errors.New("Enter a name")
	}
	if !ValidLocalPart(n.Local) || reservedLocals[n.Local] {
		return nil, errors.New("Use letters, numbers, dots, dashes or underscores for the address")
	}
	d := a.DomainByID(n.DomainID)
	if d == nil {
		return nil, errors.New("Unknown domain")
	}
	if err := ValidatePassword(n.Password); err != nil {
		return nil, err
	}
	if n.Role != "admin" {
		n.Role = "user"
	}
	email := n.Local + "@" + d.Name
	if a.addressTaken(email) {
		return nil, fmt.Errorf("%s is already in use", email)
	}
	hash, err := HashPassword(n.Password)
	if err != nil {
		return nil, err
	}
	now := nowMs()
	res, err := a.DB.Exec(`INSERT INTO users(domain_id, local, email, name, password_hash, role, quota_bytes, title, must_change_password, created_at, password_changed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, d.ID, n.Local, email, n.Name, hash, n.Role, n.QuotaBytes, strings.TrimSpace(n.Title), n.MustChange, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return a.UserByID(id)
}

func (a *App) SetPassword(userID int64, pw string, mustChange bool) error {
	if err := ValidatePassword(pw); err != nil {
		return err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	_, err = a.DB.Exec(`UPDATE users SET password_hash=?, must_change_password=?, password_changed_at=? WHERE id=?`, hash, mustChange, nowMs(), userID)
	return err
}

// adminCount counts active administrators.
func (a *App) adminCount() int {
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	return n
}

// DeleteUser removes a user and all of their mail.
func (a *App) DeleteUser(id int64) error {
	u, err := a.UserByID(id)
	if err != nil {
		return err
	}
	if u.IsAdmin() && u.Status == "active" && a.adminCount() <= 1 {
		return errors.New("You can't remove the last administrator")
	}
	// One write transaction (BEGIN IMMEDIATE): mail delivered concurrently can't slip in between
	// clearing the search index and deleting the messages, which would leave index rows behind.
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE user_id=?)`, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE user_id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(a.Cfg.DataDir, "mail", fmt.Sprint(id)))
	os.Remove(filepath.Join(a.Cfg.DataDir, "avatars", fmt.Sprintf("%d", id)))
	a.removeUserUploads(id)
	a.Events.Publish(id, Event{Type: "signout"})
	return nil
}

// ---------------------------------------------------------------- aliases (groups)

type Alias struct {
	ID          int64         `json:"id"`
	DomainID    int64         `json:"domain_id"`
	Address     string        `json:"address"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Members     []AliasMember `json:"members"`
	CreatedAt   int64         `json:"created_at"`
}

type AliasMember struct {
	UserID  int64  `json:"user_id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	CanSend bool   `json:"can_send"`
}

func (a *App) ListAliases() ([]*Alias, error) {
	rows, err := a.DB.Query(`SELECT id, domain_id, address, name, description, created_at FROM aliases ORDER BY address`)
	if err != nil {
		return nil, err
	}
	var out []*Alias
	idx := map[int64]*Alias{}
	for rows.Next() {
		al := &Alias{Members: []AliasMember{}}
		if err := rows.Scan(&al.ID, &al.DomainID, &al.Address, &al.Name, &al.Description, &al.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, al)
		idx[al.ID] = al
	}
	rows.Close()
	mrows, err := a.DB.Query(`SELECT m.alias_id, u.id, u.email, u.name, m.can_send FROM alias_members m JOIN users u ON u.id=m.user_id ORDER BY u.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var aid int64
		var m AliasMember
		if err := mrows.Scan(&aid, &m.UserID, &m.Email, &m.Name, &m.CanSend); err != nil {
			return nil, err
		}
		if al := idx[aid]; al != nil {
			al.Members = append(al.Members, m)
		}
	}
	return out, mrows.Err()
}

type AliasInput struct {
	Local       string        `json:"local"`
	DomainID    int64         `json:"domain_id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Members     []AliasMember `json:"members"`
}

func (a *App) SaveAlias(id int64, in AliasInput) (int64, error) {
	in.Name = strings.TrimSpace(sanitizeHeaderValue(in.Name))
	in.Description = strings.TrimSpace(in.Description)
	if len(in.Members) == 0 {
		return 0, errors.New("Add at least one member")
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if id == 0 {
		in.Local = strings.ToLower(strings.TrimSpace(in.Local))
		if !ValidLocalPart(in.Local) || reservedLocals[in.Local] {
			return 0, errors.New("Use letters, numbers, dots, dashes or underscores for the address")
		}
		d := a.DomainByID(in.DomainID)
		if d == nil {
			return 0, errors.New("Unknown domain")
		}
		addr := in.Local + "@" + d.Name
		var taken int // checked on the transaction's own connection (no second pool connection)
		tx.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE email=?) + (SELECT COUNT(*) FROM aliases WHERE address=?)`, addr, addr).Scan(&taken)
		if taken > 0 {
			return 0, fmt.Errorf("%s is already in use", addr)
		}
		res, err := tx.Exec(`INSERT INTO aliases(domain_id, address, name, description, created_at) VALUES(?,?,?,?,?)`, d.ID, addr, in.Name, in.Description, nowMs())
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	} else {
		if _, err := tx.Exec(`UPDATE aliases SET name=?, description=? WHERE id=?`, in.Name, in.Description, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM alias_members WHERE alias_id=?`, id); err != nil {
			return 0, err
		}
	}
	seen := map[int64]bool{}
	for _, m := range in.Members {
		if seen[m.UserID] {
			continue
		}
		seen[m.UserID] = true
		if _, err := tx.Exec(`INSERT INTO alias_members(alias_id, user_id, can_send) VALUES(?,?,?)`, id, m.UserID, m.CanSend); err != nil {
			return 0, errors.New("Unknown member")
		}
	}
	return id, tx.Commit()
}

func (a *App) DeleteAlias(id int64) error {
	_, err := a.DB.Exec(`DELETE FROM aliases WHERE id=?`, id)
	return err
}

// ---------------------------------------------------------------- resolution

// ErrNoSuchUser is returned when a local recipient doesn't exist.
var ErrNoSuchUser = errors.New("no such user")

// ResolveLocal maps a local address (user, group, or plus-address) to the
// active user IDs that should receive it. ok=false when the domain isn't local.
func (a *App) ResolveLocal(addr string) (ids []int64, ok bool, err error) {
	local, domain, valid := SplitAddress(addr)
	if !valid || !a.IsLocalDomain(domain) {
		return nil, false, nil
	}
	if i := strings.IndexByte(local, '+'); i > 0 { // plus addressing: asi+news@ -> asi@
		local = local[:i]
	}
	full := local + "@" + domain
	var uid int64
	var status string
	err = a.DB.QueryRow(`SELECT id, status FROM users WHERE email=?`, full).Scan(&uid, &status)
	if err == nil {
		if status != "active" {
			return nil, true, ErrNoSuchUser
		}
		return []int64{uid}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, true, err
	}
	rows, err := a.DB.Query(`SELECT u.id FROM aliases al JOIN alias_members m ON m.alias_id=al.id JOIN users u ON u.id=m.user_id
		WHERE al.address=? AND u.status='active'`, full)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, true, ErrNoSuchUser
	}
	return ids, true, rows.Err()
}

// SendAs is an address a user may put in From.
type SendAs struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// SendAsAddresses lists the user's own address plus groups they may send as.
func (a *App) SendAsAddresses(u *User) []SendAs {
	out := []SendAs{{Email: u.Email, Name: u.Name}}
	rows, err := a.DB.Query(`SELECT al.address, al.name FROM aliases al JOIN alias_members m ON m.alias_id=al.id WHERE m.user_id=? AND m.can_send=1 ORDER BY al.address`, u.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s SendAs
		rows.Scan(&s.Email, &s.Name)
		if s.Name == "" {
			s.Name = a.OrgName()
		}
		out = append(out, s)
	}
	return out
}

// CanSendAs reports whether u may use addr as the From / envelope sender.
func (a *App) CanSendAs(u *User, addr string) (SendAs, bool) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	for _, s := range a.SendAsAddresses(u) {
		if strings.EqualFold(s.Email, addr) {
			return s, true
		}
	}
	return SendAs{}, false
}
