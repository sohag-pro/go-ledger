// Package goledger embeds repo-root release artifacts (the compiled book) so the
// server can serve them without copying the files into a package subtree. The
// embed directive can only reach files at or below its own directory, so this
// lives at the module root where the artifacts do.
package goledger

import _ "embed"

// LedgerBookPDF is the compiled book, embedded from the repo root at build time.
// The landing page serves it inline so it opens in the browser's native PDF
// viewer instead of downloading. The ebook build regenerates the file; the next
// `go build` picks up the new bytes, so the served copy never drifts.
//
//go:embed the-ledger-book.pdf
var LedgerBookPDF []byte
