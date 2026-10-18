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
