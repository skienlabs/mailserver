package core

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// Protections against a hostile or broken source server or file.

var (
	errIMAPLiteralTooBig = errors.New("the server sent a message larger than the import limit")
	errIMAPBudget        = errors.New("the server sent far more data than expected")
	errIMAPEarlyLiteral  = errors.New("the server sent an unexpected greeting")
	errIMAPMalformed     = errors.New("the server sent a malformed response")
)

func isGuardErr(err error) bool {
	return errors.Is(err, errIMAPLiteralTooBig) || errors.Is(err, errIMAPBudget) || errors.Is(err, errIMAPEarlyLiteral) || errors.Is(err, errIMAPMalformed) ||
		(err != nil && strings.Contains(err.Error(), "literal exceeding maximum size"))
}

// imapGuardConn caps how many bytes the server may send for the command in progress. Together
// with the library's own literal size limit (setMaxLiteral) this bounds memory no matter what
// the server sends — no protocol parsing here, so it can't be confused by crafted responses.
//
// The library reads the greeting before its literal limit can be set, so until release() any
// literal at all is refused: a literal marker is "}" right before the line break, and nothing
// legitimate before sign-in (greeting, capabilities) contains one. Nothing is skipped, so
// there's nothing to desynchronise.
type imapGuardConn struct {
	net.Conn
	mu       sync.Mutex
	budget   int64
	err      error
	released bool
	prev     byte
}

func newIMAPGuard(c net.Conn, budget int64) *imapGuardConn {
	return &imapGuardConn{Conn: c, budget: budget}
}

// release ends the greeting phase; call it after setMaxLiteral.
func (g *imapGuardConn) release() {
	g.mu.Lock()
	g.released = true
	g.mu.Unlock()
}

// resetBudget allows n more bytes from the server.
func (g *imapGuardConn) resetBudget(n int64) {
	g.mu.Lock()
	g.budget = n
	g.mu.Unlock()
}

// cause returns the guard's own error if it tripped (the IMAP library may not wrap it),
// otherwise err.
func (g *imapGuardConn) cause(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	if err != nil && strings.Contains(err.Error(), "literal exceeding maximum size") {
		return errIMAPLiteralTooBig
	}
	return err
}

func (g *imapGuardConn) Read(p []byte) (int, error) {
	g.mu.Lock()
	if g.err != nil {
		err := g.err
		g.mu.Unlock()
		return 0, err
	}
	if int64(len(p)) > g.budget+1 {
		p = p[:max(g.budget+1, 1)] // never read past the budget
	}
	g.mu.Unlock()
	n, err := g.Conn.Read(p)
	g.mu.Lock()
	g.budget -= int64(n)
	if g.budget < 0 && g.err == nil {
		g.err = errIMAPBudget
	}
	if !g.released && g.err == nil {
		for _, b := range p[:n] {
			if g.prev == '}' && (b == '\r' || b == '\n') {
				g.err = errIMAPEarlyLiteral
				break
			}
			g.prev = b
		}
	}
	if gerr := g.err; gerr != nil {
		g.mu.Unlock()
		g.Conn.Close()
		return 0, gerr
	}
	g.mu.Unlock()
	return n, err
}

// trip stops the connection with err (the first reason wins).
func (g *imapGuardConn) trip(err error) {
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
	g.Conn.Close()
}

// imapErrorLog receives the IMAP library's internal errors. The library logs a refused
// oversized literal and keeps reading, so the connection is stopped here instead of waiting
// for the command to time out. Other messages (e.g. from closing on purpose) are dropped.
type imapErrorLog struct{ g *imapGuardConn }

func (l imapErrorLog) check(s string) {
	switch {
	case strings.Contains(s, "literal exceeding maximum size"):
		l.g.trip(errIMAPLiteralTooBig)
	case strings.Contains(s, "exceeding maximum"): // the patched library's field/text limits
		l.g.trip(errIMAPMalformed)
	}
}
func (l imapErrorLog) Printf(format string, v ...any) { l.check(fmt.Sprintf(format, v...)) }
func (l imapErrorLog) Println(v ...any)               { l.check(fmt.Sprintln(v...)) }

// setMaxLiteral makes the IMAP library refuse literals larger than n before allocating them.
// go-imap v1 supports the limit but its client doesn't expose it, so it's set on the client's
// connection through reflection; if that ever fails (library changed), imports refuse to run.
func setMaxLiteral(c *imapclient.Client, n uint32) bool {
	v := reflect.ValueOf(c).Elem().FieldByName("conn")
	if !v.IsValid() || v.Type() != reflect.TypeOf((*imap.Conn)(nil)) || !v.CanAddr() {
		return false
	}
	conn := *(**imap.Conn)(unsafe.Pointer(v.UnsafeAddr()))
	if conn == nil || conn.Reader == nil {
		return false
	}
	conn.Reader.MaxLiteralSize = n
	return true
}

// openZipLimited opens a zip archive, refusing it when loading its file list would take too
// much memory. archive/zip reads the whole central directory up front and trusts neither the
// declared entry count nor the declared size (it reads entries until one doesn't parse), so:
//   - the declared count is checked first, because archive/zip preallocates from it, and
//   - the bytes it actually reads while opening are capped, and the real count checked after.
func openZipLimited(name string, maxEntries int, maxDirBytes int64) (*zip.Reader, io.Closer, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if n, err := zipDeclaredEntries(f, st.Size()); err != nil {
		f.Close()
		return nil, nil, err
	} else if n > uint64(maxEntries) {
		f.Close()
		return nil, nil, errZipTooManyFiles
	}
	br := &budgetReaderAt{r: f}
	br.left.Store(maxDirBytes + 128<<10) // + the end-record search
	zr, err := zip.NewReader(br, st.Size())
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if len(zr.File) > maxEntries {
		f.Close()
		return nil, nil, errZipTooManyFiles
	}
	br.left.Store(-1) // reading the files themselves is streamed; no cap
	return zr, f, nil
}

// budgetReaderAt fails reads once left bytes have been read (left < 0 means no limit).
type budgetReaderAt struct {
	r    io.ReaderAt
	left atomic.Int64
}

func (b *budgetReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if l := b.left.Load(); l >= 0 {
		if int64(len(p)) > l {
			return 0, errZipTooManyFiles
		}
		b.left.Add(-int64(len(p)))
	}
	return b.r.ReadAt(p, off)
}

// zipDeclaredEntries returns the entry count archive/zip will see in the end record, found and
// decoded exactly as archive/zip (Go 1.26) does it.
func zipDeclaredEntries(r io.ReaderAt, size int64) (uint64, error) {
	const endLen, loc64Len, end64Len = 22, 20, 56
	var buf []byte
	var endOff int64
	for i, bLen := range []int64{1024, 65 * 1024} {
		bLen = min(bLen, size)
		buf = make([]byte, int(bLen))
		if _, err := r.ReadAt(buf, size-bLen); err != nil && err != io.EOF {
			return 0, err
		}
		p := -1
		for k := len(buf) - endLen; k >= 0; k-- {
			if buf[k] == 'P' && buf[k+1] == 'K' && buf[k+2] == 5 && buf[k+3] == 6 {
				if n := int(binary.LittleEndian.Uint16(buf[k+endLen-2:])); n+endLen+k <= len(buf) {
					p = k
				}
				break
			}
		}
		if p >= 0 {
			buf = buf[p:]
			endOff = size - bLen + int64(p)
			break
		}
		if i == 1 || bLen == size {
			return 0, errors.New("not a zip file")
		}
	}
	records := uint64(binary.LittleEndian.Uint16(buf[10:]))
	dirSize := binary.LittleEndian.Uint32(buf[12:])
	dirOff := binary.LittleEndian.Uint32(buf[16:])
	if records == 0xffff || dirSize == 0xffff || dirOff == 0xffffffff {
		locOff := endOff - loc64Len
		if locOff < 0 {
			return records, nil
		}
		loc := make([]byte, loc64Len)
		if _, err := r.ReadAt(loc, locOff); err != nil {
			return 0, err
		}
		if binary.LittleEndian.Uint32(loc) != 0x07064b50 || binary.LittleEndian.Uint32(loc[4:]) != 0 ||
			binary.LittleEndian.Uint32(loc[16:]) != 1 {
			return records, nil
		}
		rec := make([]byte, end64Len)
		if _, err := r.ReadAt(rec, int64(binary.LittleEndian.Uint64(loc[8:]))); err != nil {
			return 0, err
		}
		if binary.LittleEndian.Uint32(rec) != 0x06064b50 {
			return 0, errors.New("damaged zip64 archive")
		}
		records = binary.LittleEndian.Uint64(rec[32:])
	}
	return records, nil
}

var errZipTooManyFiles = errors.New("The .zip file lists too many files. Split the export into smaller parts.")
