package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecYAML(t *testing.T) {
	spec, err := SpecYAML()
	if err != nil {
		t.Fatalf("SpecYAML: %v", err)
	}
	if len(spec) == 0 {
		t.Fatal("empty spec")
	}
	if !strings.Contains(string(spec), "/healthz") {
		t.Error("spec missing /healthz path")
	}
}

// TestOpenAPISpecUpToDate is the day-by-day drift guard: the committed snapshot
// at api/openapi.yaml must equal what the live handlers generate. Adding or
// changing an operation without running `make openapi` fails here, and the test
// suite gates deploys.
func TestOpenAPISpecUpToDate(t *testing.T) {
	generated, err := SpecYAML()
	if err != nil {
		t.Fatalf("SpecYAML: %v", err)
	}

	committedPath := filepath.Join("..", "..", "api", "openapi.yaml")
	committed, err := os.ReadFile(committedPath) //nolint:gosec // fixed in-repo path built from constants
	if err != nil {
		t.Fatalf("read %s: %v", committedPath, err)
	}

	if !bytes.Equal(generated, committed) {
		t.Fatal("api/openapi.yaml is out of date with the API handlers. " +
			"Run `make openapi` and commit the result.")
	}
}
