package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

// dxDoctor is the healthy daemon-mode report of `_dx.md`.
func dxDoctor(h string) contract.Doctor {
	return contract.Doctor{
		Status: contract.SeverityOK,
		Source: contract.SourceDaemon,
		Items: []contract.DoctorItem{
			{ID: "home.permissions", Severity: "ok", Message: h + " is 0700"},
			{ID: "socket.permissions", Severity: "ok", Message: h + "/daemon.sock is 0600"},
			{ID: "config.valid", Severity: "ok", Message: "defaults in use (no config file)"},
			{ID: "ledger.integrity", Severity: "ok", Message: "quick_check ok"},
			{ID: "ledger.schema", Severity: "ok", Message: "schema 1 (current)"},
			{ID: "log.writable", Severity: "ok", Message: h + "/logs/harness.log is writable"},
		},
		Summary: contract.DoctorSummary{OK: 6},
	}
}

func withDoctor(e *env, d contract.Doctor) {
	e.api.doctorFn = func() (contract.Doctor, error) { return d, nil }
}

// UT-132: a report with findings is a document on stdout, with the exit code of the finding.
func TestUT132_ReportsAreDocumentsOnStdout(t *testing.T) {
	for _, mode := range []string{"json", "jsonl"} {
		t.Run("doctor "+mode, func(t *testing.T) {
			e := newEnv(t)
			d := dxDoctor(e.home.Dir)
			d.Items[0] = contract.DoctorItem{ID: "home.permissions", Severity: "error",
				Message: e.home.Dir + " is 0755; expected 0700", Fix: "chmod 700 " + e.home.Dir}
			d.Status, d.Summary = contract.SeverityError, contract.DoctorSummary{OK: 5, Error: 1}
			withDoctor(e, d)

			code := e.run("doctor", "-o", mode)

			if code != 70 || e.err() != "" {
				t.Fatalf("exit %d, stderr %q; want 70 and empty stderr", code, e.err())
			}
			oneDocument(t, "stdout", e.out())
			var got contract.DoctorResponse
			if err := json.Unmarshal([]byte(e.out()), &got); err != nil {
				t.Fatal(err)
			}
			if got.Doctor.Status != "error" || got.Doctor.Items[0].Fix != "chmod 700 "+e.home.Dir || got.Doctor.Source != "daemon" {
				t.Errorf("report = %+v", got.Doctor)
			}
		})
		t.Run("config validate "+mode, func(t *testing.T) {
			e := newEnv(t)
			writeConfig(t, e, "[daemon]\nshutdown_grase = \"5s\"\n[log]\nlevel = \"verbose\"\n")

			code := e.run("config", "validate", "-o", mode)

			if code != 78 || e.err() != "" {
				t.Fatalf("exit %d, stderr %q; want 78 and empty stderr", code, e.err())
			}
			oneDocument(t, "stdout", e.out())
			var got validateDocument
			if err := json.Unmarshal([]byte(e.out()), &got); err != nil {
				t.Fatal(err)
			}
			if got.Config.Valid || len(got.Config.Errors) != 2 {
				t.Errorf("report = %+v, want invalid with 2 errors", got.Config)
			}
		})
	}
}

func writeConfig(t *testing.T, e *env, content string) {
	t.Helper()
	if err := os.WriteFile(e.home.ConfigPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The three transcripts of `_dx.md` › harness doctor, line by line.
func TestDoctorHumanMatchesTheTranscripts(t *testing.T) {
	t.Run("healthy daemon", func(t *testing.T) {
		e := newEnv(t)
		withDoctor(e, dxDoctor(e.home.Dir))
		if code := e.run("doctor"); code != 0 || e.err() != "" {
			t.Fatalf("exit %d, stderr %q", code, e.err())
		}
		want := "OK    home.permissions     " + e.home.Dir + " is 0700\n" +
			"OK    socket.permissions   " + e.home.Dir + "/daemon.sock is 0600\n" +
			"OK    config.valid         defaults in use (no config file)\n" +
			"OK    ledger.integrity     quick_check ok\n" +
			"OK    ledger.schema        schema 1 (current)\n" +
			"OK    log.writable         " + e.home.Dir + "/logs/harness.log is writable\n" +
			"summary: 6 ok, 0 warn, 0 error\n"
		if e.out() != want {
			t.Errorf("stdout:\n%s\nwant:\n%s", e.out(), want)
		}
	})
	t.Run("warn exits 0", func(t *testing.T) {
		e := newEnv(t)
		withDoctor(e, contract.Doctor{Status: "warn", Source: "local",
			Items:   []contract.DoctorItem{{ID: "daemon.reachable", Severity: "warn", Message: "no daemon is listening on /x/daemon.sock (checks ran locally)"}},
			Summary: contract.DoctorSummary{Warn: 1}})
		code := e.run("doctor")
		want := "WARN  daemon.reachable     no daemon is listening on /x/daemon.sock (checks ran locally)\nsummary: 0 ok, 1 warn, 0 error\n"
		if code != 0 || e.out() != want {
			t.Errorf("exit %d stdout %q, want 0 and %q", code, e.out(), want)
		}
	})
	t.Run("error with a fix exits 70", func(t *testing.T) {
		e := newEnv(t)
		withDoctor(e, contract.Doctor{Status: "error", Source: "daemon",
			Items: []contract.DoctorItem{{ID: "home.permissions", Severity: "error",
				Message: "/h is 0755; expected 0700", Fix: "chmod 700 /h"}},
			Summary: contract.DoctorSummary{Error: 1}})
		code := e.run("doctor")
		want := "ERROR home.permissions     /h is 0755; expected 0700\n      fix: chmod 700 /h\nsummary: 0 ok, 0 warn, 1 error\n"
		if code != 70 || e.out() != want || e.err() != "" {
			t.Errorf("exit %d stdout %q stderr %q, want 70 and %q", code, e.out(), e.err(), want)
		}
	})
}

// UT-075 through the CLI: no daemon answers, so the checks run here.
func TestDoctorFallsBackToLocalChecksWhenNoDaemonAnswers(t *testing.T) {
	e := newEnv(t) // the fake API answers daemon_unavailable by default

	code := e.run("doctor", "-o", "json")

	if code != 0 || e.err() != "" {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	var got contract.DoctorResponse
	if err := json.Unmarshal([]byte(e.out()), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Doctor
	if d.Source != "local" || d.Status != "warn" || d.Summary != (contract.DoctorSummary{OK: 5, Warn: 1}) {
		t.Fatalf("report = %+v", d)
	}
	if d.Items[0].ID != "daemon.reachable" || d.Items[0].Severity != "warn" {
		t.Errorf("first item = %+v", d.Items[0])
	}
	for _, it := range d.Items {
		if it.ID == "socket.permissions" {
			t.Error("local mode reported socket.permissions")
		}
	}
	if e.api.doctorCalls.Load() != 1 {
		t.Errorf("the daemon was asked %d times, want once", e.api.doctorCalls.Load())
	}
}

// UT-076 through the CLI: an unreadable ledger is reported without a daemon, exit 70.
func TestDoctorLocalReportsAnUnreadableLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	e := newEnv(t)
	if err := os.WriteFile(e.home.DBPath(), []byte("data"), 0o000); err != nil {
		t.Fatal(err)
	}

	code := e.run("doctor", "-o", "json")

	if code != 70 || e.err() != "" {
		t.Fatalf("exit %d, stderr %q", code, e.err())
	}
	var got contract.DoctorResponse
	if err := json.Unmarshal([]byte(e.out()), &got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range got.Doctor.Items {
		if it.ID == "ledger.integrity" {
			found = it.Severity == "error"
		}
	}
	if !found || got.Doctor.Source != "local" {
		t.Errorf("report = %+v, want a local report with ledger.integrity as an error", got.Doctor)
	}
}

func TestDoctorLocalCreatesNothingInAMissingHome(t *testing.T) {
	e := newEnv(t)
	missing := e.home.Dir + "/nested/home"

	code := e.run("doctor", "--home", missing)

	if code != 0 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, e.out(), e.err())
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("doctor created %s (%v)", missing, err)
	}
}

// The local fallback is for "no daemon" only: a daemon that answers with an error is an error.
func TestDoctorOtherFailuresAreNotLocalReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *contract.Error
		exit int
	}{
		{"starting", &contract.Error{Code: contract.CodeDaemonNotReady, Message: "daemon is starting"}, 69},
		{"draining", &contract.Error{Code: contract.CodeDaemonDraining, Message: "daemon is stopping"}, 69},
		{"internal", &contract.Error{Code: contract.CodeInternal, Message: "internal error"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.api.doctorFn = func() (contract.Doctor, error) { return contract.Doctor{}, tc.err }

			code := e.run("doctor", "-o", "json")

			if code != tc.exit || e.out() != "" || e.wireError().Code != tc.err.Code {
				t.Errorf("exit %d stdout %q stderr %q, want %d, empty stdout and %s", code, e.out(), e.err(), tc.exit, tc.err.Code)
			}
		})
	}
}

func TestConfigPath(t *testing.T) {
	e := newEnv(t)
	if code := e.run("config", "path"); code != 0 || e.out() != e.home.Dir+"/config.toml\n" || e.err() != "" {
		t.Errorf("exit %d stdout %q stderr %q", code, e.out(), e.err())
	}
	if code := e.run("config", "path", "-o", "json"); code != 0 || e.out() != `{"config":{"path":"`+e.home.Dir+`/config.toml"}}`+"\n" {
		t.Errorf("json: exit %d stdout %q", code, e.out())
	}
	if e.api.calls() != 0 {
		t.Error("config path talked to the daemon")
	}
}

func TestConfigValidate(t *testing.T) {
	t.Run("missing file is valid", func(t *testing.T) {
		e := newEnv(t)
		if code := e.run("config", "validate"); code != 0 ||
			e.out() != "valid: "+e.home.Dir+"/config.toml (not present, defaults in use)\n" {
			t.Errorf("exit %d stdout %q", code, e.out())
		}
		if code := e.run("config", "validate", "-o", "json"); code != 0 ||
			e.out() != `{"config":{"path":"`+e.home.Dir+`/config.toml","valid":true,"errors":[]}}`+"\n" {
			t.Errorf("json: exit %d stdout %q", code, e.out())
		}
	})
	t.Run("valid file", func(t *testing.T) {
		e := newEnv(t)
		writeConfig(t, e, "[log]\nlevel = \"debug\"\n")
		if code := e.run("config", "validate"); code != 0 || e.out() != "valid: "+e.home.Dir+"/config.toml\n" {
			t.Errorf("exit %d stdout %q", code, e.out())
		}
	})
	t.Run("invalid file lists every problem", func(t *testing.T) {
		e := newEnv(t)
		writeConfig(t, e, "[daemon]\nshutdown_grase = \"5s\"\n[log]\nlevel = \"verbose\"\n")

		code := e.run("config", "validate", "-o", "json")

		want := `{"config":{"path":"` + e.home.Dir + `/config.toml","valid":false,"errors":[` +
			`{"key":"daemon.shutdown_grase","code":"unknown_key","message":"unknown key \"daemon.shutdown_grase\""},` +
			`{"key":"log.level","code":"invalid_value","message":"log.level \"verbose\" must be one of debug, info, warn, error"}]}}` + "\n"
		if code != 78 || e.out() != want || e.err() != "" {
			t.Errorf("exit %d\nstdout %q\nwant   %q\nstderr %q", code, e.out(), want, e.err())
		}

		code = e.run("config", "validate")
		wantHuman := "invalid: " + e.home.Dir + "/config.toml\n" +
			"  - unknown key \"daemon.shutdown_grase\" (unknown_key)\n" +
			"  - log.level \"verbose\" must be one of debug, info, warn, error (invalid_value)\n"
		if code != 78 || e.out() != wantHuman || e.err() != "" {
			t.Errorf("human: exit %d stdout %q stderr %q", code, e.out(), e.err())
		}
	})
	t.Run("an empty shutdown_grace is invalid, not the default", func(t *testing.T) {
		e := newEnv(t)
		writeConfig(t, e, "[daemon]\nshutdown_grace = \"\"\n")

		code := e.run("config", "validate", "-o", "json")

		want := `{"config":{"path":"` + e.home.Dir + `/config.toml","valid":false,"errors":[` +
			`{"key":"daemon.shutdown_grace","code":"invalid_value","message":"daemon.shutdown_grace \"\" is not a duration"}]}}` + "\n"
		if code != 78 || e.out() != want || e.err() != "" {
			t.Errorf("exit %d\nstdout %q\nwant   %q\nstderr %q", code, e.out(), want, e.err())
		}

		code = e.run("config", "validate")
		wantHuman := "invalid: " + e.home.Dir + "/config.toml\n" +
			"  - daemon.shutdown_grace \"\" is not a duration (invalid_value)\n"
		if code != 78 || e.out() != wantHuman || e.err() != "" {
			t.Errorf("human: exit %d stdout %q stderr %q", code, e.out(), e.err())
		}
	})
	t.Run("broken TOML names the line", func(t *testing.T) {
		e := newEnv(t)
		writeConfig(t, e, "[daemon\n")
		code := e.run("config", "validate", "-o", "json")
		if code != 78 || !strings.Contains(e.out(), `"code":"parse_error"`) || !strings.Contains(e.out(), "line 2") {
			t.Errorf("exit %d stdout %q", code, e.out())
		}
	})
	t.Run("an unreadable file is an unexpected failure", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file")
		}
		e := newEnv(t)
		if err := os.WriteFile(e.home.ConfigPath(), []byte("[log]\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		code := e.run("config", "validate", "-o", "json")
		if code != 1 || e.out() != "" || e.wireError().Code != contract.CodeInternal {
			t.Errorf("exit %d stdout %q stderr %q", code, e.out(), e.err())
		}
	})
}

func TestConfigShow(t *testing.T) {
	e := newEnv(t)
	if code := e.run("config", "show", "-o", "json"); code != 0 ||
		e.out() != `{"config":{"daemon":{"shutdown_grace":"30s"},"log":{"level":"info"}}}`+"\n" || e.err() != "" {
		t.Errorf("defaults: exit %d stdout %q stderr %q", code, e.out(), e.err())
	}
	if code := e.run("config", "show"); code != 0 ||
		e.out() != "[daemon]\nshutdown_grace = \"30s\"\n\n[log]\nlevel = \"info\"\n" {
		t.Errorf("human: exit %d stdout %q", code, e.out())
	}

	writeConfig(t, e, "[daemon]\nshutdown_grace = \"90s\"\n")
	if code := e.run("config", "show", "-o", "json"); code != 0 ||
		e.out() != `{"config":{"daemon":{"shutdown_grace":"90s"},"log":{"level":"info"}}}`+"\n" {
		t.Errorf("merged: exit %d stdout %q", code, e.out())
	}

	writeConfig(t, e, "[log]\nlevel = \"verbose\"\n")
	code := e.run("config", "show", "-o", "json")
	if code != 78 || e.out() != "" {
		t.Fatalf("invalid: exit %d stdout %q", code, e.out())
	}
	if w := e.wireError(); w.Code != contract.CodeConfigInvalid || w.Hint != "run `harness config validate`" ||
		!strings.HasPrefix(w.Message, "config invalid: "+e.home.Dir+"/config.toml: ") {
		t.Errorf("error = %+v", w)
	}
	if e.api.calls() != 0 {
		t.Error("config show talked to the daemon")
	}
}

func TestConfigUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"config", "bogus"},
		{"config", "validate", "extra"},
		{"config", "path", "extra"},
		{"version", "extra"},
		{"doctor", "extra"},
	} {
		e := newEnv(t)
		if code := e.run(append(args, "-o", "json")...); code != 64 || e.out() != "" || e.wireError().Code != "usage" {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, code, e.out(), e.err())
		}
	}
}

// E2E-018 in process: the human line and the JSON keys.
func TestVersion(t *testing.T) {
	e := newEnv(t)
	re := regexp.MustCompile(`^harness \S+ \(commit \S+, go\S+, (darwin|linux)/(arm64|amd64)\)\n$`)
	if code := e.run("version"); code != 0 || !re.MatchString(e.out()) || e.err() != "" {
		t.Errorf("exit %d stdout %q stderr %q", code, e.out(), e.err())
	}
	if code := e.run("version", "-o", "json"); code != 0 {
		t.Fatalf("json: exit %d", code)
	}
	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(e.out()), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "commit", "go", "platform"} {
		if got["version"][k] == "" {
			t.Errorf("version.%s is empty in %s", k, e.out())
		}
	}
	if len(got["version"]) != 4 {
		t.Errorf("version has keys %v, want exactly 4", got["version"])
	}
	if e.api.calls() != 0 {
		t.Error("version talked to the daemon")
	}
}

// The real client asks GET /api/doctor and decodes the report; an error answer is the typed
// error, which is what lets the command tell "no daemon" from a daemon that said no.
func TestClientDoctor(t *testing.T) {
	var got seen
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		fmt.Fprint(w, `{"doctor":{"status":"warn","source":"daemon","items":[{"id":"config.valid","severity":"warn","message":"m","fix":"f"}],"summary":{"ok":0,"warn":1,"error":0}}}`)
	}))

	d, err := newClient(sock).Doctor(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if got.method != "GET" || got.path != "/api/doctor" || got.accept != "application/json" {
		t.Errorf("requested %s %s accept %q", got.method, got.path, got.accept)
	}
	if d.Status != "warn" || d.Source != "daemon" || len(d.Items) != 1 || d.Items[0].Fix != "f" || d.Summary.Warn != 1 {
		t.Errorf("report %+v", d)
	}
}

func TestClientDoctorErrors(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":"daemon_not_ready","message":"daemon is starting"}}`)
	}))
	_, err := newClient(sock).Doctor(context.Background())
	var wire *contract.Error
	if !errors.As(err, &wire) || wire.Code != contract.CodeDaemonNotReady {
		t.Fatalf("error = %v, want daemon_not_ready", err)
	}
	if isUnavailable(err) {
		t.Error("a daemon that answered 503 was taken for no daemon")
	}

	_, err = newClient(testhome.New(t).SocketPath()).Doctor(context.Background())
	if !isUnavailable(err) {
		t.Errorf("no socket: error = %v, want daemon_unavailable", err)
	}
}
