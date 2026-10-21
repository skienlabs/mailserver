// Package server provides an IMAP server.
package server

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-sasl"
)

// The minimum autologout duration defined in RFC 3501 section 5.4.
const MinAutoLogout = 30 * time.Minute

// A command handler.
type Handler interface {
	imap.Parser

	// Handle this command for a given connection.
	//
	// By default, after this function has returned a status response is sent. To
	// prevent this behavior handlers can use imap.ErrStatusResp.
	Handle(conn Conn) error
}

// A connection upgrader. If a Handler is also an Upgrader, the connection will
// be upgraded after the Handler succeeds.
//
// This should only be used by libraries implementing an IMAP extension (e.g.
// COMPRESS).
type Upgrader interface {
	// Upgrade the connection. This method should call conn.Upgrade().
	Upgrade(conn Conn) error
}

// A function that creates handlers.
type HandlerFactory func() Handler

// A function that creates SASL servers.
type SASLServerFactory func(conn Conn) sasl.Server

// An IMAP extension.
type Extension interface {
	// Get capabilities provided by this extension for a given connection.
	Capabilities(c Conn) []string
	// Get the command handler factory for the provided command name.
	Command(name string) HandlerFactory
}

// An extension that provides additional features to each connection.
type ConnExtension interface {
	Extension

	// This function will be called when a client connects to the server. It can
	// be used to add new features to the default Conn interface by implementing
	// new methods.
	NewConn(c Conn) Conn
}

// ErrStatusResp can be returned by a Handler to replace the default status
// response. The response tag must be empty.
//
// Deprecated: Use imap.ErrStatusResp{res} instead.
//
// To disable the default status response, use imap.ErrStatusResp{nil} instead.
func ErrStatusResp(res *imap.StatusResp) error {
	return &imap.ErrStatusResp{res}
}

// ErrNoStatusResp can be returned by a Handler to prevent the default status
// response from being sent.
//
// Deprecated: Use imap.ErrStatusResp{nil} instead
func ErrNoStatusResp() error {
	return &imap.ErrStatusResp{nil}
}

// An IMAP server.
type Server struct {
	locker    sync.Mutex
	listeners map[net.Listener]struct{}
	conns     map[Conn]struct{}
	serving   sync.WaitGroup // Skien Mails patch: connection goroutines (see WaitConns)

	commands   map[string]HandlerFactory
	auths      map[string]SASLServerFactory
	extensions []Extension

	// TCP address to listen on.
	Addr string
	// This server's TLS configuration.
	TLSConfig *tls.Config
	// This server's backend.
	Backend backend.Backend
	// Backend updates that will be sent to connected clients.
	Updates <-chan backend.Update
	// Automatically logout clients after a duration. To do not logout users
	// automatically, set this to zero. The duration MUST be at least
	// MinAutoLogout (as stated in RFC 3501 section 5.4).
	AutoLogout time.Duration
	// Allow authentication over unencrypted connections.
	AllowInsecureAuth bool
	// An io.Writer to which all network activity will be mirrored.
	Debug io.Writer
	// ErrorLog specifies an optional logger for errors accepting
	// connections and unexpected behavior from handlers.
	// If nil, logging goes to os.Stderr via the log package's
	// standard logger.
	ErrorLog imap.Logger
	// The maximum literal size, in bytes. Literals exceeding this size will be
	// rejected. A value of zero disables the limit (this is the default).
	MaxLiteralSize uint32

	// Skien Mails patch (see PATCHES.md): limits that apply before a client has signed in.
	// MaxLiteralSize applies until then; MaxLiteralSizeAuthed afterwards (0 = keep MaxLiteralSize).
	// PreAuthTimeout replaces AutoLogout until sign-in (0 = AutoLogout).
	MaxLiteralSizeAuthed uint32
	PreAuthTimeout       time.Duration
	// Skien Mails patch: LiteralBudget caps the literal bytes held in memory at once (nil = no
	// limit). Several servers may share one budget; literals are released when their command
	// finishes.
	LiteralBudget *LiteralBudget
	// Skien Mails patch: CommandDone, when set, is called after each command with how long it
	// took (not counting the time to receive its literals), for spotting slow commands.
	CommandDone func(c Conn, name string, took time.Duration)
	// IdlePoll is how often a mailbox is polled during IDLE when it has no notifier (0 = 30s).
	IdlePoll time.Duration
}

// Create a new IMAP server from an existing listener.
func New(bkd backend.Backend) *Server {
	s := &Server{
		listeners: make(map[net.Listener]struct{}),
		conns:     make(map[Conn]struct{}),
		Backend:   bkd,
		ErrorLog:  log.New(os.Stderr, "imap/server: ", log.LstdFlags),
	}

	s.auths = map[string]SASLServerFactory{
		sasl.Plain: func(conn Conn) sasl.Server {
			return sasl.NewPlainServer(func(identity, username, password string) error {
				if identity != "" && identity != username {
					return errors.New("Identities not supported")
				}

				user, err := bkd.Login(conn.Info(), username, password)
				if err != nil {
					return err
				}

				ctx := conn.Context()
				ctx.State = imap.AuthenticatedState
				ctx.User = user
				conn.authenticated()
				return nil
			})
		},
	}

	s.commands = map[string]HandlerFactory{
		"NOOP":       func() Handler { return &Noop{} },
		"CAPABILITY": func() Handler { return &Capability{} },
		"LOGOUT":     func() Handler { return &Logout{} },

		"STARTTLS":     func() Handler { return &StartTLS{} },
		"LOGIN":        func() Handler { return &Login{} },
		"AUTHENTICATE": func() Handler { return &Authenticate{} },

		"SELECT": func() Handler { return &Select{} },
		"EXAMINE": func() Handler {
			hdlr := &Select{}
			hdlr.ReadOnly = true
			return hdlr
		},
		"CREATE":      func() Handler { return &Create{} },
		"DELETE":      func() Handler { return &Delete{} },
		"RENAME":      func() Handler { return &Rename{} },
		"SUBSCRIBE":   func() Handler { return &Subscribe{} },
		"UNSUBSCRIBE": func() Handler { return &Unsubscribe{} },
		"LIST":        func() Handler { return &List{} },
		"LSUB": func() Handler {
			hdlr := &List{}
			hdlr.Subscribed = true
			return hdlr
		},
		"STATUS":   func() Handler { return &Status{} },
		"APPEND":   func() Handler { return &Append{} },
		"UNSELECT": func() Handler { return &Unselect{} },
		"IDLE":     func() Handler { return &Idle{} },

		"CHECK":   func() Handler { return &Check{} },
		"CLOSE":   func() Handler { return &Close{} },
		"EXPUNGE": func() Handler { return &Expunge{} },
		"SEARCH":  func() Handler { return &Search{} },
		"FETCH":   func() Handler { return &Fetch{} },
		"STORE":   func() Handler { return &Store{} },
		"COPY":    func() Handler { return &Copy{} },
		"MOVE":    func() Handler { return &Move{} },
		"UID":     func() Handler { return &Uid{} },
	}

	return s
}

// Serve accepts incoming connections on the Listener l.
func (s *Server) Serve(l net.Listener) error {
	s.locker.Lock()
	s.listeners[l] = struct{}{}
	s.locker.Unlock()

	defer func() {
		s.locker.Lock()
		defer s.locker.Unlock()
		l.Close()
		delete(s.listeners, l)
	}()

	updater, ok := s.Backend.(backend.BackendUpdater)
	if ok {
		s.Updates = updater.Updates()
		go s.listenUpdates()
	}

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}

		var conn Conn = newConn(s, c)
		for _, ext := range s.extensions {
			if ext, ok := ext.(ConnExtension); ok {
				conn = ext.NewConn(conn)
			}
		}

		s.serving.Add(1) // before the goroutine starts, so WaitConns can't miss it
		go s.serveConn(conn)
	}
}

// ListenAndServe listens on the TCP network address s.Addr and then calls Serve
// to handle requests on incoming connections.
//
// If s.Addr is blank, ":imap" is used.
func (s *Server) ListenAndServe() error {
	addr := s.Addr
	if addr == "" {
		addr = ":imap"
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return s.Serve(l)
}

// ListenAndServeTLS listens on the TCP network address s.Addr and then calls
// Serve to handle requests on incoming TLS connections.
//
// If s.Addr is blank, ":imaps" is used.
func (s *Server) ListenAndServeTLS() error {
	addr := s.Addr
	if addr == "" {
		addr = ":imaps"
	}

	l, err := tls.Listen("tcp", addr, s.TLSConfig)
	if err != nil {
		return err
	}

	return s.Serve(l)
}

func (s *Server) serveConn(conn Conn) error {
	s.locker.Lock()
	s.conns[conn] = struct{}{}
	s.locker.Unlock()

	defer func() {
		s.locker.Lock()
		defer s.locker.Unlock()
		conn.Close()
		delete(s.conns, conn)
		s.serving.Done()
	}()

	return conn.serve(conn)
}

// Command gets a command handler factory for the provided command name.
func (s *Server) Command(name string) HandlerFactory {
	// Extensions can override builtin commands
	for _, ext := range s.extensions {
		if h := ext.Command(name); h != nil {
			return h
		}
	}

	return s.commands[name]
}

func (s *Server) listenUpdates() {
	for {
		update := <-s.Updates

		var res imap.WriterTo
		switch update := update.(type) {
		case *backend.StatusUpdate:
			res = update.StatusResp
		case *backend.MailboxUpdate:
			res = &responses.Select{Mailbox: update.MailboxStatus}
		case *backend.MailboxInfoUpdate:
			ch := make(chan *imap.MailboxInfo, 1)
			ch <- update.MailboxInfo
			close(ch)

			res = &responses.List{Mailboxes: ch}
		case *backend.MessageUpdate:
			ch := make(chan *imap.Message, 1)
			ch <- update.Message
			close(ch)

			res = &responses.Fetch{Messages: ch}
		case *backend.ExpungeUpdate:
			ch := make(chan uint32, 1)
			ch <- update.SeqNum
			close(ch)

			res = &responses.Expunge{SeqNums: ch}
		default:
			s.ErrorLog.Printf("unhandled update: %T\n", update)
		}
		if res == nil {
			continue
		}

		sends := make(chan struct{})
		wait := 0
		s.locker.Lock()
		for conn := range s.conns {
			ctx := conn.Context()

			if update.Username() != "" && (ctx.User == nil || ctx.User.Username() != update.Username()) {
				continue
			}
			if update.Mailbox() != "" && (ctx.Mailbox == nil || ctx.Mailbox.Name() != update.Mailbox()) {
				continue
			}
			if *conn.silent() {
				// If silent is set, do not send message updates
				if _, ok := res.(*responses.Fetch); ok {
					continue
				}
			}

			conn := conn // Copy conn to a local variable
			go func() {
				done := make(chan struct{})
				conn.Context().Responses <- &response{
					response: res,
					done:     done,
				}
				<-done
				sends <- struct{}{}
			}()

			wait++
		}
		s.locker.Unlock()

		if wait > 0 {
			go func() {
				for done := 0; done < wait; done++ {
					<-sends
				}

				close(update.Done())
			}()
		} else {
			close(update.Done())
		}
	}
}

// ForEachConn iterates through all opened connections.
func (s *Server) ForEachConn(f func(Conn)) {
	s.locker.Lock()
	defer s.locker.Unlock()
	for conn := range s.conns {
		f(conn)
	}
}

// Stops listening and closes all current connections.
func (s *Server) Close() error {
	s.locker.Lock()
	defer s.locker.Unlock()

	for l := range s.listeners {
		l.Close()
	}

	// Skien Mails patch: close only the network connections. Each connection's own goroutine
	// then finishes its current command and logs out (calling Close here raced with sign-in).
	for conn := range s.conns {
		if nc, ok := conn.(interface{ closeNet() error }); ok {
			nc.closeNet()
		} else {
			conn.Close()
		}
	}

	return nil
}

// WaitConns waits (up to d) until every connection's goroutine has finished, after Close.
// Skien Mails patch: shutdown lets commands that are running finish before storage closes.
func (s *Server) WaitConns(d time.Duration) bool {
	done := make(chan struct{})
	go func() { s.serving.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Enable some IMAP extensions on this server.
// Wiki entry: https://github.com/emersion/go-imap/wiki/Using-extensions
func (s *Server) Enable(extensions ...Extension) {
	for _, ext := range extensions {
		// Ignore built-in extensions
		if ext.Command("UNSELECT") != nil || ext.Command("MOVE") != nil || ext.Command("IDLE") != nil {
			continue
		}
		s.extensions = append(s.extensions, ext)
	}
}

// Enable an authentication mechanism on this server.
// Wiki entry: https://github.com/emersion/go-imap/wiki/Using-authentication-mechanisms
func (s *Server) EnableAuth(name string, f SASLServerFactory) {
	s.auths[name] = f
}

// LiteralBudget limits message uploads (literals) held in memory: in total, and per signed-in
// account so one account can't take it all. Skien Mails patch.
type LiteralBudget struct {
	Max     int64         // all connections together
	PerUser int64         // one account (0 = no separate limit)
	Grace   time.Duration // time to deliver a literal on top of the minimum rate (0 = 1 minute)

	mu     sync.Mutex
	used   int64
	byUser map[string]int64
}

func (b *LiteralBudget) reserve(user string, n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.Max || (user != "" && b.PerUser > 0 && b.byUser[user]+n > b.PerUser) {
		return false
	}
	b.used += n
	if user != "" {
		if b.byUser == nil {
			b.byUser = map[string]int64{}
		}
		b.byUser[user] += n
	}
	return true
}

func (b *LiteralBudget) release(user string, n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if user != "" {
		if b.byUser[user] -= n; b.byUser[user] <= 0 {
			delete(b.byUser, user)
		}
	}
}

// InUse reports the bytes reserved right now (for tests and monitoring).
func (b *LiteralBudget) InUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
