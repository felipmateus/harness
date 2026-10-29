//go:build e2e

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/home"
	_ "modernc.org/sqlite" // the driver, to fake a newer schema
)

// E2E-014: a ledger written by a newer harness is refused with its own code, and the file is
// left byte for byte as it was.
func TestE2E014_SchemaAheadIsRefusedAndTheFileIsUntouched(t *testing.T) {
	h := newHome(t)
	start(t, h)
	if res := harness(t, h, "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: %+v", res)
	}
	setGooseVersion(t, h, 99)
	before := sha256sum(t, h.DBPath())

	res := harness(t, h, "daemon", "start", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 69", res.exit, res.stdout, res.stderr)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "ledger_schema_ahead" || !strings.Contains(obj["message"].(string), "99") {
		t.Errorf("error %v", obj)
	}
	if _, has := obj["details"]; has {
		t.Errorf("error %v: a recognised failure carries no details", obj)
	}
	if after := sha256sum(t, h.DBPath()); after != before {
		t.Error("the refused database file was modified")
	}
	if free, _ := h.LockFree(); !free {
		t.Error("the refused start left the home locked")
	}
}

// setGooseVersion records a migration version in the ledger's goose table, as a newer binary
// would have, and checkpoints so the main file alone holds it.
func setGooseVersion(t *testing.T, h home.Home, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", h.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, version); err != nil {
		t.Fatalf("set the schema version: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
}

func sha256sum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// E2E-021: a home that others can write is refused before anything is spawned.
func TestE2E021_InsecureHomeIsRefused(t *testing.T) {
	h := newHome(t)
	if err := os.Chmod(h.Dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.Dir, 0o700) })

	res := harness(t, h, "daemon", "start", "-o", "json")
	if res.exit != 78 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 78", res.exit, res.stdout, res.stderr)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "home_insecure" || obj["hint"] != "chmod 700 "+h.Dir {
		t.Errorf("error %v", obj)
	}
	if pid := h.LockPID(); pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Errorf("a daemon process (pid %d) exists for the refused home", pid)
	}
	for _, name := range []string{"daemon.sock", "daemon.json", "harness.db", "logs"} {
		if _, err := os.Lstat(h.Dir + "/" + name); err == nil {
			t.Errorf("%s was created in a home that was refused", name)
		}
	}
}

// E2E-022: a daemon that dies for a reason the CLI does not recognise is reported as
// daemon_start_failed with its PID, exit code and output.
func TestE2E022_StartFailureCarriesDetails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	h := newHome(t)
	start(t, h)
	if res := harness(t, h, "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: %+v", res)
	}
	if err := os.Chmod(h.DBPath(), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.DBPath(), 0o600) })

	res := harness(t, h, "daemon", "start", "-o", "json")
	if res.exit != 69 || res.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 69", res.exit, res.stdout, res.stderr)
	}
	obj := errorObject(t, res.stderr)
	if obj["code"] != "daemon_start_failed" {
		t.Fatalf("error %v", obj)
	}
	details, _ := obj["details"].(map[string]any)
	pid, _ := details["pid"].(float64)
	exit, hasExit := details["exit_code"].(float64)
	tail, _ := details["output_tail"].([]any)
	if pid <= 0 || !hasExit || exit != 1 || len(tail) == 0 {
		t.Fatalf("details %v: want pid, exit_code 1 and a non-empty output_tail", details)
	}
	var mentionsLedger bool
	for _, line := range tail {
		if strings.Contains(line.(string), "ledger") {
			mentionsLedger = true
		}
	}
	if !mentionsLedger {
		t.Errorf("no tail line mentions the ledger: %v", tail)
	}
	if want := "daemon exited during startup (pid " + jsonInt(pid) + ", exit 1)"; obj["message"] != want {
		t.Errorf("message %q, want %q", obj["message"], want)
	}
	waitFor(t, "the failed daemon to be gone", 3*time.Second, func() bool {
		return syscall.Kill(int(pid), 0) != nil
	})
}

func jsonInt(f float64) string {
	data, _ := json.Marshal(int(f))
	return string(data)
}
