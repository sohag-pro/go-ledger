// Package web serves the embedded static landing page and its assets.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// indexETag is computed once at startup from the embedded index.html. Embedded
// files carry no modtime, so http.ServeContent cannot derive a validator on its
// own; a content hash gives us a stable, correct ETag for revalidation.
var (
	indexHTML   []byte
	indexETag   string
	consoleHTML []byte
	consoleETag string
)

func init() {
	indexHTML, indexETag = loadPage("static/index.html")
	consoleHTML, consoleETag = loadPage("static/console.html")
}

// loadPage reads an embedded HTML page and computes its content-hash ETag.
// Embedded files carry no modtime, so a content hash gives a stable validator.
func loadPage(name string) ([]byte, string) {
	data, err := staticFS.ReadFile(name)
	if err != nil {
		panic("web: missing embedded " + name + ": " + err.Error())
	}
	sum := sha256.Sum256(data)
	return data, `"` + hex.EncodeToString(sum[:]) + `"`
}

// Register wires the landing page, console, and assets onto a stdlib mux. Kept
// for the package's own tests; the server mounts the handlers on chi directly.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", Index)
	mux.HandleFunc("GET /console", Console)
	mux.Handle("GET /static/", http.StripPrefix("/static/", Assets()))
}

// Index serves the landing page with content-hash ETag revalidation.
func Index(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, indexHTML, indexETag)
}

// Console serves the developer console (a deliberate dev-tool exception to the
// "no web UI" scope rule; see CLAUDE.md). It calls the same public /v1 API.
func Console(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, consoleHTML, consoleETag)
}

// Favicon serves the site icon at the conventional /favicon.ico root path, which
// browsers and link scrapers request directly (in addition to the <link
// rel="icon"> tags on each page). The bytes come from the embedded static dir.
func Favicon(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/favicon.ico")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// BookPDF serves the compiled book with Content-Disposition: inline so browsers
// open it in their native PDF viewer instead of downloading it. The bytes are
// embedded at the module root (the goledger package); passing them in keeps this
// package free of that import. The content-hash ETag gives revalidation, since
// embedded bytes carry no modtime.
func BookPDF(pdf []byte) http.HandlerFunc {
	sum := sha256.Sum256(pdf)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `inline; filename="the-ledger-book.pdf"`)
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		w.Header().Set("ETag", etag)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(pdf)
	}
}

func servePage(w http.ResponseWriter, r *http.Request, body []byte, etag string) {
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// Assets serves files under static/ (fonts) with a long immutable cache. Asset
// filenames are content-stable, so a year-long cache is safe.
func Assets() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: cannot sub static FS: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fileServer.ServeHTTP(w, r)
	})
}
