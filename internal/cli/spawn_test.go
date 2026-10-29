package cli

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// The real spawner, a real process that ignores SIGTERM, and the real clock: stopChild must
// escalate to SIGKILL on the whole process group and return only after the child is reaped.
// The shell also runs a grandchild in the group, which must not outlive it.
func TestStopChildKillsAProcessGroupThatIgnoresSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("takes the 2s SIGTERM grace in real time")
	}
	h := testhome.New(t)
	pidFile := h.Dir + "/grandchild.pid"
	// The shell and its sleep both ignore SIGTERM; the shell prints the sleep's PID first.
	script := `trap '' TERM; sleep 120 & echo $! > ` + pidFile + `; wait`
	out, err := os.CreateTemp(h.Dir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	proc, err := execSpawner{}.Spawn(spawnSpec{Exe: "/bin/sh", Args: []string{"-c", script}, Dir: h.Dir, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	var grandchild int
	deadline := time.Now().Add(10 * time.Second)
	for grandchild == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("the shell did not start its child")
	}

	a := newApp(nil, nil)
	began := time.Now()
	a.stopChild(proc)
	took := time.Since(began)

	select {
	case <-proc.Done():
	default:
		t.Fatal("stopChild returned before the child was reaped")
	}
	if code := proc.ExitCode(); code != -1 {
		t.Errorf("exit code %d, want -1 (killed by a signal)", code)
	}
	if took < termGrace || took > termGrace+3*time.Second {
		t.Errorf("stopChild took %v, want about the %v SIGTERM grace", took, termGrace)
	}
	deadline = time.Now().Add(5 * time.Second)
	for err := error(nil); ; time.Sleep(10 * time.Millisecond) {
		if err = syscall.Kill(grandchild, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatalf("the grandchild %d outlived the group kill", grandchild)
		}
	}
}

// A child that exits on SIGTERM is not killed, and stopChild returns as soon as it is gone.
func TestStopChildReturnsAsSoonAsSIGTERMIsHonoured(t *testing.T) {
	h := testhome.New(t)
	out, err := os.CreateTemp(h.Dir, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	proc, err := execSpawner{}.Spawn(spawnSpec{Exe: "/bin/sleep", Args: []string{"120"}, Dir: h.Dir, Out: out})
	if err != nil {
		t.Fatal(err)
	}

	a := newApp(nil, nil)
	began := time.Now()
	a.stopChild(proc)

	select {
	case <-proc.Done():
	default:
		t.Fatal("stopChild returned before the child was reaped")
	}
	if took := time.Since(began); took > termGrace {
		t.Errorf("stopChild took %v for a child that honours SIGTERM", took)
	}
}
