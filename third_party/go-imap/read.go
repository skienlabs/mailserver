package imap

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
)

const (
	sp            = ' '
	cr            = '\r'
	lf            = '\n'
	dquote        = '"'
	literalStart  = '{'
	literalEnd    = '}'
	listStart     = '('
	listEnd       = ')'
	respCodeStart = '['
	respCodeEnd   = ']'
)

const (
	crlf    = "\r\n"
	nilAtom = "NIL"
)

// TODO: add CTL to atomSpecials
var (
	quotedSpecials = string([]rune{dquote, '\\'})
	respSpecials   = string([]rune{respCodeEnd})
	atomSpecials   = string([]rune{listStart, listEnd, literalStart, sp, '%', '*'}) + quotedSpecials + respSpecials
)

type parseError struct {
	error
}

func newParseError(text string) error {
	return &parseError{errors.New(text)}
}

// IsParseError returns true if the provided error is a parse error produced by
// Reader.
func IsParseError(err error) bool {
	_, ok := err.(*parseError)
	return ok
}

// A string reader.
type StringReader interface {
	// ReadString reads until the first occurrence of delim in the input,
	// returning a string containing the data up to and including the delimiter.
	// See https://golang.org/pkg/bufio/#Reader.ReadString
	ReadString(delim byte) (line string, err error)
}

type reader interface {
	io.Reader
	io.RuneScanner
	StringReader
}

// ParseNumber parses a number.
func ParseNumber(f interface{}) (uint32, error) {
	// Useful for tests
	if n, ok := f.(uint32); ok {
		return n, nil
	}

	var s string
	switch f := f.(type) {
	case RawString:
		s = string(f)
	case string:
		s = f
	default:
		return 0, newParseError("expected a number, got a non-atom")
	}

	nbr, err := strconv.ParseUint(string(s), 10, 32)
	if err != nil {
		return 0, &parseError{err}
	}

	return uint32(nbr), nil
}

// ParseString parses a string, which is either a literal, a quoted string or an
// atom.
func ParseString(f interface{}) (string, error) {
	if s, ok := f.(string); ok {
		return s, nil
	}

	// Useful for tests
	if a, ok := f.(RawString); ok {
		return string(a), nil
	}

	if l, ok := f.(Literal); ok {
		b := make([]byte, l.Len())
		if _, err := io.ReadFull(l, b); err != nil {
			return "", err
		}
		return string(b), nil
	}

	return "", newParseError("expected a string")
}

// Convert a field list to a string list.
func ParseStringList(f interface{}) ([]string, error) {
	fields, ok := f.([]interface{})
	if !ok {
		return nil, newParseError("expected a string list, got a non-list")
	}

	list := make([]string, len(fields))
	for i, f := range fields {
		var err error
		if list[i], err = ParseString(f); err != nil {
			return nil, newParseError("cannot parse string in string list: " + err.Error())
		}
	}
	return list, nil
}

func trimSuffix(str string, suffix rune) string {
	return str[:len(str)-1]
}

// An IMAP reader.
type Reader struct {
	MaxLiteralSize uint32 // The maximum literal size.

	// Skien Mails patch: ReserveLiteral, when set, is called before a literal is read (and
	// before the server asks for it). An error refuses it: as a parse error (BAD) when the
	// client is waiting for permission, as a connection error when it is already sending.
	ReserveLiteral func(n uint32) error

	// Skien Mails patch: limits on what one response may contain, so a hostile peer can't
	// make the parser use far more memory than it sent (see PATCHES.md). Zero = no limit.
	MaxFields  int // fields (including nested ones) in one response or command line
	MaxTextLen int // bytes in one atom, quoted string or status text
	MaxDepth   int // nesting of parenthesised lists
	fieldCount int
	depth      int

	reader

	continues chan<- bool

	brackets   int
	inRespCode bool
}

func (r *Reader) ReadSp() error {
	char, _, err := r.ReadRune()
	if err != nil {
		return err
	}
	if char != sp {
		return newParseError("expected a space")
	}
	return nil
}

func (r *Reader) ReadCrlf() (err error) {
	var char rune

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char == lf {
		return
	}
	if char != cr {
		err = newParseError("line doesn't end with a CR")
		return
	}

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != lf {
		err = newParseError("line doesn't end with a LF")
	}

	return
}

func (r *Reader) ReadAtom() (interface{}, error) {
	r.brackets = 0

	var b strings.Builder
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return nil, err
		}

		// TODO: list-wildcards and \
		if r.brackets == 0 && (char == listStart || char == literalStart || char == dquote) {
			return nil, newParseError("atom contains forbidden char: " + string(char))
		}
		if char == cr || char == lf {
			break
		}
		if r.brackets == 0 && (char == sp || char == listEnd) {
			break
		}
		if char == respCodeEnd {
			if r.brackets == 0 {
				if r.inRespCode {
					break
				} else {
					return nil, newParseError("atom contains bad brackets nesting")
				}
			}
			r.brackets--
		}
		if char == respCodeStart {
			r.brackets++
		}

		b.WriteRune(char)
		if r.MaxTextLen > 0 && b.Len() > r.MaxTextLen {
			return nil, newParseError("atom exceeding maximum length")
		}
	}
	atom := b.String()

	r.UnreadRune()

	if atom == nilAtom {
		return nil, nil
	}

	return atom, nil
}

var errLiteralTooBigNonSync = errors.New("imap: literal exceeding maximum size sent without waiting")

func (r *Reader) ReadLiteral() (Literal, error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return nil, err
	} else if char != literalStart {
		return nil, newParseError("literal string doesn't start with an open brace")
	}

	lstr, err := r.ReadString(byte(literalEnd))
	if err != nil {
		return nil, err
	}
	lstr = trimSuffix(lstr, literalEnd)

	nonSync := strings.HasSuffix(lstr, "+")
	if nonSync {
		lstr = trimSuffix(lstr, '+')
	}

	n, err := strconv.ParseUint(lstr, 10, 32)
	if err != nil {
		return nil, newParseError("cannot parse literal length: " + err.Error())
	}
	// Skien Mails patch: the line ends here; read its CRLF before any refusal below, or it would
	// be taken for an empty command afterwards.
	if err := r.ReadCrlf(); err != nil {
		return nil, err
	}
	if r.MaxLiteralSize > 0 && uint32(n) > r.MaxLiteralSize {
		if nonSync {
			// Skien Mails patch: the data is already on its way and would otherwise be read as
			// commands; the only safe answer is to end the connection.
			return nil, errLiteralTooBigNonSync
		}
		return nil, newParseError("literal exceeding maximum size")
	}
	if r.ReserveLiteral != nil {
		if err := r.ReserveLiteral(uint32(n)); err != nil {
			if nonSync {
				return nil, err
			}
			return nil, newParseError(err.Error())
		}
	}

	// Send continuation request if necessary
	if r.continues != nil && !nonSync {
		r.continues <- true
	}

	// Read literal
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return bytes.NewBuffer(b), nil
}

func (r *Reader) ReadQuotedString() (string, error) {
	if char, _, err := r.ReadRune(); err != nil {
		return "", err
	} else if char != dquote {
		return "", newParseError("quoted string doesn't start with a double quote")
	}

	var buf bytes.Buffer
	var escaped bool
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}

		if char == '\\' && !escaped {
			escaped = true
		} else {
			if char == cr || char == lf {
				r.UnreadRune()
				return "", newParseError("CR or LF not allowed in quoted string")
			}
			if char == dquote && !escaped {
				break
			}

			if !strings.ContainsRune(quotedSpecials, char) && escaped {
				return "", newParseError("quoted string cannot contain backslash followed by a non-quoted-specials char")
			}

			buf.WriteRune(char)
			escaped = false
			if r.MaxTextLen > 0 && buf.Len() > r.MaxTextLen {
				return "", newParseError("quoted string exceeding maximum length")
			}
		}
	}

	return buf.String(), nil
}

func (r *Reader) ReadFields() (fields []interface{}, err error) {
	var char rune
	for {
		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if err = r.UnreadRune(); err != nil {
			return
		}

		var field interface{}
		ok := true
		switch char {
		case literalStart:
			field, err = r.ReadLiteral()
		case dquote:
			field, err = r.ReadQuotedString()
		case listStart:
			field, err = r.ReadList()
		case listEnd:
			ok = false
		case cr:
			return
		default:
			field, err = r.ReadAtom()
		}

		if err != nil {
			return
		}
		if ok {
			r.fieldCount++
			if r.MaxFields > 0 && r.fieldCount > r.MaxFields {
				err = newParseError("response exceeding maximum number of fields")
				return
			}
			fields = append(fields, field)
		}

		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if char == cr || char == lf || char == listEnd || char == respCodeEnd {
			if char == cr || char == lf {
				r.UnreadRune()
			}
			return
		}
		if char == listStart {
			r.UnreadRune()
			continue
		}
		if char != sp {
			err = newParseError("fields are not separated by a space")
			return
		}
	}
}

func (r *Reader) ReadList() (fields []interface{}, err error) {
	r.depth++
	defer func() { r.depth-- }()
	if r.MaxDepth > 0 && r.depth > r.MaxDepth {
		return nil, newParseError("list nesting exceeding maximum depth")
	}
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != listStart {
		err = newParseError("list doesn't start with an open parenthesis")
		return
	}

	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != listEnd {
		err = newParseError("list doesn't end with a close parenthesis")
	}
	return
}

func (r *Reader) ReadLine() (fields []interface{}, err error) {
	r.fieldCount = 0
	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	err = r.ReadCrlf()
	return
}

func (r *Reader) ReadRespCode() (code StatusRespCode, fields []interface{}, err error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeStart {
		err = newParseError("response code doesn't start with an open bracket")
		return
	}

	r.inRespCode = true
	fields, err = r.ReadFields()
	r.inRespCode = false
	if err != nil {
		return
	}

	if len(fields) == 0 {
		err = newParseError("response code doesn't contain any field")
		return
	}

	codeStr, ok := fields[0].(string)
	if !ok {
		err = newParseError("response code doesn't start with a string atom")
		return
	}
	if codeStr == "" {
		err = newParseError("response code is empty")
		return
	}
	code = StatusRespCode(strings.ToUpper(codeStr))

	fields = fields[1:]

	r.UnreadRune()
	char, _, err = r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeEnd {
		err = newParseError("response code doesn't end with a close bracket")
	}
	return
}

func (r *Reader) ReadInfo() (info string, err error) {
	if r.MaxTextLen > 0 {
		var b strings.Builder
		for {
			var c rune
			if c, _, err = r.ReadRune(); err != nil {
				return
			}
			b.WriteRune(c)
			if c == lf {
				break
			}
			if b.Len() > r.MaxTextLen {
				err = newParseError("status text exceeding maximum length")
				return
			}
		}
		info = b.String()
	} else if info, err = r.ReadString(byte(lf)); err != nil {
		return
	}
	info = strings.TrimSuffix(info, string(lf))
	info = strings.TrimSuffix(info, string(cr))
	info = strings.TrimLeft(info, " ")

	return
}

// Default limits (Skien Mails patch); legitimate responses are far below them.
const (
	DefaultMaxFields  = 100000
	DefaultMaxTextLen = 1 << 20
	DefaultMaxDepth   = 100
)

func NewReader(r reader) *Reader {
	return &Reader{reader: r, MaxFields: DefaultMaxFields, MaxTextLen: DefaultMaxTextLen, MaxDepth: DefaultMaxDepth}
}

func NewServerReader(r reader, continues chan<- bool) *Reader {
	return &Reader{reader: r, continues: continues, MaxFields: DefaultMaxFields, MaxTextLen: DefaultMaxTextLen, MaxDepth: DefaultMaxDepth}
}

type Parser interface {
	Parse(fields []interface{}) error
}
