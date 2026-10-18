package core

import (
	"net"
	"strings"
	"time"
)

// Failed-login limits per 15 minutes. Attempts are counted *before* the password is
// hashed (and refunded on success), so a burst of connections can't run Argon2 more
// often than these limits allow.
const (
	authWindow         = 15 * time.Minute
	authLimitIP        = 30 // per IPv4 address or IPv6 /64
	authLimitAccountIP = 8  // per account from one address: stops a single guesser
	authLimitAccount   = 50 // per account from everywhere: slows distributed guessing
)

// ipKey groups IPv6 clients by /64, because each host usually owns the whole prefix.
func ipKey(ip string) string {
	p := net.ParseIP(strings.Trim(ip, "[]"))
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return v4.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func authKeys(ip, account string) [3]string {
	k := ipKey(ip)
	account = strings.ToLower(strings.TrimSpace(account))
	return [3]string{"authfail-ip:" + k, "authfail-acct-ip:" + account + "|" + k, "authfail-acct:" + account}
}

// imapAuthKeys: mail apps keep their own per-address counters, so a phone retrying an old
// password can't lock everyone at that address out of the web app; the per-account limit
// (distributed guessing) is shared.
func imapAuthKeys(ip, account string) [3]string {
	k := authKeys(ip, account)
	return [3]string{"imap-" + k[0], "imap-" + k[1], k[2]}
}

// authBegin records an attempt and reports whether it may proceed to password checking.
// Shared by web sign-in and SMTP AUTH.
func (a *App) authBegin(ip, account string) bool { return a.authBeginKeys(authKeys(ip, account)) }

func (a *App) authBeginKeys(k [3]string) bool {
	ok1 := a.limiter.Allow(k[0], authLimitIP, authWindow)
	ok2 := a.limiter.Allow(k[1], authLimitAccountIP, authWindow)
	ok3 := a.limiter.Allow(k[2], authLimitAccount, authWindow)
	return ok1 && ok2 && ok3
}

// authSucceeded refunds the attempt recorded by authBegin.
func (a *App) authSucceeded(ip, account string) { a.authRefund(authKeys(ip, account)) }

func (a *App) authRefund(k [3]string) {
	for _, key := range k {
		a.limiter.Refund(key)
	}
}
