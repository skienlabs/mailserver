package core

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/textproto"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/emersion/go-message/charset" // register legacy charsets (ISO-8859-x, Windows-125x, ...)
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

// Addr is a display-name + address pair used in JSON.
type Addr struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

func (a Addr) String() string {
	if a.Name == "" {
		return a.Email
	}
	return (&mail.Address{Name: a.Name, Address: a.Email}).String()
}

// Meta is the summary of a message stored in the database.
type Meta struct {
	MessageID     string
	InReplyTo     string
	Refs          []string
	From          Addr
	To, Cc        []Addr
	ReplyTo       string
	Subject       string
	Date          time.Time
	Text          string // plain-text body (for snippet / search)
	HasAttach     bool
	ListID        string
	Precedence    string
	AutoSubmitted string
}

// Attachment describes a non-body part.
type Attachment struct {
	Index     int    `json:"index"`
	Filename  string `json:"filename"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	ContentID string `json:"cid,omitempty"`
	Inline    bool   `json:"inline"`
}

// Body is the displayable content of a message.
type Body struct {
	HTML        string
	Text        string
	Attachments []Attachment
}

const maxBodyBytes = 4 << 20 // cap per text part we keep in memory for display

func decodeAddrs(h *mail.Header, key string) []Addr {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		// Fall back to a lenient parse of raw text
		raw := h.Get(key)
		if raw == "" {
			return nil
		}
		var out []Addr
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if a, err := mail.ParseAddress(part); err == nil {
				out = append(out, Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
			} else if i, j := strings.LastIndexByte(part, '<'), strings.LastIndexByte(part, '>'); i >= 0 && j > i {
				out = append(out, Addr{Name: strings.Trim(strings.TrimSpace(part[:i]), `"`), Email: strings.ToLower(part[i+1 : j])})
			} else if strings.Contains(part, "@") {
				out = append(out, Addr{Email: strings.ToLower(part)})
			}
		}
		return out
	}
	out := make([]Addr, 0, len(list))
	for _, a := range list {
		out = append(out, Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
	}
	return out
}

func cleanMsgID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return s
}

// partInfo classifies a leaf part.
type partInfo struct {
	ctype    string
	params   map[string]string
	disp     string
	filename string
	cid      string
}

func classify(h textproto.MIMEHeader, ctypeRaw, dispRaw string) partInfo {
	var p partInfo
	ct, params, err := mime.ParseMediaType(ctypeRaw)
	if err != nil || ct == "" {
		ct = "text/plain"
	}
	p.ctype, p.params = strings.ToLower(ct), params
	if dispRaw != "" {
		d, dparams, err := mime.ParseMediaType(dispRaw)
		if err == nil {
			p.disp = strings.ToLower(d)
			p.filename = dparams["filename"]
		} else {
			p.disp = strings.ToLower(strings.TrimSpace(strings.SplitN(dispRaw, ";", 2)[0]))
		}
	}
	if p.filename == "" {
		p.filename = params["name"]
	}
	if p.filename != "" {
		dec := new(mime.WordDecoder)
		if s, err := dec.DecodeHeader(p.filename); err == nil {
			p.filename = s
		}
	}
	p.cid = cleanMsgID(h.Get("Content-Id"))
	return p
}

func (p partInfo) isBody() bool {
	return (p.ctype == "text/plain" || p.ctype == "text/html") && p.disp != "attachment" && p.filename == ""
}

// MIME complexity limits. Parsing cost grows quadratically with nesting depth, so
// absurdly nested messages are refused at SMTP time and never walked when stored.
const (
	maxMultipartEntities = 40
	maxMIMEParts         = 1000
)

// MIMETooComplex reports whether raw nests multiparts or parts beyond sane limits.
// It is a linear scan of header lines (an upper bound on the real structure).
func MIMETooComplex(raw []byte) bool {
	multipart, parts := 0, 0
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		var line []byte
		if i < 0 {
			line, raw = raw, nil
		} else {
			line, raw = raw[:i], raw[i+1:]
		}
		if len(line) < 13 || (line[0] != 'C' && line[0] != 'c') {
			continue
		}
		// The parser tolerates whitespace between a field name and its colon ("Content-Type :"),
		// so match the same way or the limits could be sidestepped.
		colon := bytes.IndexByte(line, ':')
		if colon < 12 || !bytes.EqualFold(bytes.TrimRight(line[:colon], " \t"), []byte("content-type")) {
			continue
		}
		parts++
		val := line[colon+1:]
		if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') { // folded value
			if j := bytes.IndexByte(raw, '\n'); j > 0 {
				val = append(append([]byte{}, val...), raw[:j]...)
			}
		}
		if bytes.Contains(bytes.ToLower(val), []byte("multipart/")) {
			multipart++
		}
		if multipart > maxMultipartEntities || parts > maxMIMEParts {
			return true
		}
	}
	return false
}

const tooComplexNotice = "[This message has an unusually complex structure and can't be displayed. Download the original (.eml) to inspect it.]"

// walkParts iterates over leaf parts of a message. fn returns false to stop.
// Attachment indexes are stable across calls for the same raw message.
func walkParts(raw []byte, fn func(info partInfo, index int, body io.Reader) bool) (*mail.Header, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return nil, err
	}
	h := &mr.Header
	if MIMETooComplex(raw) {
		fn(partInfo{ctype: "text/plain"}, -1, strings.NewReader(tooComplexNotice))
		return h, nil
	}
	idx := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if p == nil {
			// unknown encoding or malformed multipart — keep what we have
			break
		}
		var ctypeRaw, dispRaw string
		var mh textproto.MIMEHeader
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ctypeRaw, dispRaw = ph.Get("Content-Type"), ph.Get("Content-Disposition")
			mh = textproto.MIMEHeader(ph.Map())
		case *mail.AttachmentHeader:
			ctypeRaw, dispRaw = ph.Get("Content-Type"), ph.Get("Content-Disposition")
			mh = textproto.MIMEHeader(ph.Map())
		}
		info := classify(mh, ctypeRaw, dispRaw)
		i := -1
		if !info.isBody() {
			i = idx
			idx++
		}
		if !fn(info, i, p.Body) {
			break
		}
	}
	return h, nil
}

func readLimited(r io.Reader, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// SingleFrom returns the only author of a message, or an error when the message has
// zero or several From headers, or a From header listing several mailboxes.
func SingleFrom(raw []byte) (Addr, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return Addr{}, err
	}
	h := mr.Header
	if n := len(h.Values("From")); n != 1 {
		return Addr{}, fmt.Errorf("message must have exactly one From header (found %d)", n)
	}
	list, err := h.AddressList("From")
	if err != nil || len(list) != 1 {
		return Addr{}, fmt.Errorf("the From header must contain exactly one address")
	}
	return Addr{Name: list[0].Name, Email: strings.ToLower(list[0].Address)}, nil
}

// HeaderAddresses returns every address in all occurrences of a header.
func HeaderAddresses(raw []byte, key string) ([]Addr, int) {
	mr, _ := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return nil, 0
	}
	n := len(mr.Header.Values(key))
	return decodeAddrs(&mr.Header, key), n
}

// ParseMeta extracts the summary fields used for listing and search.
func ParseMeta(raw []byte) Meta {
	var m Meta
	var texts, htmls []string
	h, _ := walkParts(raw, func(info partInfo, index int, body io.Reader) bool {
		if index < 0 {
			if info.ctype == "text/html" {
				htmls = append(htmls, readLimited(body, maxBodyBytes))
			} else {
				texts = append(texts, readLimited(body, maxBodyBytes))
			}
			return true
		}
		if !(info.disp == "inline" && info.cid != "" && strings.HasPrefix(info.ctype, "image/")) {
			m.HasAttach = true
		}
		return true
	})
	if h == nil {
		// Unparseable: fall back to treating everything as text.
		m.Text = readLimited(bytes.NewReader(raw), 64*1024)
		m.Date = time.Now()
		return m
	}
	if mid, err := h.MessageID(); err == nil {
		m.MessageID = mid
	} else {
		m.MessageID = cleanMsgID(h.Get("Message-Id"))
	}
	if ids, err := h.MsgIDList("In-Reply-To"); err == nil && len(ids) > 0 {
		m.InReplyTo = ids[0]
	}
	if ids, err := h.MsgIDList("References"); err == nil {
		m.Refs = ids
	}
	if from := decodeAddrs(h, "From"); len(from) > 0 {
		m.From = from[0]
	}
	m.To = decodeAddrs(h, "To")
	m.Cc = decodeAddrs(h, "Cc")
	if rt := decodeAddrs(h, "Reply-To"); len(rt) > 0 {
		m.ReplyTo = rt[0].Email
	}
	if s, err := h.Subject(); err == nil {
		m.Subject = s
	} else {
		m.Subject = h.Get("Subject")
	}
	m.Subject = strings.TrimSpace(strings.ToValidUTF8(m.Subject, ""))
	if d, err := h.Date(); err == nil && !d.IsZero() {
		m.Date = d
	} else {
		m.Date = time.Now()
	}
	m.ListID = h.Get("List-Id")
	m.Precedence = strings.ToLower(h.Get("Precedence"))
	m.AutoSubmitted = strings.ToLower(h.Get("Auto-Submitted"))
	if len(texts) > 0 {
		m.Text = strings.Join(texts, "\n\n")
	} else if len(htmls) > 0 {
		m.Text = HTMLToText(strings.Join(htmls, "\n"))
	}
	return m
}

// ParseBody extracts what the reader view needs.
func ParseBody(raw []byte) Body {
	var b Body
	var texts, htmls []string
	walkParts(raw, func(info partInfo, index int, body io.Reader) bool {
		if index < 0 {
			if info.ctype == "text/html" {
				htmls = append(htmls, readLimited(body, maxBodyBytes))
			} else {
				texts = append(texts, readLimited(body, maxBodyBytes))
			}
			return true
		}
		n, _ := io.Copy(io.Discard, body)
		name := info.filename
		if name == "" {
			name = defaultFilename(info.ctype, index)
		}
		b.Attachments = append(b.Attachments, Attachment{
			Index: index, Filename: name, Type: info.ctype, Size: n, ContentID: info.cid,
			Inline: info.cid != "" && strings.HasPrefix(info.ctype, "image/") && info.disp != "attachment",
		})
		return true
	})
	b.Text = strings.Join(texts, "\n\n")
	b.HTML = strings.Join(htmls, "\n")
	if b.Attachments == nil {
		b.Attachments = []Attachment{}
	}
	return b
}

func defaultFilename(ctype string, idx int) string {
	if ctype == "message/rfc822" {
		return "message.eml"
	}
	ext := ""
	if exts, _ := mime.ExtensionsByType(ctype); len(exts) > 0 {
		ext = exts[0]
	}
	return fmt.Sprintf("attachment-%d%s", idx+1, ext)
}

// FindAttachment streams the attachment at index (or with Content-ID cid) into w.
func FindAttachment(raw []byte, index int, cid string) (Attachment, []byte, bool) {
	var found Attachment
	var data []byte
	ok := false
	walkParts(raw, func(info partInfo, i int, body io.Reader) bool {
		if i < 0 {
			return true
		}
		if (cid == "" && i == index) || (cid != "" && strings.EqualFold(info.cid, cid)) {
			data, _ = io.ReadAll(body)
			name := info.filename
			if name == "" {
				name = defaultFilename(info.ctype, i)
			}
			found = Attachment{Index: i, Filename: name, Type: info.ctype, Size: int64(len(data)), ContentID: info.cid}
			ok = true
			return false
		}
		return true
	})
	return found, data, ok
}

// ---------------------------------------------------------------- HTML → text

var reSpaces = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
var reBlankLines = regexp.MustCompile(`\n{3,}`)

var blockTags = map[string]bool{"p": true, "div": true, "br": true, "tr": true, "li": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "table": true, "blockquote": true, "pre": true, "hr": true,
	"section": true, "article": true, "header": true, "footer": true, "ul": true, "ol": true}

// HTMLToText converts HTML to readable plain text.
func HTMLToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var sb strings.Builder
	skip := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			out := sb.String()
			lines := strings.Split(out, "\n")
			for i, l := range lines {
				lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(l, " "))
			}
			return strings.TrimSpace(reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "style" || tag == "script" || tag == "head" || tag == "title" {
				if tt == html.StartTagToken {
					skip++
				} else if tt == html.EndTagToken && skip > 0 {
					skip--
				}
				continue
			}
			if blockTags[tag] {
				sb.WriteByte('\n')
			}
			if tag == "li" && tt == html.StartTagToken {
				sb.WriteString("• ")
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(bytes.ReplaceAll(z.Text(), []byte("\n"), []byte(" ")))
			}
		}
	}
}

// Snippet returns a short single-line preview.
func Snippet(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	r := []rune(text)
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------- building

// OutgoingPart is an attachment to include in a composed message.
type OutgoingPart struct {
	Filename string
	Type     string
	Data     []byte
}

// Compose is a message to build.
type Compose struct {
	From       Addr
	To, Cc     []Addr
	ReplyTo    string
	Subject    string
	HTML       string // already sanitised
	Text       string // optional; derived from HTML when empty
	InReplyTo  string
	References []string
	Extra      map[string]string // extra headers (Auto-Submitted, ...)
	Files      []OutgoingPart
	MessageID  string
	Agent      string // User-Agent: the app name
}

func toMailAddrs(list []Addr) []*mail.Address {
	out := make([]*mail.Address, 0, len(list))
	for _, a := range list {
		out = append(out, &mail.Address{Name: a.Name, Address: a.Email})
	}
	return out
}

// BuildMessage renders c as an RFC 5322 message.
func BuildMessage(c Compose, hostname string) ([]byte, string, error) {
	var h mail.Header
	h.SetDate(time.Now())
	h.SetAddressList("From", toMailAddrs([]Addr{c.From}))
	if len(c.To) > 0 {
		h.SetAddressList("To", toMailAddrs(c.To))
	}
	if len(c.Cc) > 0 {
		h.SetAddressList("Cc", toMailAddrs(c.Cc))
	}
	if c.ReplyTo != "" {
		h.SetAddressList("Reply-To", []*mail.Address{{Address: c.ReplyTo}})
	}
	h.SetSubject(c.Subject)
	mid := c.MessageID
	if mid == "" {
		mid = fmt.Sprintf("%s.%s@%s", time.Now().UTC().Format("20060102150405"), RandomHex(8), hostname)
	}
	h.SetMessageID(mid)
	if c.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{c.InReplyTo})
	}
	if len(c.References) > 0 {
		h.SetMsgIDList("References", c.References)
	}
	h.Set("MIME-Version", "1.0")
	if c.Agent != "" {
		h.Set("User-Agent", c.Agent)
	}
	for k, v := range c.Extra {
		h.Set(k, v)
	}
	text := c.Text
	if text == "" && c.HTML != "" {
		text = HTMLToText(c.HTML)
	}
	var buf bytes.Buffer
	writeAlt := func(iw *mail.InlineWriter) error {
		var th mail.InlineHeader
		th.Set("Content-Type", "text/plain; charset=utf-8")
		w, err := iw.CreatePart(th)
		if err != nil {
			return err
		}
		io.WriteString(w, text)
		w.Close()
		if c.HTML != "" {
			var hh mail.InlineHeader
			hh.Set("Content-Type", "text/html; charset=utf-8")
			w, err := iw.CreatePart(hh)
			if err != nil {
				return err
			}
			io.WriteString(w, wrapHTML(c.HTML))
			w.Close()
		}
		return iw.Close()
	}
	if len(c.Files) == 0 {
		if c.HTML == "" {
			var th mail.Header = h
			th.Set("Content-Type", "text/plain; charset=utf-8")
			w, err := mail.CreateSingleInlineWriter(&buf, th)
			if err != nil {
				return nil, "", err
			}
			io.WriteString(w, text)
			if err := w.Close(); err != nil {
				return nil, "", err
			}
			return buf.Bytes(), mid, nil
		}
		iw, err := mail.CreateInlineWriter(&buf, h)
		if err != nil {
			return nil, "", err
		}
		if err := writeAlt(iw); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), mid, nil
	}
	mw, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return nil, "", err
	}
	iw, err := mw.CreateInline()
	if err != nil {
		return nil, "", err
	}
	if err := writeAlt(iw); err != nil {
		return nil, "", err
	}
	for _, f := range c.Files {
		var ah mail.AttachmentHeader
		ct := f.Type
		if ct == "" {
			ct = "application/octet-stream"
		}
		ah.Set("Content-Type", ct)
		ah.SetFilename(f.Filename)
		w, err := mw.CreateAttachment(ah)
		if err != nil {
			return nil, "", err
		}
		w.Write(f.Data)
		w.Close()
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mid, nil
}

func wrapHTML(body string) string {
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>` +
		`<body><div style="font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.5;color:#1f1f1f">` + body + `</div></body></html>`
}
