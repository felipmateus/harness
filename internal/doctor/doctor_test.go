package doctor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver the ledger registers, for the schema-ahead fixture

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/home"
	"github.com/felipmateus/harness/internal/ledger"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// healthyHome builds the home of a daemon that ran: a cleanly closed ledger, a log file, a
// listening socket. It returns the home, the socket's listener (closed with the test) and
// nothing else a probe could depend on.
func healthyHome(t *testing.T) home.Home {
	t.Helper()
	h := testhome.New(t)
	if err := h.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(context.Background(), ledger.Options{Path: h.DBPath()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Append(context.Background(), ledger.NewEvent{Stream: "daemon", Type: "x", Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.LogPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// listen makes the home's socket file, as a running daemon would have it.
func listen(t *testing.T, h home.Home) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(h.SocketPath(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, opts Options) contract.Doctor {
	t.Helper()
	d, err := New(opts).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func byID(t *testing.T, d contract.Doctor, id string) contract.DoctorItem {
	t.Helper()
	for _, it := range d.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", id, d.Items)
	return contract.DoctorItem{}
}

func ids(d contract.Doctor) []string {
	var out []string
	for _, it := range d.Items {
		out = append(out, it.ID)
	}
	return out
}

func asRoot() bool { return os.Geteuid() == 0 }

// UT-070
func TestUT070_DaemonModeHealthy(t *testing.T) {
	h := healthyHome(t)
	listen(t, h)

	d := run(t, Options{Home: h, Mode: ModeDaemon})

	if d.Status != contract.SeverityOK || d.Source != contract.SourceDaemon {
		t.Fatalf("status %q source %q, want ok daemon\n%+v", d.Status, d.Source, d.Items)
	}
	if d.Summary != (contract.DoctorSummary{OK: 6}) {
		t.Fatalf("summary = %+v, want 6 ok", d.Summary)
	}
	want := []string{IDHomePermissions, IDSocketPermissions, IDConfigValid, IDLedgerIntegrity, IDLedgerSchema, IDLogWritable}
	if got := ids(d); !slices.Equal(got, want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	for id, msg := range map[string]string{
		IDHomePermissions:   h.Dir + " is 0700",
		IDSocketPermissions: h.SocketPath() + " is 0600",
		IDConfigValid:       "defaults in use (no config file)",
		IDLedgerIntegrity:   "quick_check ok",
		IDLedgerSchema:      "schema 1 (current)",
		IDLogWritable:       h.LogPath() + " is writable",
	} {
		if it := byID(t, d, id); it.Severity != contract.SeverityOK || it.Message != msg || it.Fix != "" {
			t.Errorf("%s = %+v, want ok %q", id, it, msg)
		}
	}
}

// UT-071
func TestUT071_LooseHomeIsAnError(t *testing.T) {
	for _, mode := range []fs.FileMode{0o755, 0o750, 0o777, 0o500} {
		t.Run(mode.String(), func(t *testing.T) {
			h := healthyHome(t)
			listen(t, h)
			if err := os.Chmod(h.Dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(h.Dir, 0o700) })

			d := run(t, Options{Home: h, Mode: ModeDaemon})

			it := byID(t, d, IDHomePermissions)
			wantMsg := h.Dir + " is " + octal(mode) + "; expected 0700"
			if it.Severity != contract.SeverityError || it.Message != wantMsg || it.Fix != "chmod 700 "+h.Dir {
				t.Fatalf("home.permissions = %+v, want error %q fix chmod 700", it, wantMsg)
			}
			if d.Status != contract.SeverityError || d.Summary.Error != 1 {
				t.Errorf("status %q summary %+v, want error with 1 error", d.Status, d.Summary)
			}
		})
	}
}

func octal(m fs.FileMode) string {
	const digits = "01234567"
	p := uint32(m.Perm())
	return "0" + string([]byte{digits[p>>6&7], digits[p>>3&7], digits[p&7]})
}

func TestHomePermissions_ASymlinkIsNamedByTheDaemonsOwnWords(t *testing.T) {
	real := testhome.New(t)
	link := filepath.Join(testhome.New(t).Dir, "link")
	if err := os.Symlink(real.Dir, link); err != nil {
		t.Fatal(err)
	}

	d := run(t, Options{Home: home.Home{Dir: link}, Mode: ModeLocal})

	it := byID(t, d, IDHomePermissions)
	if it.Severity != contract.SeverityError || !strings.Contains(it.Message, "is not private: it is a symbolic link") || it.Fix == "" {
		t.Fatalf("home.permissions = %+v", it)
	}
}

// snapshot records what a probe could change about a home: the names, and for every entry
// its mode, size, modification time and content.
type snapshot map[string]string

func take(t *testing.T, dir string) snapshot {
	t.Helper()
	s := snapshot{}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		desc := fi.Mode().String() + " " + fi.ModTime().UTC().Format(time.RFC3339Nano)
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			desc += " " + string(sum[:])
		}
		s[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// UT-072
func TestUT072_NoProbeChangesTheHome(t *testing.T) {
	h := healthyHome(t)
	listen(t, h)
	cfg := "[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(h.ConfigPath(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.LockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:other"

	for name, opts := range map[string]Options{
		"daemon": {Home: h, Mode: ModeDaemon, LoadedDigest: &digest},
		"local":  {Home: h, Mode: ModeLocal},
	} {
		t.Run(name, func(t *testing.T) {
			before := take(t, h.Dir)
			run(t, opts)
			run(t, opts) // a second run must not find anything the first one left
			after := take(t, h.Dir)

			for path, desc := range before {
				if after[path] != desc {
					t.Errorf("%s changed:\n before %q\n after  %q", path, desc, after[path])
				}
			}
			for path := range after {
				if _, ok := before[path]; !ok {
					t.Errorf("%s was created", path)
				}
			}
		})
	}
}

// UT-073
func TestUT073_AStuckProbeIsIsolated(t *testing.T) {
	stuck := probe{"stuck.probe", func(ctx context.Context, _ *state) contract.DoctorItem {
		<-ctx.Done()
		return item(contract.SeverityOK, "too late")
	}}
	fine := probe{"fine.probe", func(context.Context, *state) contract.DoctorItem {
		return item(contract.SeverityOK, "fine")
	}}
	r := New(Options{Home: testhome.New(t), Mode: ModeLocal})
	r.probes = []probe{fine, stuck, fine}
	r.probes[2].id = "other.probe"

	start := time.Now()
	d, err := r.Run(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	if it := byID(t, d, "stuck.probe"); it.Severity != contract.SeverityError || it.Message != "timed out after 2s" {
		t.Fatalf("stuck.probe = %+v, want error \"timed out after 2s\"", it)
	}
	for _, id := range []string{"fine.probe", "other.probe"} {
		if it := byID(t, d, id); it.Severity != contract.SeverityOK || it.Message != "fine" {
			t.Errorf("%s = %+v, want it to run", id, it)
		}
	}
	if d.Status != contract.SeverityError || d.Summary != (contract.DoctorSummary{OK: 2, Error: 1}) {
		t.Errorf("status %q summary %+v", d.Status, d.Summary)
	}
	if elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Errorf("the report took %v, want about the 2 s timeout", elapsed)
	}
}

// The probes run in parallel: three that take 1.2 s each finish in about 1.2 s, so the whole
// report is bounded by one probe timeout, not by their sum.
func TestRun_ProbesRunInParallel(t *testing.T) {
	slow := func(id string) probe {
		return probe{id, func(ctx context.Context, _ *state) contract.DoctorItem {
			select {
			case <-time.After(1200 * time.Millisecond):
			case <-ctx.Done():
			}
			return item(contract.SeverityOK, "done")
		}}
	}
	r := New(Options{Home: testhome.New(t), Mode: ModeLocal})
	r.probes = []probe{slow("a"), slow("b"), slow("c")}

	start := time.Now()
	d, err := r.Run(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != contract.SeverityOK {
		t.Fatalf("%+v", d.Items)
	}
	if elapsed > 2400*time.Millisecond {
		t.Errorf("three 1.2 s probes took %v; they must run in parallel", elapsed)
	}
}

func TestRun_ACancelledContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(Options{Home: testhome.New(t), Mode: ModeLocal}).Run(ctx); err == nil {
		t.Fatal("Run on a cancelled context returned a report")
	}
}

// UT-074
func TestUT074_ConfigValid(t *testing.T) {
	loaded := "sha256:loaded"
	valid := "[log]\nlevel = \"debug\"\n"

	cases := []struct {
		name     string
		file     *string // nil: no file
		loaded   *string
		mode     Mode
		severity string
		contains string
		fix      string
	}{
		{"defaults", nil, nil, ModeDaemon, contract.SeverityOK, "defaults in use (no config file)", ""},
		{"same digest", &valid, digestOf(valid), ModeDaemon, contract.SeverityOK, "is valid", ""},
		{"changed after load", &valid, &loaded, ModeDaemon, contract.SeverityWarn, "restart to apply", "run `harness daemon stop` and then `harness daemon start`"},
		{"added after load", &valid, nil, ModeDaemon, contract.SeverityWarn, "restart to apply", "run `harness daemon stop` and then `harness daemon start`"},
		{"removed after load", nil, &loaded, ModeDaemon, contract.SeverityWarn, "restart to apply", "run `harness daemon stop` and then `harness daemon start`"},
		{"broken after load", ptr("[log\n"), &loaded, ModeDaemon, contract.SeverityError, "the next start will fail", "run `harness config validate`"},
		{"unknown key", ptr("[daemon]\nshutdown_grase = \"5s\"\n"), digestOf(valid), ModeDaemon, contract.SeverityError, "unknown key \"daemon.shutdown_grase\"", "run `harness config validate`"},
		{"two problems", ptr("[daemon]\nshutdown_grase = \"5s\"\n[log]\nlevel = \"verbose\"\n"), nil, ModeDaemon, contract.SeverityError, "(and 1 more)", "run `harness config validate`"},
		{"local valid", &valid, nil, ModeLocal, contract.SeverityOK, "is valid", ""},
		{"local invalid", ptr("[log]\nlevel = 3\n"), nil, ModeLocal, contract.SeverityError, "the next start will fail", "run `harness config validate`"},
		{"local ignores the digest", &valid, &loaded, ModeLocal, contract.SeverityOK, "is valid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testhome.New(t)
			if tc.file != nil {
				if err := os.WriteFile(h.ConfigPath(), []byte(*tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			it := byID(t, run(t, Options{Home: h, Mode: tc.mode, LoadedDigest: tc.loaded}), IDConfigValid)
			if it.Severity != tc.severity || !strings.Contains(it.Message, tc.contains) || it.Fix != tc.fix {
				t.Fatalf("config.valid = %+v, want %s containing %q with fix %q", it, tc.severity, tc.contains, tc.fix)
			}
		})
	}
}

func TestConfigValid_AnUnreadableFileIsAnError(t *testing.T) {
	if asRoot() {
		t.Skip("root reads every file")
	}
	h := testhome.New(t)
	if err := os.WriteFile(h.ConfigPath(), []byte("[log]\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDConfigValid)
	if it.Severity != contract.SeverityError || !strings.Contains(it.Message, "cannot be read") || it.Fix == "" {
		t.Fatalf("config.valid = %+v", it)
	}
}

func ptr(s string) *string { return &s }

// digestOf is the digest the daemon records for the bytes of a file.
func digestOf(content string) *string {
	sum := sha256.Sum256([]byte(content))
	d := "sha256:" + hexOf(sum[:])
	return &d
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// UT-075
func TestUT075_LocalMode(t *testing.T) {
	h := healthyHome(t)

	d := run(t, Options{Home: h, Mode: ModeLocal})

	want := []string{IDDaemonReachable, IDHomePermissions, IDConfigValid, IDLedgerIntegrity, IDLedgerSchema, IDLogWritable}
	if got := ids(d); !slices.Equal(got, want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	if d.Source != contract.SourceLocal || d.Status != contract.SeverityWarn {
		t.Fatalf("source %q status %q, want local warn", d.Source, d.Status)
	}
	if d.Summary != (contract.DoctorSummary{OK: 5, Warn: 1}) {
		t.Fatalf("summary = %+v, want 5 ok 1 warn", d.Summary)
	}
	it := byID(t, d, IDDaemonReachable)
	if it.Severity != contract.SeverityWarn || it.Message != "no daemon is listening on "+h.SocketPath()+" (checks ran locally)" {
		t.Fatalf("daemon.reachable = %+v", it)
	}
}

func TestLocalMode_AHomeThatNeverStartedIsHealthy(t *testing.T) {
	for name, h := range map[string]home.Home{
		"empty home":   testhome.New(t),
		"missing home": testhome.NewMissing(t),
	} {
		t.Run(name, func(t *testing.T) {
			d := run(t, Options{Home: h, Mode: ModeLocal})

			if d.Summary.Error != 0 || d.Summary.Warn != 1 || d.Status != contract.SeverityWarn {
				t.Fatalf("%+v", d.Items)
			}
			if _, err := os.Lstat(h.Dir + "/harness.db"); !os.IsNotExist(err) {
				t.Errorf("doctor created the ledger: %v", err)
			}
			if _, err := os.Lstat(h.LogsDir()); !os.IsNotExist(err) {
				t.Errorf("doctor created logs/: %v", err)
			}
		})
	}
}

// UT-076
func TestUT076_UnreadableLedgerIsReportedLocally(t *testing.T) {
	if asRoot() {
		t.Skip("root reads every file")
	}
	h := healthyHome(t)
	if err := os.Chmod(h.DBPath(), 0o000); err != nil {
		t.Fatal(err)
	}

	d := run(t, Options{Home: h, Mode: ModeLocal})

	it := byID(t, d, IDLedgerIntegrity)
	if it.Severity != contract.SeverityError || !strings.Contains(it.Message, "permission denied") || it.Fix != "chmod 600 "+h.DBPath() {
		t.Fatalf("ledger.integrity = %+v", it)
	}
	if d.Status != contract.SeverityError {
		t.Errorf("status %q, want error (the CLI exits 70 on it)", d.Status)
	}
	// One root cause, one error: the schema item points at it.
	if s := byID(t, d, IDLedgerSchema); s.Severity != contract.SeverityWarn || !strings.Contains(s.Message, IDLedgerIntegrity) {
		t.Errorf("ledger.schema = %+v", s)
	}
}

func TestLedger_ACorruptFileIsAnError(t *testing.T) {
	h := testhome.New(t)
	if err := os.WriteFile(h.DBPath(), []byte(strings.Repeat("this is not a database\n", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	d := run(t, Options{Home: h, Mode: ModeLocal})
	if it := byID(t, d, IDLedgerIntegrity); it.Severity != contract.SeverityError || it.Fix == "" {
		t.Fatalf("ledger.integrity = %+v", it)
	}
}

func TestLedger_ASchemaFromANewerBinaryIsAnError(t *testing.T) {
	h := healthyHome(t)
	db, err := sql.Open("sqlite", h.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (3, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	d := run(t, Options{Home: h, Mode: ModeLocal})

	it := byID(t, d, IDLedgerSchema)
	if it.Severity != contract.SeverityError || it.Message != "ledger schema 3 is newer than this binary supports (1)" || it.Fix != "upgrade harness" {
		t.Fatalf("ledger.schema = %+v", it)
	}
	if i := byID(t, d, IDLedgerIntegrity); i.Severity != contract.SeverityOK {
		t.Errorf("ledger.integrity = %+v, want ok", i)
	}
}

func TestLedger_AnOlderSchemaIsAWarning(t *testing.T) {
	h := testhome.New(t)
	db, err := sql.Open("sqlite", h.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDLedgerSchema)
	if it.Severity != contract.SeverityWarn || !strings.Contains(it.Message, "the next start migrates it") {
		t.Fatalf("ledger.schema = %+v", it)
	}
}

func TestSocketPermissions(t *testing.T) {
	h := healthyHome(t)
	listen(t, h)
	if err := os.Chmod(h.SocketPath(), 0o666); err != nil {
		t.Fatal(err)
	}
	it := byID(t, run(t, Options{Home: h, Mode: ModeDaemon}), IDSocketPermissions)
	if it.Severity != contract.SeverityError || it.Message != h.SocketPath()+" is 0666; expected 0600" || it.Fix != "chmod 600 "+h.SocketPath() {
		t.Fatalf("socket.permissions = %+v", it)
	}

	os.Remove(h.SocketPath())
	if it := byID(t, run(t, Options{Home: h, Mode: ModeDaemon}), IDSocketPermissions); it.Severity != contract.SeverityError {
		t.Fatalf("a missing socket under a daemon that answers = %+v, want error", it)
	}
}

func TestLogWritable(t *testing.T) {
	if asRoot() {
		t.Skip("root writes everywhere")
	}
	t.Run("log file read-only", func(t *testing.T) {
		h := healthyHome(t)
		if err := os.Chmod(h.LogPath(), 0o400); err != nil {
			t.Fatal(err)
		}
		it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDLogWritable)
		if it.Severity != contract.SeverityError || it.Fix != "chmod 600 "+h.LogPath() {
			t.Fatalf("log.writable = %+v", it)
		}
	})
	t.Run("no log yet, logs dir read-only", func(t *testing.T) {
		h := testhome.New(t)
		if err := os.Mkdir(h.LogsDir(), 0o500); err != nil {
			t.Fatal(err)
		}
		it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDLogWritable)
		if it.Severity != contract.SeverityError || it.Fix != "chmod 700 "+h.LogsDir() {
			t.Fatalf("log.writable = %+v", it)
		}
	})
	t.Run("no log yet, home read-only", func(t *testing.T) {
		h := testhome.New(t)
		if err := os.Chmod(h.Dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(h.Dir, 0o700) })
		it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDLogWritable)
		if it.Severity != contract.SeverityError {
			t.Fatalf("log.writable = %+v", it)
		}
	})
	t.Run("no log yet, writable directory", func(t *testing.T) {
		h := testhome.New(t)
		it := byID(t, run(t, Options{Home: h, Mode: ModeLocal}), IDLogWritable)
		if it.Severity != contract.SeverityOK {
			t.Fatalf("log.writable = %+v", it)
		}
	})
}

func TestReport_StatusIsTheWorstSeverity(t *testing.T) {
	mk := func(sev ...string) contract.Doctor {
		var items []contract.DoctorItem
		for _, s := range sev {
			items = append(items, contract.DoctorItem{Severity: s})
		}
		return report("local", items)
	}
	for _, tc := range []struct {
		sev  []string
		want string
	}{
		{nil, contract.SeverityOK},
		{[]string{"ok", "ok"}, contract.SeverityOK},
		{[]string{"ok", "warn"}, contract.SeverityWarn},
		{[]string{"warn", "error", "ok"}, contract.SeverityError},
	} {
		if got := mk(tc.sev...); got.Status != tc.want {
			t.Errorf("%v -> %q, want %q", tc.sev, got.Status, tc.want)
		}
	}
}
