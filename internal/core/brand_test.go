package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCleanAppName(t *testing.T) {
	ok := map[string]string{
		"Softop Mails":                     "Softop Mails",
		"  Softop   Mails \n":              "Softop Mails",
		"Société Mails":                    "Société Mails",
		"O'Neil & Co Mail":                 "O'Neil & Co Mail",
		"Acme (EU) Mails":                  "Acme (EU) Mails",
		strings.Repeat("a", maxAppNameLen): strings.Repeat("a", maxAppNameLen),
	}
	for in, want := range ok {
		got, err := cleanAppName(in)
		if err != nil || got != want {
			t.Errorf("cleanAppName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "<script>", `A"B`, "a/b", `a\b`, strings.Repeat("a", maxAppNameLen+1), "x‮y", "x\x00y"} {
		if got, err := cleanAppName(in); err == nil {
			t.Errorf("cleanAppName(%q) = %q, want an error", in, got)
		}
	}
}

func TestNormalizeAccent(t *testing.T) {
	for in, want := range map[string]string{"#4453F2": "#4453f2", " #0b57d0 ": "#0b57d0", "#000000": "#000000"} {
		if got, ok := normalizeAccent(in); !ok || got != want {
			t.Errorf("normalizeAccent(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "4453f2", "#445", "#4453f2ff", "red", "#gggggg", "#4453f2;}body{x:y"} {
		if _, ok := normalizeAccent(in); ok {
			t.Errorf("normalizeAccent(%q) accepted", in)
		}
	}
}

func TestDefaultAppName(t *testing.T) {
	if got := defaultAppName("Softop"); got != "Softop Mails" {
		t.Fatal(got)
	}
	if got := defaultAppName(strings.Repeat("x", 39)); len([]rune(got)) > maxAppNameLen {
		t.Fatal("too long:", got)
	}
	if got := defaultAppName("  "); got != genericAppName {
		t.Fatal(got)
	}
}

// Whatever colour is chosen, text stays readable in both themes.
func TestBrandVarsContrast(t *testing.T) {
	colours := append([]string{"#ffffff", "#000000", "#ffff00", "#00ff00", "#00ffff", "#ff0000", "#0000ff", "#808080",
		"#f6c343", "#fde047", "#1e1b4b", "#111111", "#fafafa", "#e91e63", "#ff9800"}, AccentPresets...)
	for r := 0; r < 256; r += 51 {
		for g := 0; g < 256; g += 51 {
			for b := 0; b < 256; b += 51 {
				colours = append(colours, fmt.Sprintf("#%02x%02x%02x", r, g, b))
			}
		}
	}
	for _, c := range colours {
		v := brandVars(c)
		for k, val := range v {
			if !reHexColor.MatchString(val) {
				t.Fatalf("%s: %s = %q", c, k, val)
			}
		}
		p := func(k string) rgb { return parseHex(v[k]) }
		check := func(what string, a, b rgb, min float64) {
			if got := contrast(a, b); got < min-0.01 {
				t.Errorf("%s: %s contrast %.2f < %.1f", c, what, got, min)
			}
		}
		check("light primary on white", p("--b-primary"), white, 4.5)
		check("light on-primary", p("--b-on-primary"), p("--b-primary"), 4.5)
		check("light on-accent", p("--b-on-accent"), p("--b-accent"), 4.5)
		check("light accent visible on white", p("--b-accent"), white, 1.49)
		check("light container visible on white", p("--b-primary-container"), white, 1.11)
		check("light selected row visible on white", p("--b-selected"), white, 1.09)
		check("snackbar action (dark mode)", p("--b-primary-strong"), parseHex("#e4e4e7"), 4.5)
		check("snackbar action (light mode)", p("--bd-primary"), parseHex("#27272a"), 4.5)
		check("light container text", p("--b-on-primary-container"), p("--b-primary-container"), 4.5)
		check("dark primary on black", p("--bd-primary"), black, 4.5)
		check("dark on-primary", p("--bd-on-primary"), p("--bd-primary"), 4.5)
		check("dark on-accent", p("--bd-on-accent"), p("--bd-accent"), 4.5)
		check("dark container text", p("--bd-on-primary-container"), p("--bd-primary-container"), 4.5)
		check("dark accent visible on black", p("--bd-accent"), black, 2.9)
	}
	// the colours offered stay exactly what was chosen
	for _, c := range AccentPresets {
		if v := brandVars(c); v["--b-primary"] != c || v["--b-accent"] != c {
			t.Errorf("preset %s changed: %v", c, v)
		}
	}
}

func TestBrandIcons(t *testing.T) {
	for file, spec := range iconSpecs {
		for _, c := range []string{"#4453f2", "#fde047", "#000000", "#ffffff"} {
			f := brandFile(c, file)
			img, err := png.Decode(bytes.NewReader(f.body))
			if err != nil || img.Bounds().Dx() != spec.size || img.Bounds().Dy() != spec.size {
				t.Fatalf("%s %s: %v", file, c, err)
			}
			_, _, _, a := img.At(0, 0).RGBA()
			if (spec.rounded || spec.badge) != (a == 0) {
				t.Errorf("%s %s: corner alpha %d", file, c, a)
			}
			y := spec.size / 2
			if spec.badge {
				y = spec.size * 7 / 10 // below the flap
			}
			if _, _, _, a = img.At(spec.size/2, y).RGBA(); a != 0xffff {
				t.Errorf("%s %s: centre not opaque", file, c)
			}
		}
	}
	if brandFile("#4453f2", "nope.png") != nil || brandFile("#4453f2", "../x") != nil {
		t.Fatal("unknown icon served")
	}
	if os.Getenv("SM_ICON_DIR") != "" { // write samples for a visual check
		for file := range iconSpecs {
			os.WriteFile(os.Getenv("SM_ICON_DIR")+"/"+strings.TrimPrefix("#4453f2", "#")+"-"+file, brandFile("#4453f2", file).body, 0o644)
			os.WriteFile(os.Getenv("SM_ICON_DIR")+"/fde047-"+file, brandFile("#fde047", file).body, 0o644)
		}
	}
}

func TestBrandingEndToEnd(t *testing.T) {
	app, _, admin, user := newTestApp(t, false)
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	loginClient := func(email, pw string) *client {
		c := newClient(t, srv.URL)
		if code := c.do("POST", "/api/auth/login", map[string]any{"email": email, "password": pw}, nil); code != 200 {
			t.Fatalf("login %s: %d", email, code)
		}
		return c
	}
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// before setup there is no organisation: a neutral name
	if app.AppName() != genericAppName || app.OrgName() != genericAppName {
		t.Fatalf("app name %q, org %q", app.AppName(), app.OrgName())
	}
	app.Settings.Set(map[string]string{SetOrgName: "Softop"})
	if app.AppName() != "Softop Mails" || app.OrgName() != "Softop" {
		t.Fatalf("app name %q", app.AppName())
	}
	if err := app.Settings.Set(map[string]string{SetAppName: `Softop <Mails> & "Co"`, SetAccent: "#be123c"}); err != nil {
		t.Fatal(err)
	}
	w := get("/")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<title>Softop &lt;Mails&gt; &amp; &#34;Co&#34;</title>") || strings.Contains(body, "<Mails>") {
		t.Fatalf("index: %d %s", w.Code, body)
	}
	if !strings.Contains(body, "--b-accent:#be123c;") || !strings.Contains(body, "/brand/mark.svg?c=be123c1") || strings.Contains(body, "{{") {
		t.Fatalf("index brand: %s", body)
	}
	if w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Fatal("index caching", w.Header())
	}
	if get("/", "If-None-Match", w.Header().Get("ETag")).Code != http.StatusNotModified {
		t.Fatal("index ETag")
	}
	// the name changes: the page (and its ETag) follow at once
	app.Settings.Set(map[string]string{SetAppName: "Softop Mails"})
	w2 := get("/", "If-None-Match", w.Header().Get("ETag"))
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "<title>Softop Mails</title>") {
		t.Fatal("index did not change with the name")
	}
	// manifest is valid JSON with the name
	w = get("/manifest.webmanifest")
	var man struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		Icons     []struct{ Src string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &man); err != nil || man.Name != "Softop Mails" {
		t.Fatalf("manifest: %v %s", err, w.Body.String())
	}
	for _, ic := range man.Icons {
		if r := get(ic.Src); r.Code != 200 {
			t.Fatalf("manifest icon %s: %d", ic.Src, r.Code)
		}
	}
	app.Settings.Set(map[string]string{SetAppName: `Bad "name"`})
	if err := json.Unmarshal(get("/manifest.webmanifest").Body.Bytes(), &man); err != nil || man.Name != `Bad "name"` {
		t.Fatalf("manifest escaping: %v %q", err, man.Name)
	}
	app.Settings.Set(map[string]string{SetAppName: "Softop Mails"})
	// icons: right colour key is cached for a year, any other is revalidated
	if r := get("/brand/icon-192.png?c=be123c1"); r.Code != 200 || !strings.Contains(r.Header().Get("Cache-Control"), "immutable") || r.Header().Get("Content-Type") != "image/png" {
		t.Fatal("icon", r.Code, r.Header())
	}
	if r := get("/brand/icon-192.png?c=4453f21x"); r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("stale icon key", r.Header())
	}
	if r := get("/brand/mark.svg"); r.Code != 200 || !strings.Contains(r.Body.String(), `fill="#be123c"`) {
		t.Fatal("svg", r.Body.String())
	}
	if r := get("/favicon.ico"); r.Code != 200 || r.Header().Get("Content-Type") != "image/png" {
		t.Fatal("favicon.ico", r.Code)
	}
	for _, p := range []string{"/brand/", "/brand/x.png", "/brand/../index.html", "/brand/icon-192.png/x"} {
		if r := get(p); r.Code != 404 {
			t.Fatalf("%s: %d", p, r.Code)
		}
	}
	// public setup status and /api/me carry the brand
	var st struct {
		Product string
		Brand   struct {
			Name, Accent, Icon string
			Vars               map[string]string
		}
	}
	json.Unmarshal(get("/api/setup").Body.Bytes(), &st)
	if st.Product != "Softop Mails" || st.Brand.Name != "Softop Mails" || st.Brand.Accent != "#be123c" || st.Brand.Vars["--b-accent"] != "#be123c" || st.Brand.Icon != "be123c1" {
		t.Fatalf("setup status: %+v", st)
	}
	// admin settings: validated
	c := loginClient(admin.Email, "password-asi")
	for _, bad := range []map[string]any{{"app_name": ""}, {"app_name": "<b>"}, {"app_name": strings.Repeat("x", 41)}, {"accent": "blue"}, {"accent": "#12345"}, {"accent": "#123456;x"}} {
		if code := c.do("PUT", "/api/admin/settings", bad, nil); code != 400 {
			t.Fatalf("bad settings %v accepted: %d", bad, code)
		}
	}
	var out map[string]any
	code := c.do("PUT", "/api/admin/settings", map[string]any{"app_name": "  Zupnex   Mail ", "accent": "#0F766E"}, &out)
	if code != 200 || out["app_name"] != "Zupnex Mail" || out["accent"] != "#0f766e" {
		t.Fatalf("settings: %d %v", code, out)
	}
	if app.AppName() != "Zupnex Mail" || app.Accent() != "#0f766e" {
		t.Fatal("not saved")
	}
	// a non-admin can't change it
	cu := loginClient(user.Email, "password-sajin")
	if code := cu.do("PUT", "/api/admin/settings", map[string]any{"app_name": "Hacked"}, nil); code != 403 {
		t.Fatalf("user changed settings: %d", code)
	}
	// header-safe name: no comment breakers, ASCII only
	app.Settings.Set(map[string]string{SetAppName: "Café (Tést) Mails"})
	if got := app.headerName(); got != "Caf Tst Mails" {
		t.Fatalf("headerName %q", got)
	}
	app.Settings.Set(map[string]string{SetAppName: "日本"})
	if got := app.headerName(); got != genericAppName {
		t.Fatalf("headerName %q", got)
	}
	// the export folder follows the name and is a safe path
	app.Settings.Set(map[string]string{SetAppName: "Acme: Mails?"})
	if got := app.exportTop(); got != "Acme_ Mails_ export" {
		t.Fatalf("exportTop %q", got)
	}
}

func TestSetupBranding(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	h, err := app.handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(t, srv.URL)
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"token": app.setupToken, "org_name": "Softop", "domain": "softop.test", "hostname": "mail.softop.test",
			"name": "Boss", "local": "boss", "password": "supersecret1"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	// refused before anything is created
	for _, bad := range []map[string]any{{"accent": "red"}, {"accent": "#12345g"}, {"app_name": "<b>Softop</b>"}, {"app_name": strings.Repeat("x", 41)}, {"app_name": "a​b"}} {
		if code := c.do("POST", "/api/setup", base(bad), nil); code != 400 {
			t.Fatalf("setup with %v: %d", bad, code)
		}
	}
	if app.SetupDone() || app.Settings.Get(SetAppName) != "" || app.Domain("softop.test") != nil {
		t.Fatal("a refused setup left something behind")
	}
	if code := c.do("POST", "/api/setup", base(map[string]any{"app_name": "  Softop   Mails ", "accent": "#BE123C"}), nil); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	if app.AppName() != "Softop Mails" || app.Accent() != "#be123c" || app.Settings.Get(SetAppName) != "Softop Mails" {
		t.Fatalf("saved %q %q", app.AppName(), app.Accent())
	}
	var me struct {
		Product string
		Brand   struct{ Name, Accent string }
	}
	c.do("GET", "/api/me", nil, &me)
	if me.Product != "Softop Mails" || me.Brand.Name != "Softop Mails" || me.Brand.Accent != "#be123c" {
		t.Fatalf("me: %+v", me)
	}
	if code := c.do("POST", "/api/setup", base(nil), nil); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
}

func TestSetupDefaultsName(t *testing.T) {
	app, _, _, _ := newTestApp(t, false)
	h, _ := app.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	org := "The Very Long Organisation Name Pvt Ltd" // 39 characters: "+ Mails" would be too long
	if code := newClient(t, srv.URL).do("POST", "/api/setup", map[string]any{"token": app.setupToken, "org_name": org, "domain": "long.test",
		"hostname": "mail.long.test", "name": "Boss", "local": "boss", "password": "supersecret1"}, nil); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	if app.AppName() != org || app.Accent() != DefaultAccent {
		t.Fatalf("defaults %q %q", app.AppName(), app.Accent())
	}
}

func TestUserAgentIsAppName(t *testing.T) {
	raw, _, err := BuildMessage(Compose{From: Addr{Email: "a@x.test"}, To: []Addr{{Email: "b@y.test"}}, Subject: "s", HTML: "<p>x</p>", Agent: "Softop Mails"}, "mail.x.test")
	if err != nil || !bytes.Contains(raw, []byte("User-Agent: Softop Mails\r\n")) {
		t.Fatalf("%v %s", err, raw[:200])
	}
	raw, _, _ = BuildMessage(Compose{From: Addr{Email: "a@x.test"}, To: []Addr{{Email: "b@y.test"}}, Subject: "s", HTML: "<p>x</p>"}, "mail.x.test")
	if bytes.Contains(raw, []byte("User-Agent")) {
		t.Fatal("empty agent still written")
	}
}
