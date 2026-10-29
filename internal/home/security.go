package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Seams for tests that need ownership cases a test process cannot create.
var (
	lstat   = os.Lstat
	geteuid = os.Geteuid
)

func ownerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// CheckSecurity refuses a home that other users could tamper with: a symlink, a
// directory owned by someone else, or one writable by group or others. A home that is
// only readable by others is accepted (doctor reports it). A missing home passes;
// EnsureLayout creates it private.
func (h Home) CheckSecurity() error {
	fi, err := lstat(h.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect home %s: %w", h.Dir, err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return h.insecure("it is a symbolic link", "point HARNESS_HOME at the real directory")
	case !fi.IsDir():
		return h.insecure("it is not a directory", "set HARNESS_HOME to a directory")
	}
	if uid, ok := ownerUID(fi); ok && int(uid) != geteuid() {
		return h.insecure(fmt.Sprintf("it is owned by uid %d", uid), "use a directory owned by the current user")
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return h.insecure(fmt.Sprintf("mode %04o is writable by group or others", perm), "chmod 700 "+h.Dir)
	}
	return nil
}

func (h Home) insecure(reason, hint string) error {
	return &Error{
		Code:    CodeHomeInsecure,
		Message: fmt.Sprintf("home %s is not private: %s", h.Dir, reason),
		Hint:    hint,
	}
}
