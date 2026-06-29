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
	indexHTML []byte
	indexETag string
)

func init() {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		panic("web: missing embedded static/index.html: " + err.Error())
	}
	indexHTML = data
	sum := sha256.Sum256(data)
	indexETag = `"` + hex.EncodeToString(sum[:]) + `"`
}

// Register wires the landing page and its assets onto a stdlib mux. Kept for the
// package's own tests; the server mounts Index and Assets on chi directly.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", Index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", Assets()))
}

// Index serves the landing page with content-hash ETag revalidation.
func Index(w http.ResponseWriter, r *http.Request) {
	if match := r.Header.Get("If-None-Match"); match == indexETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	w.Header().Set("ETag", indexETag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(indexHTML)
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
