package core

import (
	"os"
	"strconv"
	"strings"
)

// Config holds process-level settings read from the environment.
// Organisation-level settings (domain, hostname, relay...) live in the database
// and are edited from the admin console.
type Config struct {
	DataDir string

	HTTPAddr string // plain HTTP listener (put a reverse proxy in front), e.g. ":8080"

	SMTPAddr        string // inbound MX, e.g. ":25"
	SubmissionAddr  string // STARTTLS submission, e.g. ":587"
	SubmissionsAddr string // implicit TLS submission, e.g. ":465"
	IMAPAddr        string // mail apps, STARTTLS, e.g. ":143"
	IMAPSAddr       string // mail apps, implicit TLS, e.g. ":993"

	Hostname string // overrides the hostname stored in settings (EHLO / banner / TLS name)

	TLSCertFile string
	TLSKeyFile  string

	ACME          bool   // obtain Let's Encrypt certificates directly (no reverse proxy)
	ACMEEmail     string //
	HTTPSAddr     string // used when ACME is on, e.g. ":443"
	ACMEHTTPAddr  string // used when ACME is on, e.g. ":80" (HTTP-01 + redirect)
	SetupToken    string // optional fixed setup token
	TrustProxy    bool   // trust X-Forwarded-* from private networks (default true)
	MaxMessageMB  int    // default maximum message size, overridable in settings
	LogLevelDebug bool
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, ""))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

// LoadConfig reads SM_* environment variables.
func LoadConfig() *Config {
	return &Config{
		DataDir:         env("SM_DATA_DIR", "/data"),
		HTTPAddr:        env("SM_HTTP_ADDR", ":8080"),
		SMTPAddr:        env("SM_SMTP_ADDR", ":25"),
		SubmissionAddr:  env("SM_SUBMISSION_ADDR", ":587"),
		SubmissionsAddr: env("SM_SUBMISSIONS_ADDR", ":465"),
		IMAPAddr:        env("SM_IMAP_ADDR", ":143"),
		IMAPSAddr:       env("SM_IMAPS_ADDR", ":993"),
		Hostname:        strings.ToLower(env("SM_HOSTNAME", "")),
		TLSCertFile:     env("SM_TLS_CERT", ""),
		TLSKeyFile:      env("SM_TLS_KEY", ""),
		ACME:            envBool("SM_ACME", false),
		ACMEEmail:       env("SM_ACME_EMAIL", ""),
		HTTPSAddr:       env("SM_HTTPS_ADDR", ":443"),
		ACMEHTTPAddr:    env("SM_ACME_HTTP_ADDR", ":80"),
		SetupToken:      env("SM_SETUP_TOKEN", ""),
		TrustProxy:      envBool("SM_TRUST_PROXY", true),
		MaxMessageMB:    envInt("SM_MAX_MESSAGE_MB", 25),
		LogLevelDebug:   envBool("SM_DEBUG", false),
	}
}
