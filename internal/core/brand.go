package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Branding: each server shows its own name ("Softop Mails") and main colour. Only the
// copyright line in the web app is fixed.

const (
	DefaultAccent = "#4453f2"
	maxAppNameLen = 40
	// genericAppName is shown before the organisation has been set up.
	genericAppName = "Mail"
)

// AccentPresets are offered in setup and settings; any other #rrggbb works too.
var AccentPresets = []string{"#4453f2", "#0b57d0", "#0f766e", "#15803d", "#b45309", "#c2410c", "#be123c", "#7c3aed", "#334155"}

var reHexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// AppName is the name shown in the web app, notifications and generated messages.
func (a *App) AppName() string {
	if n := a.Settings.Get(SetAppName); n != "" {
		return n
	}
	if o := a.Settings.Get(SetOrgName); o != "" {
		return defaultAppName(o)
	}
	return genericAppName
}

// defaultAppName suggests a name for an organisation: "Softop" -> "Softop Mails".
func defaultAppName(org string) string {
	org = strings.TrimSpace(org)
	if org == "" {
		return genericAppName
	}
	n := org + " Mails"
	if utf8.RuneCountInString(n) > maxAppNameLen {
		n = truncateRunes(org, maxAppNameLen)
	}
	return strings.TrimSpace(n)
}

// Accent is the main colour, always a valid #rrggbb.
func (a *App) Accent() string {
	if c, ok := normalizeAccent(a.Settings.Get(SetAccent)); ok {
		return c
	}
	return DefaultAccent
}

// cleanAppName validates a name typed by an administrator.
func cleanAppName(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ") // no line breaks, tabs or double spaces
	if s == "" {
		return "", fmt.Errorf("Enter a name for the app, like “Softop Mails”")
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxAppNameLen {
		return "", fmt.Errorf("Keep the app name to %d characters or fewer", maxAppNameLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == '<' || r == '>' || r == '"' || r == '\\' || r == '/' || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("The app name can't contain < > \" / or \\")
		}
	}
	return s, nil
}

// normalizeAccent accepts #rrggbb (any case) and returns it in lower case.
func normalizeAccent(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !reHexColor.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// ---------------------------------------------------------------- colours

type rgb struct{ r, g, b float64 } // 0..1, sRGB

func parseHex(s string) rgb {
	var r, g, b int
	fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b)
	return rgb{float64(r) / 255, float64(g) / 255, float64(b) / 255}
}

func (c rgb) hex() string {
	f := func(v float64) int { return int(math.Round(math.Max(0, math.Min(1, v)) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", f(c.r), f(c.g), f(c.b))
}

func (c rgb) rgba() color.NRGBA {
	f := func(v float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, v)) * 255)) }
	return color.NRGBA{f(c.r), f(c.g), f(c.b), 255}
}

// mix returns c moved towards o by t (0 = c, 1 = o).
func (c rgb) mix(o rgb, t float64) rgb {
	return rgb{c.r + (o.r-c.r)*t, c.g + (o.g-c.g)*t, c.b + (o.b-c.b)*t}
}

func (c rgb) luminance() float64 {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := a.luminance()+0.05, b.luminance()+0.05
	if la < lb {
		la, lb = lb, la
	}
	return la / lb
}

var (
	white = rgb{1, 1, 1}
	black = rgb{0, 0, 0}
)

// towards moves c towards o in small steps until it has at least min contrast against bg.
func towards(c, o, bg rgb, min float64) rgb {
	ok := func(c rgb) bool { return contrast(parseHex(c.hex()), bg) >= min } // as written out (#rrggbb)
	for i := 0; i < 40 && !ok(c); i++ {
		c = c.mix(o, 0.06)
	}
	for i := 0; i < 20 && !ok(c); i++ { // the last steps, finer
		c = c.mix(o, 0.02)
	}
	return parseHex(c.hex())
}

// withText adjusts a fill colour slightly, if needed, so white or near-black text on it is
// comfortably readable (4.5:1); most colours are left exactly as they are.
func withText(c rgb) rgb {
	if ink := readableOn(c); contrast(ink, c) < 4.5 {
		if ink == white {
			return towards(c, black, white, 4.5)
		}
		return towards(c, white, ink, 4.5)
	}
	return c
}

// visibleFill is the main colour as a filled shape: very light colours are darkened just enough
// to stand out from a white page (and from the icon's surroundings).
func visibleFill(c rgb) rgb { return towards(c, black, white, 1.5) }

// readableOn picks white or near-black text for a coloured background.
func readableOn(bg rgb) rgb {
	ink := rgb{0.07, 0.08, 0.12}
	if contrast(white, bg) >= contrast(ink, bg) {
		return white
	}
	return ink
}

// brandVars derives the colour tokens for light and dark mode from the main colour, keeping
// text readable whatever colour was chosen. Light tokens are --b-*, dark ones --bd-*.
func brandVars(accent string) map[string]string {
	c := parseHex(accent)
	darkBg := black
	v := map[string]string{}

	// light mode
	fill := withText(visibleFill(c))         // Compose button, unread avatar: must show on a white page
	primary := towards(c, black, white, 4.5) // links, filled buttons, selected text
	container := towards(c.mix(white, 0.86), fill, white, 1.12)
	v["--b-accent"] = fill.hex()
	v["--b-on-accent"] = readableOn(fill).hex()
	v["--b-primary"] = primary.hex()
	v["--b-primary-hover"] = primary.mix(black, 0.14).hex()
	v["--b-primary-strong"] = towards(primary, black, parseHex("#e4e4e7"), 4.5).hex() // on the light snackbar (dark mode)
	v["--b-on-primary"] = white.hex()
	v["--b-primary-container"] = container.hex()
	v["--b-on-primary-container"] = towards(c.mix(black, 0.35), black, container, 7).hex()
	v["--b-selected"] = towards(c.mix(white, 0.88), fill, white, 1.1).hex()
	v["--b-soft"] = towards(c.mix(white, 0.94), fill, white, 1.04).hex()

	// dark mode (true black)
	dAccent := withText(towards(c, white, darkBg, 3))
	dPrimary := towards(c, white, darkBg, 7)
	dContainer := c.mix(black, 0.7)
	if contrast(dContainer, darkBg) < 1.25 {
		dContainer = dAccent.mix(black, 0.62)
	}
	v["--bd-accent"] = dAccent.hex()
	v["--bd-on-accent"] = readableOn(dAccent).hex()
	v["--bd-primary"] = dPrimary.hex()
	v["--bd-primary-hover"] = dPrimary.mix(white, 0.25).hex()
	v["--bd-on-primary"] = towards(c.mix(black, 0.75), black, dPrimary, 4.5).hex()
	v["--bd-primary-container"] = dContainer.hex()
	v["--bd-on-primary-container"] = towards(c.mix(white, 0.8), white, dContainer, 7).hex()
	v["--bd-selected"] = dAccent.mix(black, 0.74).hex()
	v["--bd-soft"] = dAccent.mix(black, 0.86).hex()
	return v
}

// brandCSS is the :root rule with the colour tokens, put after the stylesheet.
func brandCSS(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(":root{")
	for _, k := range keys {
		b.WriteString(k + ":" + vars[k] + ";")
	}
	b.WriteString("}")
	return b.String()
}

// brandInfo is what the web app needs to show this server's name and colours.
func (a *App) brandInfo() map[string]any {
	acc := a.Accent()
	return map[string]any{"name": a.AppName(), "accent": acc, "vars": brandVarsFor(acc), "icon": iconKey(acc)}
}

// iconKey changes whenever the icon would look different, so browsers fetch the new one: the
// colour plus the version of the drawing below (bump iconDrawing when the drawing changes).
const iconDrawing = "1"

func iconKey(accent string) string { return strings.TrimPrefix(accent, "#") + iconDrawing }

// ---------------------------------------------------------------- icons
//
// The app icon is drawn in the main colour: an envelope on a rounded square. Drawn here
// (signed-distance shapes, 4×4 supersampling) so every server gets icons in its own colour.

type iconSpec struct {
	size     int
	rounded  bool    // transparent corners (otherwise a full square, for maskable/Apple icons)
	scale    float64 // envelope size relative to the standard layout
	maskable bool
	badge    bool // white envelope on transparent (Android's small notification icon)
}

var iconSpecs = map[string]iconSpec{
	"icon-32.png":      {32, true, 1, false, false},
	"icon-192.png":     {192, true, 1, false, false},
	"icon-512.png":     {512, true, 1, false, false},
	"apple-touch.png":  {180, false, 1.08, false, false},
	"maskable-512.png": {512, false, 0.8, true, false},
	"badge-96.png":     {96, false, 1.5, false, true},
}

func sdRoundRect(px, py, x0, y0, x1, y1, r float64) float64 {
	cx, cy := (x0+x1)/2, (y0+y1)/2
	qx := math.Abs(px-cx) - (x1-x0)/2 + r
	qy := math.Abs(py-cy) - (y1-y0)/2 + r
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

func sdSegment(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// drawIcon renders the icon on a 64×64 design grid scaled to spec.size.
func drawIcon(accent string, spec iconSpec) []byte {
	c := visibleFill(parseHex(accent))
	bg, ink := c, readableOn(c)
	n := spec.size
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	const ss = 4
	k := 64 / float64(n)
	// envelope geometry around the centre (32, 32)
	s := spec.scale
	ex0, ey0, ex1, ey1 := 32-18*s, 32-12.5*s, 32+18*s, 32+12.5*s
	er := 4.5 * s
	fw := 1.9 * s // half the flap's stroke width
	fax, fay := 32-15*s, 32-9*s
	fmx, fmy := 32.0, 32+2.5*s
	fbx, fby := 32+15*s, 32-9*s
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var r, g, b, al float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/ss) * k
					py := (float64(y) + (float64(sy)+0.5)/ss) * k
					if spec.rounded && sdRoundRect(px, py, 0, 0, 64, 64, 14) > 0 {
						continue // transparent corner
					}
					if spec.badge {
						inEnv := sdRoundRect(px, py, ex0, ey0, ex1, ey1, er) <= 0
						if !inEnv || math.Min(sdSegment(px, py, fax, fay, fmx, fmy), sdSegment(px, py, fmx, fmy, fbx, fby)) <= fw {
							continue
						}
						r, g, b, al = r+1, g+1, b+1, al+1
						continue
					}
					col := bg
					if sdRoundRect(px, py, ex0, ey0, ex1, ey1, er) <= 0 {
						col = ink
						if math.Min(sdSegment(px, py, fax, fay, fmx, fmy), sdSegment(px, py, fmx, fmy, fbx, fby)) <= fw {
							col = bg
						}
					}
					r += col.r
					g += col.g
					b += col.b
					al++
				}
			}
			if al == 0 {
				continue
			}
			px := rgb{r / al, g / al, b / al}.rgba()
			px.A = uint8(math.Round(al / (ss * ss) * 255))
			img.SetNRGBA(x, y, px)
		}
	}
	var buf bytes.Buffer
	(&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img)
	return buf.Bytes()
}

// iconSVG is the same icon as a vector, used as the favicon and the logo in the app.
func iconSVG(accent string) []byte {
	c := visibleFill(parseHex(accent))
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`+
		`<rect width="64" height="64" rx="14" fill="%s"/>`+
		`<rect x="14" y="19.5" width="36" height="25" rx="4.5" fill="%s"/>`+
		`<path d="M17 23 32 34.5 47 23" fill="none" stroke="%s" stroke-width="3.8" stroke-linecap="round" stroke-linejoin="round"/>`+
		`</svg>`, c.hex(), readableOn(c).hex(), c.hex()))
}

type brandAsset struct {
	body  []byte
	etag  string
	ctype string
}

var brandCache = struct {
	sync.Mutex
	m map[string]*brandAsset // accent + "/" + file
}{m: map[string]*brandAsset{}}

func brandFile(accent, file string) *brandAsset {
	key := accent + "/" + file
	brandCache.Lock()
	defer brandCache.Unlock()
	if a := brandCache.m[key]; a != nil {
		return a
	}
	var body []byte
	ctype := "image/png"
	if file == "mark.svg" {
		body, ctype = iconSVG(accent), "image/svg+xml"
	} else if spec, ok := iconSpecs[file]; ok {
		body = drawIcon(accent, spec)
	} else {
		return nil
	}
	if len(brandCache.m) > 64 { // the colour changes rarely; don't keep old ones forever
		brandCache.m = map[string]*brandAsset{}
	}
	sum := sha256.Sum256(body)
	a := &brandAsset{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ctype}
	brandCache.m[key] = a
	return a
}

// serveBrand serves /brand/<file> (and /favicon.ico) in the current main colour.
func (a *App) serveBrand(w http.ResponseWriter, r *http.Request) bool {
	file := strings.TrimPrefix(r.URL.Path, "/brand/")
	if r.URL.Path == "/favicon.ico" {
		file = "icon-32.png"
	} else if !strings.HasPrefix(r.URL.Path, "/brand/") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	acc := a.Accent()
	f := brandFile(acc, file)
	if f == nil {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	if r.URL.Query().Get("c") == iconKey(acc) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	if r.Method == http.MethodHead {
		return true
	}
	w.Write(f.body)
	return true
}

// headerName is the app name for mail headers and certificates: printable ASCII only, and
// nothing that would end a header comment.
func (a *App) headerName() string {
	var b strings.Builder
	for _, r := range a.AppName() {
		if r >= 0x20 && r < 0x7f && r != '(' && r != ')' && r != '\\' && r != '"' {
			b.WriteRune(r)
		}
	}
	if s := strings.Join(strings.Fields(b.String()), " "); s != "" {
		return s
	}
	return genericAppName
}

// The colour tokens and filled-in pages are worked out once per name and colour, not on every
// request (they're on the path of every page load and /api/me).
var brandMemo = struct {
	sync.Mutex
	vars  map[string]map[string]string // accent -> tokens (shared: read only)
	pages map[string]*renderedPage     // path|name|accent
}{vars: map[string]map[string]string{}, pages: map[string]*renderedPage{}}

type renderedPage struct {
	body, gz []byte
	etag     string
}

// brandVarsFor is brandVars, remembered. The map it returns must not be changed.
func brandVarsFor(accent string) map[string]string {
	brandMemo.Lock()
	defer brandMemo.Unlock()
	if v := brandMemo.vars[accent]; v != nil {
		return v
	}
	if len(brandMemo.vars) > 32 {
		brandMemo.vars = map[string]map[string]string{}
	}
	v := brandVars(accent)
	brandMemo.vars[accent] = v
	return v
}

// renderPage fills this server's name and colours into index.html and the web app manifest.
func (a *App) renderPage(p string, tmpl []byte) ([]byte, string, []byte) {
	name, acc := a.AppName(), a.Accent()
	key := p + "|" + name + "|" + acc
	brandMemo.Lock()
	rp := brandMemo.pages[key]
	brandMemo.Unlock()
	if rp == nil {
		var r *strings.Replacer
		switch p {
		case "/index.html":
			r = strings.NewReplacer("{{PRODUCT}}", html.EscapeString(name), "{{BRAND_CSS}}", brandCSS(brandVarsFor(acc)), "{{ICON}}", iconKey(acc))
		case "/manifest.webmanifest":
			j, _ := json.Marshal(name)
			r = strings.NewReplacer("{{PRODUCT}}", string(j[1:len(j)-1]), "{{ICON}}", iconKey(acc)) // without the quotes
		default:
			r = strings.NewReplacer()
		}
		body := []byte(r.Replace(string(tmpl)))
		sum := sha256.Sum256(body)
		rp = &renderedPage{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, gz: gzipped(body)}
		brandMemo.Lock()
		if len(brandMemo.pages) > 32 {
			brandMemo.pages = map[string]*renderedPage{}
		}
		brandMemo.pages[key] = rp
		brandMemo.Unlock()
	}
	return rp.body, rp.etag, rp.gz
}

// GET /api/brand/preview?accent=%23rrggbb — the colour tokens for a colour being picked (setup
// and settings preview it before saving). Works out colours only; changes nothing.
func (a *App) handleBrandPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := normalizeAccent(r.URL.Query().Get("accent"))
	if !ok {
		httpError(w, http.StatusBadRequest, "Choose a colour like #4453f2")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accent": c, "vars": brandVars(c)})
}
