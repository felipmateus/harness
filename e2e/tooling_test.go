//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// doctorReport decodes the {"doctor":{...}} document of a `doctor -o json` run.
type doctorReport struct {
	Doctor struct {
		Status string `json:"status"`
		Source string `json:"source"`
		Items  []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
			Fix      string `json:"fix"`
		} `json:"items"`
		Summary map[string]int `json:"summary"`
	} `json:"doctor"`
}

func parseDoctor(t *testing.T, res result) doctorReport {
	t.Helper()
	var rep doctorReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("stdout %q is not a doctor report: %v\nstderr %q", res.stdout, err, res.stderr)
	}
	return rep
}

func (r doctorReport) item(t *testing.T, id string) (severity, message string, ok bool) {
	t.Helper()
	for _, it := range r.Doctor.Items {
		if it.ID == id {
			return it.Severity, it.Message, true
		}
	}
	return "", "", false
}

func (r doctorReport) ids() []string {
	var out []string
	for _, it := range r.Doctor.Items {
		out = append(out, it.ID)
	}
	return out
}

// E2E-003: a config with a misspelt key and a bad level stops the start and is listed in
// full by `config validate`.
func TestE2E003_InvalidConfig(t *testing.T) {
	h := newHome(t)
	cfg := "[daemon]\nshutdown_grase = \"5s\"\n[log]\nlevel = \"verbose\"\n"
	if err := os.WriteFile(h.ConfigPath(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	res := harness(t, h, "daemon", "start")
	if res.exit != 78 || res.stdout != "" || !strings.HasPrefix(res.stderr, "error: config invalid:") {
		t.Fatalf("start: exit %d stdout %q stderr %q; want 78 and an error: config invalid: line", res.exit, res.stdout, res.stderr)
	}
	if free, _ := h.LockFree(); !free {
		t.Error("the refused start left a daemon holding the home")
	}

	res = harness(t, h, "config", "validate", "-o", "json")
	if res.exit != 78 || res.stderr != "" {
		t.Fatalf("validate: exit %d stderr %q; want 78 and empty stderr", res.exit, res.stderr)
	}
	var doc struct {
		Config struct {
			Path   string `json:"path"`
			Valid  bool   `json:"valid"`
			Errors []struct {
				Key, Code, Message string
			} `json:"errors"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil {
		t.Fatalf("stdout %q: %v", res.stdout, err)
	}
	if doc.Config.Valid || len(doc.Config.Errors) != 2 || doc.Config.Path != h.ConfigPath() {
		t.Fatalf("report %+v, want invalid with 2 errors at %s", doc.Config, h.ConfigPath())
	}
	if e := doc.Config.Errors; e[0].Key != "daemon.shutdown_grase" || e[0].Code != "unknown_key" ||
		e[1].Key != "log.level" || e[1].Code != "invalid_value" {
		t.Errorf("errors %+v", e)
	}
}

// E2E-009: a healthy daemon answers its own doctor.
func TestE2E009_DoctorHealthy(t *testing.T) {
	h := newHome(t)
	start(t, h)

	res := harness(t, h, "doctor")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if len(lines) != 7 || lines[6] != "summary: 6 ok, 0 warn, 0 error" {
		t.Fatalf("stdout:\n%s", res.stdout)
	}
	for i, id := range []string{"home.permissions", "socket.permissions", "config.valid", "ledger.integrity", "ledger.schema", "log.writable"} {
		if !strings.HasPrefix(lines[i], "OK    "+id) {
			t.Errorf("line %d = %q, want OK %s", i+1, lines[i], id)
		}
	}

	res = harness(t, h, "doctor", "-o", "json")
	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("json: exit %d, stderr %q", res.exit, res.stderr)
	}
	rep := parseDoctor(t, res)
	if rep.Doctor.Source != "daemon" || rep.Doctor.Status != "ok" || rep.Doctor.Summary["ok"] != 6 {
		t.Errorf("report %+v", rep.Doctor)
	}
	if !strings.Contains(res.stdout, `"source":"daemon"`) {
		t.Errorf("stdout %q has no \"source\":\"daemon\"", res.stdout)
	}
}

// E2E-010: a home that others can read is reported with a fix, exit 70, and left alone.
func TestE2E010_LooseHomeIsReportedNotFixed(t *testing.T) {
	h := newHome(t)
	start(t, h)
	t.Cleanup(func() { os.Chmod(h.Dir, 0o700) }) // runs before the cleanup that stops the daemon
	if err := os.Chmod(h.Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	res := harness(t, h, "doctor", "-o", "json")

	if res.exit != 70 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q; want 70 and empty stderr", res.exit, res.stderr)
	}
	rep := parseDoctor(t, res)
	sev, msg, ok := rep.item(t, "home.permissions")
	if !ok || sev != "error" || msg != h.Dir+" is 0755; expected 0700" {
		t.Errorf("home.permissions = %q %q", sev, msg)
	}
	for _, it := range rep.Doctor.Items {
		if it.ID == "home.permissions" && it.Fix != "chmod 700 "+h.Dir {
			t.Errorf("fix = %q", it.Fix)
		}
	}
	if rep.Doctor.Status != "error" || rep.Doctor.Summary["error"] != 1 || rep.Doctor.Source != "daemon" {
		t.Errorf("report %+v", rep.Doctor)
	}
	if fi, err := os.Stat(h.Dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the home is %v after doctor (%v); doctor must not change it", fi.Mode().Perm(), err)
	}

	res = harness(t, h, "doctor")
	if res.exit != 70 || !strings.Contains(res.stdout, "ERROR home.permissions") ||
		!strings.Contains(res.stdout, "      fix: chmod 700 "+h.Dir+"\n") {
		t.Errorf("human: exit %d stdout %q", res.exit, res.stdout)
	}
}

// E2E-012: with no config file the defaults are valid and shown.
func TestE2E012_ConfigValidateAndShowWithoutAFile(t *testing.T) {
	h := newHome(t)

	res := harness(t, h, "config", "validate")
	if res.exit != 0 || res.stderr != "" || !strings.HasPrefix(res.stdout, "valid: "+h.ConfigPath()) {
		t.Errorf("validate: exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}

	res = harness(t, h, "config", "show", "-o", "json")
	want := `{"config":{"daemon":{"shutdown_grace":"30s"},"log":{"level":"info"}}}` + "\n"
	if res.exit != 0 || res.stdout != want || res.stderr != "" {
		t.Errorf("show: exit %d stdout %q stderr %q, want %q", res.exit, res.stdout, res.stderr, want)
	}

	if err := os.WriteFile(h.ConfigPath(), []byte("[daemon]\nshutdown_grace = \"2m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = harness(t, h, "config", "show", "-o", "json")
	want = `{"config":{"daemon":{"shutdown_grace":"2m"},"log":{"level":"info"}}}` + "\n"
	if res.exit != 0 || res.stdout != want {
		t.Errorf("show with a file: exit %d stdout %q, want %q", res.exit, res.stdout, want)
	}
}

// E2E-018: the version line and document, with no daemon.
func TestE2E018_Version(t *testing.T) {
	h := newHome(t)

	res := harness(t, h, "version")
	re := regexp.MustCompile(`^harness \S+ \(commit \S+, go\S+, (darwin|linux)/(arm64|amd64)\)\n$`)
	if res.exit != 0 || !re.MatchString(res.stdout) || res.stderr != "" {
		t.Errorf("exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}

	res = harness(t, h, "version", "-o", "json")
	var doc map[string]map[string]string
	if err := json.Unmarshal([]byte(res.stdout), &doc); err != nil || res.exit != 0 {
		t.Fatalf("exit %d stdout %q: %v", res.exit, res.stdout, err)
	}
	keys := make([]string, 0, 4)
	for k := range doc["version"] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"commit", "go", "platform", "version"}) {
		t.Errorf("version keys %v", keys)
	}
}

// E2E-019: the path of the config file, whether or not it exists.
func TestE2E019_ConfigPath(t *testing.T) {
	h := newHome(t)
	res := harness(t, h, "config", "path")
	if res.exit != 0 || res.stdout != h.Dir+"/config.toml\n" || res.stderr != "" {
		t.Errorf("exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}
	if _, err := os.Lstat(h.ConfigPath()); !os.IsNotExist(err) {
		t.Errorf("config path created %s (%v)", h.ConfigPath(), err)
	}
}

// E2E-020: with no daemon the same checks run locally.
func TestE2E020_DoctorWithoutADaemon(t *testing.T) {
	h := newHome(t)

	res := harness(t, h, "doctor", "-o", "json")

	if res.exit != 0 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q", res.exit, res.stderr)
	}
	rep := parseDoctor(t, res)
	if rep.Doctor.Source != "local" {
		t.Errorf("source %q, want local", rep.Doctor.Source)
	}
	if sev, _, ok := rep.item(t, "daemon.reachable"); !ok || sev != "warn" {
		t.Errorf("daemon.reachable = %q (present %v), want warn", sev, ok)
	}
	if _, _, ok := rep.item(t, "socket.permissions"); ok {
		t.Error("a local report has socket.permissions")
	}
	if want := []string{"daemon.reachable", "home.permissions", "config.valid", "ledger.integrity", "ledger.schema", "log.writable"}; !slices.Equal(rep.ids(), want) {
		t.Errorf("items %v, want %v", rep.ids(), want)
	}

	res = harness(t, h, "doctor")
	if res.exit != 0 || !strings.HasPrefix(res.stdout, "WARN  daemon.reachable     no daemon is listening on "+h.SocketPath()+" (checks ran locally)\n") ||
		!strings.HasSuffix(res.stdout, "summary: 5 ok, 1 warn, 0 error\n") {
		t.Errorf("human: exit %d stdout %q", res.exit, res.stdout)
	}
}

// The local doctor on a home a daemon has used must read the ledger without leaving a mark:
// the same files, with the same modes, sizes and times, before and after.
func TestDoctorWithoutADaemonLeavesTheHomeAsItWas(t *testing.T) {
	h := newHome(t)
	start(t, h)
	if res := harness(t, h, "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: %+v", res)
	}
	before := homeFiles(t, h.Dir)
	if !slices.ContainsFunc(before, func(s string) bool { return strings.HasPrefix(s, "harness.db ") }) {
		t.Fatalf("the home has no ledger: %v", before)
	}

	res := harness(t, h, "doctor", "-o", "json")

	if res.exit != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", res.exit, res.stdout, res.stderr)
	}
	rep := parseDoctor(t, res)
	if sev, msg, _ := rep.item(t, "ledger.integrity"); sev != "ok" || msg != "quick_check ok" {
		t.Errorf("ledger.integrity = %q %q", sev, msg)
	}
	if after := homeFiles(t, h.Dir); !slices.Equal(before, after) {
		t.Errorf("doctor changed the home:\n before %v\n after  %v", before, after)
	}
}

// E2E-010's local form (UT-076): the daemon cannot read its ledger, doctor still says why.
func TestDoctorLocalReportsAnUnreadableLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	h := newHome(t)
	start(t, h)
	if res := harness(t, h, "daemon", "stop"); res.exit != 0 {
		t.Fatalf("stop: %+v", res)
	}
	if err := os.Chmod(h.DBPath(), 0o000); err != nil {
		t.Fatal(err)
	}

	res := harness(t, h, "doctor", "-o", "json")

	if res.exit != 70 || res.stderr != "" {
		t.Fatalf("exit %d, stderr %q; want 70 and empty stderr", res.exit, res.stderr)
	}
	rep := parseDoctor(t, res)
	if sev, _, _ := rep.item(t, "ledger.integrity"); sev != "error" || rep.Doctor.Source != "local" {
		t.Errorf("report %+v", rep.Doctor)
	}
}

// homeFiles describes every entry under dir: relative path, mode, size and modification time.
func homeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, strings.Join([]string{rel, fi.Mode().String(), fi.ModTime().UTC().Format("15:04:05.000000000")}, " ")+" "+sizeOf(fi))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sizeOf(fi os.FileInfo) string {
	if fi.IsDir() {
		return "-"
	}
	return strconv.FormatInt(fi.Size(), 10)
}
