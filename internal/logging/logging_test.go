package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/logging"
)

// UT-081
func TestLogger_RedactsAttributesInTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.log")
	l, err := logging.New(logging.Options{Path: path, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	l.Error("call failed", "err", "call failed: token=abc", "cause", errors.New("Authorization: Bearer xyz"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, "token=[REDACTED]") || strings.Contains(out, "token=abc") {
		t.Fatalf("string attribute not redacted: %s", out)
	}
	if strings.Contains(out, "xyz") {
		t.Fatalf("error attribute not redacted: %s", out)
	}
	if !strings.Contains(out, `"pid":`) {
		t.Fatalf("record lacks pid: %s", out)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestLogger_MirrorCanBeSwitchedOff(t *testing.T) {
	var mirror bytes.Buffer
	l, err := logging.New(logging.Options{Path: filepath.Join(t.TempDir(), "harness.log"), Level: slog.LevelInfo, Mirror: &mirror})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Info("first")
	l.SetMirror(false)
	l.Info("second")
	if !strings.Contains(mirror.String(), "first") || strings.Contains(mirror.String(), "second") {
		t.Fatalf("mirror = %q", mirror.String())
	}
}

// failingWriter accepts failAfter writes, then fails every one with err, counting them all.
type failingWriter struct {
	failAfter int
	err       error
	calls     int
	got       bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.failAfter {
		return 0, w.err
	}
	return w.got.Write(p)
}

// UT-083: a mirror that fails, such as a stderr pipe whose reader has gone (EPIPE), is
// switched off at its first error. The file log keeps every record, the log calls neither
// fail nor block, and the dead mirror costs one failed write, not one per record.
func TestLogger_BrokenMirrorStopsMirroringAndKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.log")
	mirror := &failingWriter{failAfter: 1, err: fmt.Errorf("write |2: %w", syscall.EPIPE)}
	l, err := logging.New(logging.Options{Path: path, Level: slog.LevelInfo, Mirror: mirror})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("first")  // mirrored
	l.Info("second") // the mirror fails here
	l.Info("third")
	l.Info("fourth")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	if mirror.calls != 2 {
		t.Errorf("the mirror was written %d times, want 2: the one that worked and the one that failed", mirror.calls)
	}
	if !strings.Contains(mirror.got.String(), "first") {
		t.Errorf("mirror = %q, want the record written before the failure", mirror.got.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"first", "second", "third", "fourth"} {
		if !strings.Contains(string(data), `"msg":"`+msg+`"`) {
			t.Errorf("the file log lacks %q after the mirror broke: %s", msg, data)
		}
	}
}

// UT-082 (Conventions: times are UTC with exactly three fractional digits): the `time` of a
// record is fixed-width UTC, so it lines up by string comparison with the ledger's event
// times (`2006-01-02T15:04:05.000Z`) and sorts chronologically. slog's default is local time
// with a zone offset and as many fractional digits as the clock gave, without trailing zeros.
func TestLogger_TimeIsFixedWidthUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.log")
	var mirror bytes.Buffer
	l, err := logging.New(logging.Options{Path: path, Level: slog.LevelInfo, Mirror: &mirror})
	if err != nil {
		t.Fatal(err)
	}
	minus3 := time.FixedZone("UTC-3", -3*60*60)
	times := []struct {
		name string
		at   time.Time
		want string
	}{
		{"a local zone is converted", time.Date(2026, 10, 29, 0, 30, 1, 556_760_000, minus3), "2026-10-29T03:30:01.556Z"},
		{"trailing zeros are kept", time.Date(2026, 10, 29, 0, 30, 1, 500_000_000, minus3), "2026-10-29T03:30:01.500Z"},
		{"a whole second has three zeros", time.Date(2026, 10, 29, 0, 30, 2, 0, minus3), "2026-10-29T03:30:02.000Z"},
		{"sub-millisecond digits are cut, not rounded", time.Date(2026, 10, 29, 0, 30, 3, 999_999_999, minus3), "2026-10-29T03:30:03.999Z"},
		{"a UTC time stays", time.Date(2026, 12, 31, 23, 59, 59, 120_000_000, time.UTC), "2026-12-31T23:59:59.120Z"},
	}
	for _, tt := range times {
		rec := slog.NewRecord(tt.at, slog.LevelInfo, tt.name, 0)
		if err := l.Handler().Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	l.Info("now") // the real clock, in whatever zone the machine has
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	fixedWidth := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	for _, out := range []struct{ where, text string }{{"file", readFile(t, path)}, {"mirror", mirror.String()}} {
		lines := strings.Split(strings.TrimSpace(out.text), "\n")
		if len(lines) != len(times)+1 {
			t.Fatalf("%s has %d records, want %d:\n%s", out.where, len(lines), len(times)+1, out.text)
		}
		for i, line := range lines {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s line %d is not JSON: %v", out.where, i, err)
			}
			got, _ := rec["time"].(string)
			if !fixedWidth.MatchString(got) {
				t.Errorf("%s record %q: time = %q, want fixed-width UTC with three fractional digits", out.where, rec["msg"], got)
			}
			if i < len(times) && got != times[i].want {
				t.Errorf("%s record %q: time = %q, want %q", out.where, rec["msg"], got, times[i].want)
			}
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
