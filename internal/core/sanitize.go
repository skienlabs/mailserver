package core

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
)

var (
	policyOnce sync.Once
	policy     *bluemonday.Policy
)

// emailPolicy allows the formatting real-world email relies on (tables, inline
// styles, <style> blocks) while removing scripts, event handlers, forms,
// iframes and dangerous URL schemes. Rendered output is additionally isolated
// in a sandboxed iframe with a restrictive CSP in the browser.
func emailPolicy() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.UGCPolicy()
		p.AllowElements("style", "center", "font", "span", "div", "section", "header", "footer", "article",
			"main", "nav", "figure", "figcaption", "small", "big", "mark", "u", "s", "strike", "del", "ins", "wbr")
		p.AllowUnsafe(true) // keep <style> contents; scripts are still stripped
		p.AllowAttrs("style", "class", "align", "valign", "bgcolor", "width", "height", "border", "dir", "title", "lang").Globally()
		p.AllowAttrs("cellpadding", "cellspacing", "colspan", "rowspan", "nowrap", "background").OnElements("table", "td", "th", "tr", "tbody", "thead", "tfoot")
		p.AllowAttrs("color", "face", "size").OnElements("font")
		p.AllowAttrs("media", "type").OnElements("style")
		p.AllowStyles("color", "background-color", "background", "background-image", "background-repeat", "background-position", "background-size",
			"font", "font-family", "font-size", "font-weight", "font-style", "font-variant", "line-height", "letter-spacing",
			"text-align", "text-decoration", "text-transform", "text-indent", "vertical-align", "white-space", "word-break", "word-wrap", "overflow-wrap",
			"width", "height", "max-width", "min-width", "max-height", "min-height",
			"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
			"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
			"border", "border-top", "border-right", "border-bottom", "border-left", "border-color", "border-style", "border-width",
			"border-radius", "border-collapse", "border-spacing", "display", "float", "clear", "overflow", "table-layout",
			"list-style", "list-style-type", "direction", "opacity", "box-sizing", "mso-line-height-rule").Globally()
		p.AllowURLSchemes("http", "https", "mailto", "tel", "cid")
		p.AllowDataURIImages()
		p.RequireNoReferrerOnLinks(true)
		p.AddTargetBlankToFullyQualifiedLinks(true)
		policy = p
	})
	return policy
}

var reCidSrc = regexp.MustCompile(`(?i)(src|background)="cid:([^"]+)"`)

// SanitizeHTML cleans untrusted HTML. When msgID > 0, cid: references are
// rewritten to the inline-attachment endpoint for that message.
func SanitizeHTML(s string, msgID int64) string {
	out := emailPolicy().Sanitize(s)
	if msgID > 0 {
		out = reCidSrc.ReplaceAllStringFunc(out, func(m string) string {
			sm := reCidSrc.FindStringSubmatch(m)
			cid := strings.ReplaceAll(sm[2], "&amp;", "&")
			return fmt.Sprintf(`%s="/api/messages/%d/cid/%s"`, sm[1], msgID, url.PathEscape(cid))
		})
	}
	return out
}
