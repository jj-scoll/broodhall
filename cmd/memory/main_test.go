package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testExtDir is where the signed DuckDB extensions (fts, httpfs, quack) are read from during
// tests: MEMORY_EXT_DIR if set, else the production default. It is fixed before any test
// changes MEMORY_DIR, which would otherwise move the default.
var testExtDir string

func TestMain(m *testing.M) {
	testExtDir = memdExtDir()
	os.Setenv("MEMORY_EXT_DIR", testExtDir)
	for _, name := range []string{"fts", "httpfs", "quack"} {
		if _, err := os.Stat(filepath.Join(testExtDir, name+".duckdb_extension")); err != nil {
			fmt.Fprintf(os.Stderr, "memory tests need the signed DuckDB extensions fts, httpfs and quack in %s (set MEMORY_EXT_DIR): %v\n", testExtDir, err)
			os.Exit(1)
		}
	}
	// No test reads the user's settings file.
	os.Setenv("MEMORY_SETTINGS", "/nonexistent/memory-settings.json")
	os.Exit(m.Run())
}
