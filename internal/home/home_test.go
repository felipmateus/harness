package home_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// The helper process holds the lock of the home named by the environment until its
// stdin closes. It runs before the tests when the variable is set.
const helperEnv = "HARNESS_HOME_TEST_HELPER_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperEnv); dir != "" {
		lock, err := home.Home{Dir: dir}.AcquireLock()
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		lock.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// holdLock starts a helper process holding h's lock and returns its PID and a function
// that makes it release the lock and exit.
func holdLock(t *testing.T, h home.Home) (int, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+h.Dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not get the lock: %q %v", line, err)
	}
	release := func() {
		stdin.Close()
		cmd.Wait()
	}
	t.Cleanup(release)
	return cmd.Process.Pid, release
}

func homeError(t *testing.T, err error, code string) *home.Error {
	t.Helper()
	var he *home.Error
	if !errors.As(err, &he) || he.Code != code {
		t.Fatalf("want *home.Error with code %s, got %v", code, err)
	}
	return he
}

// UT-010
func TestCheckSocketPath_Limits(t *testing.T) {
	path := func(n int) string { return "/" + strings.Repeat("a", n-1) }
	cases := []struct {
		goos string
		len  int
		ok   bool
	}{
		{"darwin", 103, true},
		{"darwin", 104, false},
		{"linux", 107, true},
		{"linux", 108, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.goos, tc.len), func(t *testing.T) {
			err := home.CheckSocketPathFor(tc.goos, path(tc.len))
			if tc.ok {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			he := homeError(t, err, home.CodeSocketPathTooLong)
			want := fmt.Sprintf("is %d bytes; at most 103 bytes on macOS and 107 on Linux", tc.len)
			if !strings.HasSuffix(he.Message, want) {
				t.Fatalf("message %q does not end with %q", he.Message, want)
			}
		})
	}
}

// UT-011
func TestEnsureLayout_CreatesPrivateDirs(t *testing.T) {
	h := testhome.NewMissing(t)
	if err := h.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{h.Dir, h.LogsDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %04o, want 0700", dir, fi.Mode().Perm())
		}
	}
}

// UT-012
func TestAcquireLock_SecondOwnerGetsHolderPID(t *testing.T) {
	h := testhome.New(t)
	pid, _ := holdLock(t, h)
	_, err := h.AcquireLock()
	he := homeError(t, err, home.CodeAlreadyRunning)
	if he.PID != pid {
		t.Fatalf("holder pid = %d, want %d", he.PID, pid)
	}
	if !strings.Contains(he.Message, "daemon already running for "+h.Dir+" (pid "+strconv.Itoa(pid)+")") {
		t.Fatalf("message = %q", he.Message)
	}
}

// UT-014
func TestWriteInfo_IsAtomic(t *testing.T) {
	h := testhome.New(t)
	old := home.Info{PID: 1, Version: "old", Socket: h.SocketPath(), StartedAt: "2026-10-28T22:10:03.140Z"}
	if err := h.WriteInfo(old); err != nil {
		t.Fatal(err)
	}
	home.SetRenameFile(t, func(string, string) error { return errors.New("crash before rename") })
	if err := h.WriteInfo(home.Info{PID: 2, Version: "new"}); err == nil {
		t.Fatal("want an error when the rename fails")
	}
	got, err := h.ReadInfo()
	if err != nil {
		t.Fatalf("old daemon.json unreadable after failed write: %v", err)
	}
	if got != old {
		t.Fatalf("daemon.json = %+v, want the old content %+v", got, old)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(h.Dir, ".daemon.json.*")); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestWriteInfo_SuccessIsReadable(t *testing.T) {
	h := testhome.New(t)
	want := home.Info{PID: 7, Version: "0.1.0-dev", Socket: h.SocketPath(), StartedAt: "2026-10-28T22:10:03.140Z"}
	if err := h.WriteInfo(want); err != nil {
		t.Fatal(err)
	}
	got, err := h.ReadInfo()
	if err != nil || got != want {
		t.Fatalf("ReadInfo = %+v, %v; want %+v", got, err, want)
	}
	fi, err := os.Stat(h.InfoPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon.json mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

// UT-015
func TestResolve_Precedence(t *testing.T) {
	fakeUserHome := t.TempDir()
	t.Setenv("HOME", fakeUserHome)

	t.Setenv(home.EnvVar, "")
	got, err := home.Resolve("")
	if err != nil || got.Dir != filepath.Join(fakeUserHome, ".harness") {
		t.Fatalf("default = %q, %v", got.Dir, err)
	}

	t.Setenv(home.EnvVar, "/tmp/from-env")
	got, _ = home.Resolve("")
	if got.Dir != "/tmp/from-env" {
		t.Fatalf("env = %q", got.Dir)
	}

	got, _ = home.Resolve("/tmp/from-flag")
	if got.Dir != "/tmp/from-flag" {
		t.Fatalf("flag = %q", got.Dir)
	}
}

type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "h" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

// UT-016
func TestCheckSecurity(t *testing.T) {
	t.Run("modes", func(t *testing.T) {
		cases := []struct {
			mode fs.FileMode
			ok   bool
		}{
			{0o700, true},
			{0o755, true},
			{0o770, false},
			{0o777, false},
		}
		for _, tc := range cases {
			h := testhome.New(t)
			if err := os.Chmod(h.Dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			err := h.CheckSecurity()
			if tc.ok && err != nil {
				t.Fatalf("mode %04o: want ok, got %v", tc.mode, err)
			}
			if !tc.ok {
				he := homeError(t, err, home.CodeHomeInsecure)
				want := fmt.Sprintf("home %s is not private: mode %04o is writable by group or others", h.Dir, tc.mode)
				if he.Message != want || he.Hint != "chmod 700 "+h.Dir {
					t.Fatalf("got %q / %q", he.Message, he.Hint)
				}
			}
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := testhome.New(t)
		link := filepath.Join(testhome.New(t).Dir, "link")
		if err := os.Symlink(target.Dir, link); err != nil {
			t.Fatal(err)
		}
		homeError(t, home.Home{Dir: link}.CheckSecurity(), home.CodeHomeInsecure)
	})
	t.Run("foreign owner", func(t *testing.T) {
		home.SetLstat(t, func(string) (fs.FileInfo, error) {
			return fakeInfo{mode: fs.ModeDir | 0o700, uid: uint32(os.Geteuid()) + 1}, nil
		})
		homeError(t, home.Home{Dir: "/tmp/hz-foreign"}.CheckSecurity(), home.CodeHomeInsecure)
	})
	t.Run("missing passes", func(t *testing.T) {
		if err := testhome.NewMissing(t).CheckSecurity(); err != nil {
			t.Fatalf("missing home: %v", err)
		}
	})
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func readLockFile(t *testing.T, h home.Home) string {
	t.Helper()
	data, err := os.ReadFile(h.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// UT-017
func TestLockFile_LoserNeverModifiesIt(t *testing.T) {
	h := testhome.New(t)
	pid, release := holdLock(t, h)
	before := inode(t, h.LockPath())
	if readLockFile(t, h) != strconv.Itoa(pid) {
		t.Fatalf("owner pid not recorded: %q", readLockFile(t, h))
	}

	if _, err := h.AcquireLock(); err == nil {
		t.Fatal("loser acquired a held lock")
	}
	if got := readLockFile(t, h); got != strconv.Itoa(pid) {
		t.Fatalf("lock file changed by loser: %q, want %d", got, pid)
	}
	if inode(t, h.LockPath()) != before {
		t.Fatal("lock file inode changed by loser")
	}

	release()
	lock, err := h.AcquireLock()
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	defer lock.Release()
	if got := readLockFile(t, h); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("new owner pid = %q, want %d", got, os.Getpid())
	}
	if inode(t, h.LockPath()) != before {
		t.Fatal("lock file inode changed by new owner")
	}
}

// UT-018
func TestLockFree_Probe(t *testing.T) {
	h := testhome.New(t)

	free, err := h.LockFree()
	if err != nil || !free {
		t.Fatalf("no lock file: free=%v err=%v", free, err)
	}
	if _, err := os.Stat(h.LockPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("probe created the lock file")
	}

	_, release := holdLock(t, h)
	if free, err := h.LockFree(); err != nil || free {
		t.Fatalf("held lock: free=%v err=%v", free, err)
	}

	release()
	if free, err := h.LockFree(); err != nil || !free {
		t.Fatalf("released lock: free=%v err=%v", free, err)
	}
	lock, err := h.AcquireLock()
	if err != nil {
		t.Fatalf("acquire right after a free probe: %v", err)
	}
	lock.Release()
}

// UT-019
func TestResolve_CleansAndMeasuresTheCleanPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := home.Resolve("./a/../b")
	if err != nil || got.Dir != filepath.Join(wd, "b") {
		t.Fatalf("Resolve = %q, %v; want %q", got.Dir, err, filepath.Join(wd, "b"))
	}

	// The raw path is far over the limit; the cleaned one is short.
	raw := "/tmp/" + strings.Repeat("x/../", 40) + "hz"
	h, err := home.Resolve(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.Dir != "/tmp/hz" {
		t.Fatalf("Resolve(%q) = %q", raw, h.Dir)
	}
	if err := h.CheckSocketPath(); err != nil {
		t.Fatalf("cleaned path measured as too long: %v", err)
	}
}
