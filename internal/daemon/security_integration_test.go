//go:build integration

package daemon

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// IT-041: with the test process umask at 022, the daemon's directories are 0700 and every
// file it creates, including the ledger's -wal and -shm, is 0600.
func TestIT041_FileModes(t *testing.T) {
	checkLeaks(t)
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	h := testhome.NewMissing(t)
	r := startDaemon(t, h, nil)

	for path, want := range map[string]os.FileMode{
		h.Dir:               0o700,
		h.LogsDir():         0o700,
		h.SocketPath():      0o600,
		h.InfoPath():        0o600,
		h.LockPath():        0o600,
		h.DBPath():          0o600,
		h.DBPath() + "-wal": 0o600,
		h.DBPath() + "-shm": 0o600,
		filepath.Join(h.LogsDir(), "harness.log"): 0o600,
	} {
		if got := statMode(t, path); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}
	if st := r.status(t); st.State != "running" {
		t.Errorf("state = %q, want running", st.State)
	}
}

// IT-042: a secret that reaches the logger is redacted in harness.log (and in the stderr
// mirror).
func TestIT042_LogRedaction(t *testing.T) {
	checkLeaks(t)
	h := testhome.New(t)
	r := startDaemon(t, h, nil)

	r.access.Logger.Error("fetch failed: Authorization: Bearer abc123")
	r.access.Logger.Warn("login", "err", "call failed: token=abc123xyz")

	data, err := os.ReadFile(h.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"harness.log": string(data), "stderr mirror": r.stderr.String()} {
		if !strings.Contains(text, "[REDACTED]") {
			t.Errorf("%s has no [REDACTED] marker:\n%s", name, text)
		}
		if strings.Contains(text, "abc123") {
			t.Errorf("%s leaks the secret:\n%s", name, text)
		}
	}
}

// IT-043: the daemon opens no TCP listener. The probe is checked against a control first:
// a TCP listener the test opens itself must show up, so an empty answer means something.
func TestIT043_NoTCPListener(t *testing.T) {
	checkLeaks(t)
	pid := os.Getpid() // the daemon runs in this process

	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	withControl := tcpListeners(t, pid)
	control.Close()
	baseline := tcpListeners(t, pid)
	if len(withControl) <= len(baseline) {
		t.Fatalf("the probe did not see the control listener (with: %v, without: %v)", withControl, baseline)
	}

	h := testhome.New(t)
	r := startDaemon(t, h, nil)
	_ = r

	var added []string
	for _, l := range tcpListeners(t, pid) {
		if !slices.Contains(baseline, l) {
			added = append(added, l)
		}
	}
	if len(added) > 0 {
		t.Errorf("the running daemon opened TCP listeners: %v", added)
	}
}

// tcpListeners lists the TCP sockets in LISTEN state for pid: lsof on darwin, the network
// namespace's /proc tables on linux.
func tcpListeners(t *testing.T, pid int) []string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-nP")
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		if err != nil {
			// lsof exits 1 when it finds nothing; anything else is a broken probe.
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || out.Len() != 0 {
				t.Fatalf("lsof: %v\n%s", err, out.String())
			}
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "COMMAND") {
			lines = lines[1:]
		}
		return nonEmpty(lines)
	case "linux":
		var listeners []string
		for _, table := range []string{"tcp", "tcp6"} {
			data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/net/" + table)
			if err != nil {
				t.Fatalf("read %s table: %v", table, err)
			}
			for _, line := range strings.Split(string(data), "\n")[1:] {
				fields := strings.Fields(line)
				if len(fields) > 3 && fields[3] == "0A" { // st == TCP_LISTEN
					listeners = append(listeners, table+" "+fields[1])
				}
			}
		}
		return listeners
	}
	t.Skipf("no TCP listener probe for %s", runtime.GOOS)
	return nil
}

func nonEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
