// Package home resolves the harness home directory and owns the rules for the files
// inside it: the socket-path limit, home privacy, the single-instance lock and the
// atomic daemon.json. It is a leaf package (see the boundaries table in the spec).
package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// EnvVar overrides the default home location.
const EnvVar = "HARNESS_HOME"

// Contract codes carried by *Error. They match internal/api/contract.
const (
	CodeSocketPathTooLong = "socket_path_too_long"
	CodeHomeInsecure      = "home_insecure"
	CodeAlreadyRunning    = "daemon_already_running"
)

// Error is a home failure with a stable contract code.
type Error struct {
	Code    string
	Message string
	Hint    string
	PID     int // holder PID for CodeAlreadyRunning; 0 when unknown
}

func (e *Error) Error() string { return e.Message }

// Home is a resolved home directory. Dir is always a cleaned absolute path.
type Home struct {
	Dir string
}

// Resolve picks the home from the --home flag, then $HARNESS_HOME, then ~/.harness.
func Resolve(flag string) (Home, error) {
	dir := flag
	if dir == "" {
		dir = os.Getenv(EnvVar)
	}
	if dir == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Home{}, fmt.Errorf("resolve home: %w", err)
		}
		dir = filepath.Join(userHome, ".harness")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Home{}, fmt.Errorf("resolve home %q: %w", dir, err)
	}
	return Home{Dir: abs}, nil
}

func (h Home) SocketPath() string    { return filepath.Join(h.Dir, "daemon.sock") }
func (h Home) LockPath() string      { return filepath.Join(h.Dir, "daemon.lock") }
func (h Home) InfoPath() string      { return filepath.Join(h.Dir, "daemon.json") }
func (h Home) ConfigPath() string    { return filepath.Join(h.Dir, "config.toml") }
func (h Home) DBPath() string        { return filepath.Join(h.Dir, "harness.db") }
func (h Home) LogsDir() string       { return filepath.Join(h.Dir, "logs") }
func (h Home) LogPath() string       { return filepath.Join(h.LogsDir(), "harness.log") }
func (h Home) DaemonOutPath() string { return filepath.Join(h.LogsDir(), "daemon.out") }

// CheckSocketPath fails when the socket path does not fit the OS limit
// (103 bytes on macOS, 107 on Linux; the sun_path buffers include the NUL).
func (h Home) CheckSocketPath() error {
	return checkSocketPathFor(runtime.GOOS, h.SocketPath())
}

func maxSocketPathLen(goos string) int {
	if goos == "linux" {
		return 107
	}
	return 103
}

func checkSocketPathFor(goos, path string) error {
	if len(path) <= maxSocketPathLen(goos) {
		return nil
	}
	return &Error{
		Code:    CodeSocketPathTooLong,
		Message: fmt.Sprintf("socket path %s is %d bytes; at most 103 bytes on macOS and 107 on Linux", path, len(path)),
		Hint:    "set HARNESS_HOME to a shorter directory",
	}
}

// EnsureLayout creates the home and logs/ at mode 0700 when they are missing. It never
// changes the mode of a directory that already exists.
func (h Home) EnsureLayout() error {
	for _, dir := range []string{h.Dir, h.LogsDir()} {
		if err := ensureDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureDir(dir string) error {
	_, err := os.Stat(dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll is subject to the umask; make the created directory exactly 0700.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}
