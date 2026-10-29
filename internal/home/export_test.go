package home

import (
	"io/fs"
	"testing"
)

// Test-only access to the package seams, for the external home_test package.

var CheckSocketPathFor = checkSocketPathFor

func SetLstat(t *testing.T, f func(string) (fs.FileInfo, error)) {
	prev := lstat
	lstat = f
	t.Cleanup(func() { lstat = prev })
}

func SetRenameFile(t *testing.T, f func(string, string) error) {
	prev := renameFile
	renameFile = f
	t.Cleanup(func() { renameFile = prev })
}
