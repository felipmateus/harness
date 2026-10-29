package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/felipmateus/harness/internal/home"
)

// spawnSpec describes the detached daemon process to start.
type spawnSpec struct {
	Exe  string
	Args []string
	Dir  string
	// Out is where the child's stdout and stderr go: logs/daemon.out, opened O_APPEND.
	Out *os.File
}

// process is a spawned child.
type process interface {
	PID() int
	// Done is closed once the child has exited and been reaped.
	Done() <-chan struct{}
	// ExitCode is valid after Done is closed; it is -1 for a child killed by a signal.
	ExitCode() int
	// Signal sends sig to the child's whole process group.
	Signal(sig syscall.Signal) error
}

// spawner starts the detached daemon process.
type spawner interface {
	Spawn(spec spawnSpec) (process, error)
}

// execSpawner starts a real child in its own session, so it outlives the CLI and a signal
// sent to the terminal's foreground group does not reach it.
type execSpawner struct{}

func (execSpawner) Spawn(spec spawnSpec) (process, error) {
	cmd := exec.Command(spec.Exe, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = nil // the null device
	cmd.Stdout = spec.Out
	cmd.Stderr = spec.Out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		p.code = cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	return p, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (p *execProcess) PID() int              { return p.cmd.Process.Pid }
func (p *execProcess) Done() <-chan struct{} { return p.done }
func (p *execProcess) ExitCode() int         { return p.code }
func (p *execProcess) Signal(sig syscall.Signal) error {
	// Setsid made the child a session and group leader, so its group ID is its PID.
	return syscall.Kill(-p.cmd.Process.Pid, sig)
}

// homeProbe answers the questions the CLI asks of the home's files while it waits for a
// daemon: is the lock free, who holds it, is the socket still there.
type homeProbe interface {
	LockFree() (bool, error)
	LockPID() int
	SocketExists() bool
}

// fsProbe reads the real files of a home.
type fsProbe struct{ h home.Home }

func (p fsProbe) LockFree() (bool, error) { return p.h.LockFree() }
func (p fsProbe) LockPID() int            { return p.h.LockPID() }
func (p fsProbe) SocketExists() bool {
	_, err := os.Lstat(p.h.SocketPath())
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// openDaemonOut opens logs/daemon.out for appending and returns it with its current size,
// where this start's output begins. The file is never truncated.
func openDaemonOut(h home.Home) (*os.File, int64, error) {
	f, err := os.OpenFile(h.DaemonOutPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", h.DaemonOutPath(), err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat %s: %w", h.DaemonOutPath(), err)
	}
	return f, fi.Size(), nil
}

// tailLines is how many lines of the daemon's output a start failure reports.
const tailLines = 20

// maxTailRead bounds how much of daemon.out is read for the tail.
const maxTailRead = 256 << 10

// readOutput returns the lines written to path at or after offset: the whole of this
// start's output, or its last maxTailRead bytes when that is more. A missing or unreadable
// file reads as no output.
func readOutput(path string, offset int64) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return []string{}
	}
	start := offset
	if fi.Size()-start > maxTailRead {
		start = fi.Size() - maxTailRead
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return []string{}
	}
	lines := splitLines(string(buf))
	if start > offset && len(lines) > 0 {
		lines = lines[1:] // the first line of a partial read is cut
	}
	return lines
}

// readTail returns the last n lines written to path at or after offset.
func readTail(path string, offset int64, n int) []string {
	lines := readOutput(path, offset)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// splitLines splits on newlines and drops the empty line a trailing newline leaves.
func splitLines(s string) []string {
	lines := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
