//go:build integration

package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/testutil/testhome"
)

func (r *daemonRun) doctor(t *testing.T) contract.Doctor {
	t.Helper()
	code, body := r.do(t, http.MethodGet, "/api/doctor", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/doctor = %d %s, want 200", code, body)
	}
	var resp contract.DoctorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp.Doctor
}

func doctorItem(t *testing.T, d contract.Doctor, id string) contract.DoctorItem {
	t.Helper()
	for _, it := range d.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", id, d.Items)
	return contract.DoctorItem{}
}

// IT-040: a daemon on a fresh home answers its own doctor with all six probes ok.
func TestIT040_DoctorThroughTheDaemon(t *testing.T) {
	checkLeaks(t)
	h := testhome.NewMissing(t)
	r := startDaemon(t, h, nil)

	d := r.doctor(t)

	if d.Status != contract.SeverityOK || d.Source != contract.SourceDaemon {
		t.Fatalf("status %q source %q, want ok daemon\n%+v", d.Status, d.Source, d.Items)
	}
	if d.Summary != (contract.DoctorSummary{OK: 6}) {
		t.Fatalf("summary %+v, want 6 ok", d.Summary)
	}
	var got []string
	for _, it := range d.Items {
		got = append(got, it.ID)
		if it.Severity != contract.SeverityOK {
			t.Errorf("%s = %+v, want ok", it.ID, it)
		}
	}
	want := []string{"home.permissions", "socket.permissions", "config.valid", "ledger.integrity", "ledger.schema", "log.writable"}
	if !slices.Equal(got, want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	if it := doctorItem(t, d, "socket.permissions"); it.Message != h.SocketPath()+" is 0600" {
		t.Errorf("socket.permissions = %q", it.Message)
	}
}

// The daemon's runner knows the digest this run loaded: a config edited afterwards is a
// warning when valid and an error when not, and nothing is applied (no hot reload).
func TestIT040_DoctorSeesAConfigEditedAfterBoot(t *testing.T) {
	checkLeaks(t)
	h := testhome.NewMissing(t)
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.ConfigPath(), []byte("[log]\nlevel = \"info\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startDaemon(t, h, nil)

	if it := doctorItem(t, r.doctor(t), "config.valid"); it.Severity != contract.SeverityOK {
		t.Fatalf("config.valid before the edit = %+v", it)
	}

	if err := os.WriteFile(h.ConfigPath(), []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if it := doctorItem(t, r.doctor(t), "config.valid"); it.Severity != contract.SeverityWarn || it.Fix == "" {
		t.Errorf("config.valid after a valid edit = %+v, want warn", it)
	}

	if err := os.WriteFile(h.ConfigPath(), []byte("[log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := r.doctor(t)
	if it := doctorItem(t, d, "config.valid"); it.Severity != contract.SeverityError {
		t.Errorf("config.valid after a breaking edit = %+v, want error", it)
	}
	if d.Status != contract.SeverityError {
		t.Errorf("status %q, want error", d.Status)
	}
}

// A home made readable by others after boot is reported and left alone (Business Rule 28).
func TestIT040_DoctorReportsALooseHomeAndLeavesItAlone(t *testing.T) {
	checkLeaks(t)
	h := testhome.NewMissing(t)
	r := startDaemon(t, h, nil)
	if err := os.Chmod(h.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.Dir, 0o700) })

	d := r.doctor(t)

	if it := doctorItem(t, d, "home.permissions"); it.Severity != contract.SeverityError || it.Fix != "chmod 700 "+h.Dir {
		t.Fatalf("home.permissions = %+v", it)
	}
	if fi, err := os.Stat(h.Dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the home is %v (%v), doctor must not change it", fi.Mode().Perm(), err)
	}
}
