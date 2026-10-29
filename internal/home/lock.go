package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Lock is the single-instance lock on <home>/daemon.lock, held through flock.
//
// Rules (spec invariant 2): the lock file is never removed, renamed or replaced; a
// process that does not hold the lock never modifies it; the owner writes its PID in
// place only after acquiring the lock.
type Lock struct {
	f *os.File
}

// AcquireLock takes the lock without blocking. When another process holds it, it
// returns *Error with CodeAlreadyRunning and the holder PID read from the file.
func (h Home) AcquireLock() (*Lock, error) {
	f, err := os.OpenFile(h.LockPath(), os.O_CREATE|os.O_RDWR, 0o600) // never O_TRUNC
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", h.LockPath(), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &Error{
				Code:    CodeAlreadyRunning,
				Message: fmt.Sprintf("daemon already running for %s (%s)", h.Dir, describePID(pid)),
				PID:     pid,
			}
		}
		return nil, fmt.Errorf("lock %s: %w", h.LockPath(), err)
	}
	if err := writePID(f, os.Getpid()); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("record pid in %s: %w", h.LockPath(), err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. The file itself stays.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	return errors.Join(unlockErr, closeErr)
}

// LockFree reports whether no process holds the lock. It probes on a read-only
// descriptor and releases at once; it never creates or modifies the lock file.
func (h Home) LockFree() (bool, error) {
	f, err := os.Open(h.LockPath())
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("open lock %s: %w", h.LockPath(), err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, fmt.Errorf("probe lock %s: %w", h.LockPath(), err)
}

// LockPID reads the PID recorded in the lock file; 0 when absent or unreadable.
func (h Home) LockPID() int {
	f, err := os.Open(h.LockPath())
	if err != nil {
		return 0
	}
	defer f.Close()
	return readPID(f)
}

func readPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid < 0 {
		return 0
	}
	return pid
}

func writePID(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		return err
	}
	return f.Sync()
}

func describePID(pid int) string {
	if pid == 0 {
		return "pid unknown"
	}
	return "pid " + strconv.Itoa(pid)
}
