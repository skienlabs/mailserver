package core

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// InboundVerdict is the outcome of authentication and spam checks.
type InboundVerdict struct {
	SPF        string
	DKIM       string
	DMARC      string
	Score      float64
	Reasons    []string
	Reject     bool
	RejectMsg  string
	Quarantine bool
	Header     string // Authentication-Results value
	Summary    string
	// FromAuth: the From domain itself is vouched for (SPF or DKIM passes in alignment with it)
	// and DMARC doesn't fail. "Always allow" only rescues such mail from Spam.
	FromAuth bool
}

// dkimLookupTXT replaces DNS for DKIM keys in tests (nil = real DNS).
var dkimLookupTXT func(domain string) ([]string, error)

func orgDomain(d string) string {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	if o, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return o
	}
	return d
}

func aligned(a, b string, mode dmarc.AlignmentMode) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	if mode == dmarc.AlignmentStrict {
		return a == b
	}
	return orgDomain(a) == orgDomain(b)
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// lookupDMARC finds the policy for fromDomain, falling back to the organisational domain.
func lookupDMARC(fromDomain string) (*dmarc.Record, bool) {
	rec, err := dmarc.Lookup(fromDomain)
	if err == nil {
		return rec, false
	}
	if org := orgDomain(fromDomain); org != fromDomain {
		if rec, err := dmarc.Lookup(org); err == nil {
			return rec, true
		}
	}
	return nil, false
}

func dnsblListed(ctx context.Context, ip net.IP, zone string) bool {
	v4 := ip.To4()
	if v4 == nil || zone == "" {
		return false
	}
	name := fmt.Sprintf("%d.%d.%d.%d.%s", v4[3], v4[2], v4[1], v4[0], zone)
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(c, name)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		// 127.255.255.x are error codes (e.g. "query via public resolver refused") — not listings.
		if strings.HasPrefix(a, "127.") && !strings.HasPrefix(a, "127.255.255.") {
			return true
		}
	}
	return false
}

// CheckInbound authenticates an incoming message and scores it for spam.
func (a *App) CheckInbound(ip net.IP, helo, mailFrom string, raw []byte, meta *Meta) InboundVerdict {
	v := InboundVerdict{}
	add := func(score float64, reason string) {
		v.Score += score
		v.Reasons = append(v.Reasons, reason)
	}
	public := isPublicIP(ip)

	// --- SPF
	sender := mailFrom
	if sender == "" {
		sender = "postmaster@" + helo
	}
	spfRes, _ := spf.CheckHostWithSender(ip, helo, sender)
	v.SPF = string(spfRes)
	if v.SPF == "" {
		v.SPF = "none"
	}
	switch spfRes {
	case spf.Fail:
		add(3.5, "SPF fail")
	case spf.SoftFail:
		add(1.5, "SPF softfail")
	case spf.None, spf.PermError:
		if public {
			add(0.5, "no valid SPF")
		}
	}
	_, mailFromDomain, _ := SplitAddress(sender)

	// --- DKIM
	verifs, _ := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{MaxVerifications: 5, LookupTXT: dkimLookupTXT})
	var dkimPass []string
	dkimFail := false
	for _, vf := range verifs {
		if vf.Err == nil {
			dkimPass = append(dkimPass, vf.Domain)
		} else {
			dkimFail = true
		}
	}
	switch {
	case len(dkimPass) > 0:
		v.DKIM = "pass"
	case dkimFail:
		v.DKIM = "fail"
		add(1, "DKIM signature invalid")
	default:
		v.DKIM = "none"
		if public {
			add(0.3, "no DKIM signature")
		}
	}

	// --- DMARC
	_, fromDomain, _ := SplitAddress(meta.From.Email)
	if fromDomain != "" {
		v.FromAuth = spfRes == spf.Pass && aligned(mailFromDomain, fromDomain, dmarc.AlignmentRelaxed)
		for _, d := range dkimPass {
			if aligned(d, fromDomain, dmarc.AlignmentRelaxed) {
				v.FromAuth = true
			}
		}
	}
	v.DMARC = "none"
	if fromDomain != "" {
		if rec, sub := lookupDMARC(fromDomain); rec != nil {
			pass := false
			if spfRes == spf.Pass && aligned(mailFromDomain, fromDomain, rec.SPFAlignment) {
				pass = true
			}
			for _, d := range dkimPass {
				if aligned(d, fromDomain, rec.DKIMAlignment) {
					pass = true
				}
			}
			if pass {
				v.DMARC = "pass"
			} else {
				v.DMARC = "fail"
				policy := rec.Policy
				if sub && rec.SubdomainPolicy != "" {
					policy = rec.SubdomainPolicy
				}
				switch policy {
				case dmarc.PolicyReject:
					v.Reject = true
					v.RejectMsg = "Message rejected per DMARC policy of " + fromDomain
				case dmarc.PolicyQuarantine:
					v.Quarantine = true
					add(5, "DMARC quarantine")
				default:
					add(2, "DMARC fail")
				}
			}
		}
	}

	// --- Anti-spoofing of our own domains
	if a.IsLocalDomain(fromDomain) && v.DMARC != "pass" && !(spfRes == spf.Pass && a.IsLocalDomain(mailFromDomain)) {
		if meta.ListID != "" {
			// Mailing lists often break DKIM when relaying our own users' posts back.
			add(3, "unauthenticated local From via mailing list")
		} else {
			v.Reject = true
			v.RejectMsg = "Unauthenticated mail claiming to be from " + fromDomain + " is not accepted"
		}
	}
	if name := strings.ToLower(meta.From.Name); strings.Contains(name, "@") {
		for _, d := range a.Domains() {
			if strings.Contains(name, "@"+d.Name) && !strings.EqualFold(fromDomain, d.Name) {
				add(3, "display name impersonates a local address")
			}
		}
	}

	// --- Header hygiene
	if meta.MessageID == "" {
		add(1.5, "missing Message-ID")
	}
	if meta.From.Email == "" {
		add(3, "missing From")
	}
	if !strings.Contains(helo, ".") || net.ParseIP(strings.Trim(helo, "[]")) != nil {
		if public {
			add(1, "HELO is not a hostname")
		}
	}

	// --- Reverse DNS + blocklists
	if public {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		names, err := net.DefaultResolver.LookupAddr(ctx, ip.String())
		cancel()
		if err != nil || len(names) == 0 {
			add(1, "no reverse DNS")
		}
		for _, zone := range strings.Fields(strings.ReplaceAll(a.Settings.Get(SetDNSBL), ",", " ")) {
			if dnsblListed(context.Background(), ip, zone) {
				add(5, "listed on "+zone)
			}
		}
	}

	threshold := a.Settings.Float(SetSpamThreshold)
	if v.Score >= threshold {
		v.Quarantine = true
	}

	results := []authres.Result{
		&authres.SPFResult{Value: authres.ResultValue(v.SPF), From: mailFrom, Helo: helo},
		&authres.DKIMResult{Value: authres.ResultValue(v.DKIM), Domain: strings.Join(dkimPass, ",")},
		&authres.DMARCResult{Value: authres.ResultValue(v.DMARC), From: fromDomain},
	}
	v.Header = authres.Format(a.Hostname(), results)
	v.Summary = fmt.Sprintf("spf=%s dkim=%s dmarc=%s", v.SPF, v.DKIM, v.DMARC)
	if v.DMARC == "fail" {
		v.FromAuth = false
	}
	if len(v.Reasons) > 0 {
		v.Summary += "; " + strings.Join(v.Reasons, ", ")
	}
	return v
}
