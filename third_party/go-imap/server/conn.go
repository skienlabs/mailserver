package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
)

// Conn is a connection to a client.
type Conn interface {
	io.Reader

	// Server returns this connection's server.
	Server() *Server
	// Context returns this connection's context.
	Context() *Context
	// Capabilities returns a list of capabilities enabled for this connection.
	Capabilities() []string
	// WriteResp writes a response to this connection.
	WriteResp(res imap.WriterTo) error
	// IsTLS returns true if TLS is enabled.
	IsTLS() bool
	// TLSState returns the TLS connection state if TLS is enabled, nil otherwise.
	TLSState() *tls.ConnectionState
	// Upgrade upgrades a connection, e.g. wrap an unencrypted connection with an
	// encrypted tunnel.
	Upgrade(upgrader imap.ConnUpgrader) error
	// Close closes this connection.
	Close() error
	WaitReady()

	Info() *imap.ConnInfo

	setTLSConn(*tls.Conn)
	authenticated() // Skien Mails patch: raise the pre-sign-in limits
	silent() *bool  // TODO: remove this
	serve(Conn) error
	commandHandler(cmd *imap.Command) (hdlr Handler, err error)
}

// Context stores a connection's metadata.
type Context struct {
	// This connection's current state.
	State imap.ConnState
	// If the client is logged in, the user.
	User backend.User
	// If the client has selected a mailbox, the mailbox.
	Mailbox backend.Mailbox
	// True if the currently selected mailbox has been opened in read-only mode.
	MailboxReadOnly bool
	// Responses to send to the client.
	Responses chan<- imap.WriterTo
	// Closed when the client is logged out.
	LoggedOut <-chan struct{}
}

type conn struct {
	*imap.Conn

	conn      Conn // With extensions overrides
	s         *Server
	ctx       *Context
	tlsConn   *tls.Conn
	continues chan bool
	upgrade   chan bool
	responses chan imap.WriterTo
	loggedOut chan struct{}
	silentVal bool

	preAuthUntil time.Time // Skien Mails patch: fixed deadline for signing in
	litReserved  int64     // Skien Mails patch: this connection's share of Server.LiteralBudget
	litUser      string
}

var errLiteralBusy = errors.New("[LIMIT] The server is busy receiving other messages. Try again shortly.")

// literalRate is the slowest upload speed allowed while a literal holds memory.
const literalRate = 128 << 10 // bytes per second

func (c *conn) reserveLiteral(n uint32) error {
	user := ""
	if c.ctx.User != nil {
		user = c.ctx.User.Username()
	}
	if c.litReserved > 0 && user != c.litUser {
		c.releaseLiterals() // can't happen within one command, but never mix accounts
	}
	if !c.s.LiteralBudget.reserve(user, int64(n)) {
		return errLiteralBusy
	}
	c.litReserved += int64(n)
	c.litUser = user
	// A stalled upload mustn't hold memory for long: a minute plus time at a slow rate. Before
	// sign-in the fixed sign-in deadline still applies if it's sooner.
	grace := c.s.LiteralBudget.Grace
	if grace <= 0 {
		grace = time.Minute
	}
	dl := time.Now().Add(grace + time.Duration(int64(n)/literalRate)*time.Second)
	if !c.preAuthUntil.IsZero() && (c.ctx.State == imap.ConnectingState || c.ctx.State == imap.NotAuthenticatedState) && c.preAuthUntil.Before(dl) {
		dl = c.preAuthUntil
	}
	c.Conn.SetDeadline(dl)
	return nil
}

func (c *conn) releaseLiterals() {
	if c.litReserved > 0 {
		c.s.LiteralBudget.release(c.litUser, c.litReserved)
		c.litReserved = 0
	}
}

func newConn(s *Server, c net.Conn) *conn {
	// Create an imap.Reader and an imap.Writer
	continues := make(chan bool)
	r := imap.NewServerReader(nil, continues)
	w := imap.NewWriter(nil)

	responses := make(chan imap.WriterTo)
	loggedOut := make(chan struct{})

	tlsConn, _ := c.(*tls.Conn)

	conn := &conn{
		Conn: imap.NewConn(c, r, w),

		s: s,
		ctx: &Context{
			State:     imap.ConnectingState,
			Responses: responses,
			LoggedOut: loggedOut,
		},
		tlsConn:   tlsConn,
		continues: continues,
		upgrade:   make(chan bool),
		responses: responses,
		loggedOut: loggedOut,
	}

	if s.PreAuthTimeout > 0 {
		conn.preAuthUntil = time.Now().Add(s.PreAuthTimeout)
	}
	if s.LiteralBudget != nil {
		conn.Conn.Reader.ReserveLiteral = conn.reserveLiteral
	}
	if s.Debug != nil {
		conn.Conn.SetDebug(s.Debug)
	}
	if s.MaxLiteralSize > 0 {
		conn.Conn.MaxLiteralSize = s.MaxLiteralSize
	}

	go conn.send()

	return conn
}

func (c *conn) Server() *Server {
	return c.s
}

func (c *conn) Context() *Context {
	return c.ctx
}

type response struct {
	response imap.WriterTo
	done     chan struct{}
}

func (r *response) WriteTo(w *imap.Writer) error {
	err := r.response.WriteTo(w)
	close(r.done)
	return err
}

func (c *conn) authenticated() {
	if c.s.MaxLiteralSizeAuthed > 0 {
		c.Conn.MaxLiteralSize = c.s.MaxLiteralSizeAuthed
	}
}

func (c *conn) setDeadline() {
	if c.s.PreAuthTimeout > 0 && (c.ctx.State == imap.ConnectingState || c.ctx.State == imap.NotAuthenticatedState) {
		// A fixed time from connecting, not renewed by commands: a client that keeps
		// sending NOOP without signing in is still cut off.
		if c.preAuthUntil.IsZero() {
			c.preAuthUntil = time.Now().Add(c.s.PreAuthTimeout)
		}
		c.Conn.SetDeadline(c.preAuthUntil)
		return
	}
	if c.s.AutoLogout == 0 {
		return
	}

	dur := c.s.AutoLogout
	if dur < MinAutoLogout {
		dur = MinAutoLogout
	}
	t := time.Now().Add(dur)

	c.Conn.SetDeadline(t)
}

func (c *conn) WriteResp(r imap.WriterTo) error {
	done := make(chan struct{})
	c.responses <- &response{r, done}
	<-done
	c.setDeadline()
	return nil
}

func (c *conn) closeNet() error { return c.Conn.Conn.Close() }

func (c *conn) Close() error {
	if c.ctx.User != nil {
		c.ctx.User.Logout()
	}

	return c.Conn.Close()
}

func (c *conn) Capabilities() []string {
	caps := []string{"IMAP4rev1", "LITERAL+", "SASL-IR", "CHILDREN", "UNSELECT", "MOVE", "IDLE"}

	appendLimitSet := false
	if c.ctx.State == imap.AuthenticatedState {
		if u, ok := c.ctx.User.(backend.AppendLimitUser); ok {
			if limit := u.CreateMessageLimit(); limit != nil {
				caps = append(caps, fmt.Sprintf("APPENDLIMIT=%v", *limit))
				appendLimitSet = true
			}
		}
	} else if be, ok := c.Server().Backend.(backend.AppendLimitBackend); ok {
		if limit := be.CreateMessageLimit(); limit != nil {
			caps = append(caps, fmt.Sprintf("APPENDLIMIT=%v", *limit))
			appendLimitSet = true
		}
	}
	if !appendLimitSet {
		caps = append(caps, "APPENDLIMIT")
	}

	if c.ctx.State == imap.NotAuthenticatedState {
		if !c.IsTLS() && c.s.TLSConfig != nil {
			caps = append(caps, "STARTTLS")
		}

		if !c.canAuth() {
			caps = append(caps, "LOGINDISABLED")
		} else {
			for name := range c.s.auths {
				caps = append(caps, "AUTH="+name)
			}
		}
	}

	for _, ext := range c.s.extensions {
		caps = append(caps, ext.Capabilities(c)...)
	}

	return caps
}

func (c *conn) writeAndFlush(w imap.WriterTo) error {
	if err := w.WriteTo(c.Writer); err != nil {
		return err
	}
	return c.Writer.Flush()
}

func (c *conn) send() {
	// Send responses
	for {
		select {
		case <-c.upgrade:
			// Wait until upgrade is finished.
			c.Wait()
		case needCont := <-c.continues:
			// Send continuation requests
			if needCont {
				resp := &imap.ContinuationReq{Info: "send literal"}
				if err := c.writeAndFlush(resp); err != nil {
					c.Server().ErrorLog.Println("cannot send continuation request: ", err)
				}
			}
		case res := <-c.responses:
			// Got a response that needs to be sent
			// Request to send the response
			if err := c.writeAndFlush(res); err != nil {
				c.Server().ErrorLog.Println("cannot send response: ", err)
			}
		case <-c.loggedOut:
			return
		}
	}
}

func (c *conn) greet() error {
	c.ctx.State = imap.NotAuthenticatedState

	caps := c.Capabilities()
	args := make([]interface{}, len(caps))
	for i, cap := range caps {
		args[i] = imap.RawString(cap)
	}

	greeting := &imap.StatusResp{
		Type:      imap.StatusRespOk,
		Code:      imap.CodeCapability,
		Arguments: args,
		Info:      "IMAP4rev1 Service Ready",
	}

	return c.WriteResp(greeting)
}

func (c *conn) setTLSConn(tlsConn *tls.Conn) {
	c.tlsConn = tlsConn
}

func (c *conn) IsTLS() bool {
	return c.tlsConn != nil
}

func (c *conn) TLSState() *tls.ConnectionState {
	if c.tlsConn != nil {
		state := c.tlsConn.ConnectionState()
		return &state
	}
	return nil
}

// canAuth checks if the client can use plain text authentication.
func (c *conn) canAuth() bool {
	return c.IsTLS() || c.s.AllowInsecureAuth
}

func (c *conn) silent() *bool {
	return &c.silentVal
}

func (c *conn) serve(conn Conn) (err error) {
	c.conn = conn

	defer func() {
		c.ctx.State = imap.LogoutState
		close(c.loggedOut)
		c.releaseLiterals()
	}()

	defer func() {
		if r := recover(); r != nil {
			c.WriteResp(&imap.StatusResp{
				Type: imap.StatusRespBye,
				Info: "Internal server error, closing connection.",
			})

			stack := debug.Stack()
			c.s.ErrorLog.Printf("panic serving %v: %v\n%s", c.Info().RemoteAddr, r, stack)

			err = fmt.Errorf("%v", r)
		}
	}()

	// Send greeting
	if err := c.greet(); err != nil {
		return err
	}

	for {
		if c.ctx.State == imap.LogoutState {
			return nil
		}

		var res *imap.StatusResp
		var up Upgrader

		c.releaseLiterals() // the previous command is done with its literals
		fields, err := c.ReadLine()
		if err == io.EOF || c.ctx.State == imap.LogoutState {
			return nil
		}
		c.setDeadline()

		if err != nil {
			if imap.IsParseError(err) {
				res = &imap.StatusResp{
					Type: imap.StatusRespBad,
					Info: err.Error(),
				}
				// Skien Mails patch: answer with the command's tag when it was read (e.g. a
				// refused literal), so the client isn't left waiting for its reply.
				if len(fields) > 0 {
					if tag, ok := fields[0].(string); ok && tag != "" && len(tag) <= 64 && tag != "*" && tag != "+" {
						res.Tag = tag
					}
				}
			} else {
				c.s.ErrorLog.Println("cannot read command:", err)
				return err
			}
		} else {
			cmd := &imap.Command{}
			if err := cmd.Parse(fields); err != nil {
				res = &imap.StatusResp{
					Tag:  cmd.Tag,
					Type: imap.StatusRespBad,
					Info: err.Error(),
				}
			} else {
				var err error
				res, up, err = c.handleCommand(cmd)
				if err != nil {
					res = &imap.StatusResp{
						Tag:  cmd.Tag,
						Type: imap.StatusRespBad,
						Info: err.Error(),
					}
				}
			}
		}

		c.releaseLiterals() // the command is done with its literals before its reply goes out

		if res != nil {

			if err := c.WriteResp(res); err != nil {
				c.s.ErrorLog.Println("cannot write response:", err)
				continue
			}

			if up != nil && res.Type == imap.StatusRespOk {
				if err := up.Upgrade(c.conn); err != nil {
					c.s.ErrorLog.Println("cannot upgrade connection:", err)
					return err
				}
			}
		}
	}
}

func (c *conn) WaitReady() {
	c.upgrade <- true
	c.Conn.WaitReady()
}

func (c *conn) commandHandler(cmd *imap.Command) (hdlr Handler, err error) {
	newHandler := c.s.Command(cmd.Name)
	if newHandler == nil {
		err = errors.New("Unknown command")
		return
	}

	hdlr = newHandler()
	err = hdlr.Parse(cmd.Arguments)
	return
}

func (c *conn) handleCommand(cmd *imap.Command) (res *imap.StatusResp, up Upgrader, err error) {
	hdlr, err := c.commandHandler(cmd)
	if err != nil {
		return
	}

	hdlrErr := hdlr.Handle(c.conn)
	if statusErr, ok := hdlrErr.(*imap.ErrStatusResp); ok {
		res = statusErr.Resp
	} else if hdlrErr != nil {
		res = &imap.StatusResp{
			Type: imap.StatusRespNo,
			Info: hdlrErr.Error(),
		}
	} else {
		res = &imap.StatusResp{
			Type: imap.StatusRespOk,
		}
	}

	if res != nil {
		res.Tag = cmd.Tag

		if res.Type == imap.StatusRespOk && res.Info == "" {
			res.Info = cmd.Name + " completed"
		}
	}

	up, _ = hdlr.(Upgrader)
	return
}
