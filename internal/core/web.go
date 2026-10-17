package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skienlabs/mailserver/web"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

var httpState struct {
	mu      sync.Mutex
	servers []*http.Server
}

// ---------------------------------------------------------------- static assets

type asset struct {
	body  []byte
	gz    []byte
	etag  string
	ctype string
}

var reImport = regexp.MustCompile(`((?:from|import)\s*\(?\s*['"]\./[\w./-]+\.js)(['"])`)

type assetStore struct {
	files   map[string]*asset
	version string
}

func loadAssets() (*assetStore, error) {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	st := &assetStore{files: map[string]*asset{}}
	var names []string
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	h := sha256.New()
	raw := map[string][]byte{}
	for _, n := range names {
		b, err := fs.ReadFile(sub, n)
		if err != nil {
			return nil, err
		}
		raw[n] = b
		h.Write([]byte(n))
		h.Write(b)
	}
	st.version = hex.EncodeToString(h.Sum(nil))[:10]
	for _, n := range names {
		b := raw[n]
		if n == "index.html" || n == "sw.js" {
			b = bytes.ReplaceAll(b, []byte("{{V}}"), []byte(st.version))
			b = bytes.ReplaceAll(b, []byte("{{PRODUCT}}"), []byte(ProductName))
		}
		if strings.HasPrefix(n, "js/") && strings.HasSuffix(n, ".js") {
			// Version module imports so every release is fetched as one consistent set.
			b = reImport.ReplaceAll(b, []byte("${1}?v="+st.version+"${2}"))
		}
		ct := mime.TypeByExtension(path.Ext(n))
		switch path.Ext(n) {
		case ".webmanifest":
			ct = "application/manifest+json"
		case ".js":
			ct = "text/javascript; charset=utf-8"
		case ".woff2":
			ct = "font/woff2"
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		a := &asset{body: b, ctype: ct}
		sum := sha256.Sum256(b)
		a.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
		if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") || strings.Contains(ct, "json") || strings.Contains(ct, "svg") {
			var gz bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if gz.Len() < len(b) {
				a.gz = gz.Bytes()
			}
		}
		st.files["/"+n] = a
	}
	// Browsers and tools still probe /favicon.ico; browsers accept a PNG there.
	if f := st.files["/icons/favicon-32.png"]; f != nil {
		st.files["/favicon.ico"] = f
	}
	return st, nil
}

func (st *assetStore) serve(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	a := st.files[p]
	if a == nil {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	h.Set("Vary", "Accept-Encoding")
	switch {
	case p == "/index.html" || p == "/sw.js" || p == "/manifest.webmanifest":
		h.Set("Cache-Control", "no-cache")
	case r.URL.Query().Get("v") != "":
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		h.Set("Cache-Control", "public, max-age=3600")
	}
	if match := r.Header.Get("If-None-Match"); match == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	body := a.body
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	if r.Method == http.MethodHead {
		return true
	}
	w.Write(body)
	return true
}

// ---------------------------------------------------------------- server

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https: http:; font-src 'self' data:; connect-src 'self'; frame-src 'self'; " +
	"frame-ancestors 'self'; base-uri 'none'; form-action 'self'; object-src 'none'; manifest-src 'self'; worker-src 'self'"

func (a *App) handler() (http.Handler, error) {
	assets, err := loadAssets()
	if err != nil {
		return nil, err
	}
	api := http.NewServeMux()
	a.routes(api, assets.version)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if a.isSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.URL.Path == "/healthz" {
			io.WriteString(w, "ok")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// CSRF defence: custom header (forces CORS preflight) + same-origin check.
				if r.Header.Get("X-SM") != "1" {
					httpError(w, http.StatusForbidden, "Missing request header")
					return
				}
				if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r) {
					httpError(w, http.StatusForbidden, "Cross-origin request blocked")
					return
				}
			}
			if r.URL.Path != "/api/events" && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				gw := &gzipJSONWriter{ResponseWriter: w}
				defer gw.Close()
				w = gw
			}
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if !assets.serve(w, r) {
			http.NotFound(w, r)
		}
	}), nil
}

func sameOrigin(origin string, r *http.Request) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return strings.EqualFold(o, host) || strings.EqualFold(o, r.Host)
}

func (a *App) startHTTP() error {
	h, err := a.handler()
	if err != nil {
		return err
	}
	mk := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
	}
	var servers []*http.Server
	type lst struct {
		srv *http.Server
		ln  net.Listener
	}
	var ls []lst
	add := func(srv *http.Server) error {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range ls {
				l.ln.Close()
			}
			return err
		}
		ls = append(ls, lst{srv, ln})
		servers = append(servers, srv)
		return nil
	}
	if a.Cfg.ACME {
		https := mk(a.Cfg.HTTPSAddr, h)
		https.TLSConfig = a.TLS.HTTPSConfig()
		if err := add(https); err != nil {
			return err
		}
		redirect := mk(a.Cfg.ACMEHTTPAddr, a.TLS.acme.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+a.Hostname()+r.URL.RequestURI(), http.StatusMovedPermanently)
		})))
		if err := add(redirect); err != nil {
			return err
		}
	}
	if a.Cfg.HTTPAddr != "" && a.Cfg.HTTPAddr != "off" {
		if err := add(mk(a.Cfg.HTTPAddr, h)); err != nil {
			return err
		}
	}
	for _, l := range ls {
		go func(l lst) {
			var err error
			if l.srv.TLSConfig != nil {
				err = l.srv.ServeTLS(l.ln, "", "")
			} else {
				err = l.srv.Serve(l.ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("http server stopped", "addr", l.srv.Addr, "err", err)
			}
		}(l)
		a.Log.Info("http listening", "addr", l.srv.Addr)
	}
	httpState.mu.Lock()
	httpState.servers = servers
	httpState.mu.Unlock()
	return nil
}

func (a *App) stopHTTP(ctx context.Context) {
	httpState.mu.Lock()
	servers := httpState.servers
	httpState.mu.Unlock()
	for _, s := range servers {
		s.Shutdown(ctx)
	}
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "Invalid request")
		return false
	}
	return true
}

func (a *App) internalError(w http.ResponseWriter, err error) {
	a.Log.Error("request failed", "err", err)
	httpError(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
}

func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if a.Cfg.TrustProxy && ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" && net.ParseIP(xr) != nil {
			return xr
		}
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			if first := strings.TrimSpace(strings.Split(xf, ",")[0]); net.ParseIP(first) != nil {
				return first
			}
		}
	}
	return host
}

func (a *App) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if a.Cfg.TrustProxy {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

func sessionFrom(r *http.Request) int64 {
	id, _ := r.Context().Value(ctxSession).(int64)
	return id
}

// gzipJSONWriter compresses JSON API responses (lists and conversations are 25–300 KB of
// highly repetitive JSON). Binary responses (attachments, avatars, raw) pass through.
type gzipJSONWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

func (g *gzipJSONWriter) WriteHeader(code int) {
	if !g.decided {
		g.decided = true
		h := g.Header()
		if strings.HasPrefix(h.Get("Content-Type"), "application/json") && h.Get("Content-Encoding") == "" && code != http.StatusNoContent && code != http.StatusNotModified {
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			h.Del("Content-Length")
			g.gz = gzPool.Get().(*gzip.Writer)
			g.gz.Reset(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipJSONWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipJSONWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipJSONWriter) Close() {
	if g.gz != nil {
		g.gz.Close()
		gzPool.Put(g.gz)
		g.gz = nil
	}
}
