// Command genopenapi writes the OpenAPI spec to api/openapi.yaml. It is the
// `make openapi` target. The committed snapshot is what PRs diff against and
// what external tooling consumes; the drift test in internal/api keeps it equal
// to the spec the running service generates.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/sohag-pro/go-ledger/internal/api"
)

func main() {
	spec, err := api.SpecYAML()
	if err != nil {
		log.Fatalf("generate openapi: %v", err)
	}

	out := filepath.Join("api", "openapi.yaml")
	if err := os.WriteFile(out, spec, 0o600); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	log.Printf("wrote %s (%d bytes)", out, len(spec))
}
