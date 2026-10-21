# go-imap v1.2.1, patched for Skien Mails

This is github.com/emersion/go-imap v1.2.1 (MIT licence, see LICENSE), used through a
`replace` directive in the top-level go.mod. Test files were left out. The only changes are in
`read.go` and `response.go`, and protect the mail importer from hostile IMAP servers:

- **Field limit** (`Reader.MaxFields`, default 100 000 per response): the parser keeps a whole
  response in memory, and tiny fields like `\a ` cost ~25 bytes of memory per 3 bytes sent.
- **Nesting limit** (`Reader.MaxDepth`, default 100): lists are parsed recursively, so
  millions of `(` could otherwise exhaust the stack and crash the process.
- **Text limit** (`Reader.MaxTextLen`, default 1 MB) for atoms, quoted strings and status text.
- **Linear atom parsing**: atoms were built with `atom += string(char)`, which is quadratic, so
  one long atom could keep a CPU busy for minutes.

Literal sizes are limited separately by the importer through `Reader.MaxLiteralSize`.

Server side (the mail-app IMAP server), in `server/` and `backend/updates.go`:

- **Limits before sign-in** (`Server.PreAuthTimeout`, `Server.MaxLiteralSizeAuthed`): a client
  that hasn't signed in gets small literals and a fixed time to sign in (not renewed by further
  commands); the limits rise after sign-in.
- **Mailbox-owned updates** (`backend.ResponseWriterBinder`): SELECT hands the mailbox a writer for
  its own connection, so it can send EXPUNGE / EXISTS / FETCH updates in order (each session keeps
  its own view of message numbers; the stock broadcast model can't do that).
- **Selection bookkeeping** (`backend.ReadOnlySetter`, unbinding on SELECT/CLOSE/UNSELECT): the
  mailbox knows when it stops being selected and whether it was opened with EXAMINE; CLOSE on a
  read-only mailbox no longer expunges (RFC 3501 6.4.2).
- **IDLE that notices changes** (`backend.IdleNotifier`, `Server.IdlePoll`): the stock IDLE only
  waited for DONE; now it polls the selected mailbox when the backend signals a change.
- **Memory budget for literals** (`Server.LiteralBudget`, `Reader.ReserveLiteral`): message
  uploads held in memory at once are capped in total (one budget can be shared by several
  servers) and per account; a literal over the budget is refused before the server asks for it
  (or the connection is closed if the client sent it unasked), and a literal being read must
  arrive at a minimum rate so a stalled upload releases its memory.
- **Oversized unasked literals end the connection**: a LITERAL+ literal over `MaxLiteralSize`
  used to be refused while its data was then read as commands; the connection is now closed.
  A refused literal's line ending is read first, so it isn't taken for an empty command.
- **Clean shutdown** (`Server.WaitConns`): `Close` closes only the network connections (each
  connection's goroutine logs out itself, instead of racing with sign-in), and `WaitConns` lets
  running commands finish before the application closes its storage.
- **Expunge in one step** (`backend.ExpungeReporter`): EXPUNGE lets the mailbox decide what to
  remove from the flags at that moment, remove it and report it, instead of the server searching
  first and the mailbox expunging separately (which needed a cached search that CLOSE could
  reuse after it went stale).
- **Tagged replies to unreadable commands**: a command refused while it was being read (for
  example a literal over a limit) gets a BAD with its own tag, not an untagged one the client
  doesn't recognise as the answer.
- **Command timing** (`Server.CommandDone`): called after each command with its duration, so
  the application can log slow ones.
- **Ended sessions leave IDLE** (`backend.ErrSessionEnded`): when a poll during IDLE reports
  that the session has ended (password changed, account disabled), the server says BYE and
  disconnects instead of idling on.
- **Keyword case kept** (`imap.CanonicalFlagKeepCase`, used by STORE and APPEND): the stock code
  lowercased every keyword (`$MailFlagBit0` became `$mailflagbit0`); system flags are still
  canonicalised, and the backend compares keywords case-insensitively.
