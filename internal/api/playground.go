package api

import (
	_ "embed"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// scalarJS is the self-hosted Scalar API reference bundle. Pinned to
// @scalar/api-reference@1.59.3 (standalone). Re-pin by replacing the file under
// internal/api/static/ and recording the new version here. Self-hosted so the
// playground makes no third-party CDN request, matching the self-hosted fonts.
//
//go:embed static/scalar.js
var scalarJS []byte

// playgroundHTML loads the self-hosted Scalar bundle and points it at the
// huma-generated spec. The standalone script reads the data-url off the
// #api-reference script tag.
const playgroundHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <meta name="color-scheme" content="dark" />
  <title>go-ledger API playground</title>
</head>
<body>
  <script id="api-reference" data-url="/openapi.json"></script>
  <script src="/playground/scalar.js"></script>
</body>
</html>
`

// RegisterPlayground serves the interactive Scalar playground at /playground
// and its embedded JS asset. The spec it renders comes from huma at
// /openapi.json, so the playground always reflects the live API.
func RegisterPlayground(router chi.Router) {
	router.Get("/playground", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(playgroundHTML))
	})

	router.Get("/playground/scalar.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(scalarJS)
	})
}
