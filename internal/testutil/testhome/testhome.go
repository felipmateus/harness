// Package testhome creates temporary harness homes for tests. Paths live under /tmp so
// the socket path stays far below the 103-byte macOS limit; t.TempDir() paths on macOS
// are too long. Tests must never use the operator's real ~/.harness.
package testhome

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/felipmateus/harness/internal/home"
)

// New returns an existing, private (0700) home that is removed when the test ends.
func New(t testing.TB) home.Home {
	t.Helper()
	dir := mkdir(t)
	return home.Home{Dir: dir}
}

// NewMissing returns a home path that does not exist yet, inside a private temporary
// parent that is removed when the test ends.
func NewMissing(t testing.TB) home.Home {
	t.Helper()
	return home.Home{Dir: filepath.Join(mkdir(t), "h")}
}

func mkdir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hz")
	if err != nil {
		t.Fatalf("testhome: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("testhome: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
