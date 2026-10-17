package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// TLSManager supplies certificates for SMTP STARTTLS / SMTPS and (in ACME
// mode) HTTPS. Three sources, in order of preference:
//  1. Let's Encrypt via ACME (SM_ACME=true)
//  2. PEM files (SM_TLS_CERT / SM_TLS_KEY), hot-reloaded when they change
//     — point these at your reverse proxy's certificate for the mail hostname
//  3. A self-signed certificate (fine for opportunistic STARTTLS on port 25)
type TLSManager struct {
	app  *App
	acme *autocert.Manager

	mu        sync.Mutex
	fileCert  *tls.Certificate
	fileMod   time.Time
	fileCheck time.Time
	self      *tls.Certificate
	selfHost  string
}

func NewTLSManager(a *App) *TLSManager {
	t := &TLSManager{app: a}
	if a.Cfg.ACME {
		t.acme = &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(filepath.Join(a.Cfg.DataDir, "certs", "acme")),
			Email:  a.Cfg.ACMEEmail,
			HostPolicy: func(_ context.Context, host string) error {
				if strings.EqualFold(host, a.Hostname()) {
					return nil
				}
				return errors.New("host not allowed")
			},
		}
	}
	return t
}

// Mode describes the active certificate source for the admin console.
func (t *TLSManager) Mode() string {
	switch {
	case t.acme != nil:
		return "letsencrypt"
	case t.app.Cfg.TLSCertFile != "":
		return "file"
	default:
		return "self-signed"
	}
}

func (t *TLSManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if t.acme != nil {
		h := *hello
		if h.ServerName == "" || !strings.EqualFold(h.ServerName, t.app.Hostname()) {
			h.ServerName = t.app.Hostname()
		}
		c, err := t.acme.GetCertificate(&h)
		if err == nil {
			return c, nil
		}
		t.app.Log.Warn("acme certificate unavailable, using fallback", "err", err)
	}
	if t.app.Cfg.TLSCertFile != "" {
		c, err := t.loadFile()
		if err == nil {
			return c, nil
		}
		t.app.Log.Error("loading TLS certificate files", "err", err)
	}
	return t.selfSigned()
}

// ServerConfig is used for SMTP listeners.
func (t *TLSManager) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: t.GetCertificate,
	}
}

// HTTPSConfig is used for the HTTPS listener in ACME mode.
func (t *TLSManager) HTTPSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: t.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
	}
}

func (t *TLSManager) loadFile() (*tls.Certificate, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fileCert != nil && time.Since(t.fileCheck) < time.Minute {
		return t.fileCert, nil
	}
	t.fileCheck = time.Now()
	st, err := os.Stat(t.app.Cfg.TLSCertFile)
	if err != nil {
		if t.fileCert != nil {
			return t.fileCert, nil
		}
		return nil, err
	}
	if t.fileCert != nil && st.ModTime().Equal(t.fileMod) {
		return t.fileCert, nil
	}
	c, err := tls.LoadX509KeyPair(t.app.Cfg.TLSCertFile, t.app.Cfg.TLSKeyFile)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			err = fmt.Errorf("%w — the container user can't read the key; see README (TLS certificates)", err)
		}
		if t.fileCert != nil {
			return t.fileCert, nil
		}
		return nil, err
	}
	t.fileCert, t.fileMod = &c, st.ModTime()
	t.app.Log.Info("loaded TLS certificate", "file", t.app.Cfg.TLSCertFile)
	return t.fileCert, nil
}

func (t *TLSManager) selfSigned() (*tls.Certificate, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	host := t.app.Hostname()
	if t.self != nil && t.selfHost == host {
		return t.self, nil
	}
	dir := filepath.Join(t.app.Cfg.DataDir, "certs")
	certPath, keyPath := filepath.Join(dir, "selfsigned.crt"), filepath.Join(dir, "selfsigned.key")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && leaf.VerifyHostname(host) == nil && time.Now().Before(leaf.NotAfter.Add(-30*24*time.Hour)) {
			t.self, t.selfHost = &c, host
			return t.self, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{ProductName}},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	os.WriteFile(certPath, certPEM, 0o644)
	os.WriteFile(keyPath, keyPEM, 0o600)
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	t.self, t.selfHost = &c, host
	return t.self, nil
}
