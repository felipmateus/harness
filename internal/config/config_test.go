package config_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/felipmateus/harness/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func invalid(t *testing.T, err error) *config.InvalidError {
	t.Helper()
	var inv *config.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("want *config.InvalidError, got %v", err)
	}
	return inv
}

// UT-001
func TestLoad_UnknownKeyIsReported(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[daemon]\nshutdown_grase = \"10s\"\n"))
	inv := invalid(t, err)
	found := false
	for _, e := range inv.Errors {
		if e.Key == "daemon.shutdown_grase" && e.Code == config.CodeUnknownKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors %+v do not contain unknown_key for daemon.shutdown_grase", inv.Errors)
	}
}

// UT-002
func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Exists || got.Digest != nil {
		t.Fatalf("exists=%v digest=%v, want false and nil", got.Exists, got.Digest)
	}
	if got.Config.Daemon.ShutdownGrace.Duration != 30*time.Second || got.Config.Log.Level != "info" {
		t.Fatalf("defaults = %+v", got.Config)
	}
}

// UT-003
func TestLoad_ValidFileValuesAndDigest(t *testing.T) {
	body := "[daemon]\nshutdown_grace = \"45s\"\n\n[log]\nlevel = \"debug\"\n"
	got, err := config.Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got.Digest == nil || *got.Digest != want {
		t.Fatalf("digest = %v, want %s", got.Digest, want)
	}
	if !got.Exists || got.Config.Daemon.ShutdownGrace.Duration != 45*time.Second || got.Config.Log.Level != "debug" {
		t.Fatalf("loaded = %+v", got)
	}
	out, err := json.Marshal(got.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"daemon":{"shutdown_grace":"45s"},"log":{"level":"debug"}}` {
		t.Fatalf("effective config json = %s", out)
	}
}

// UT-004
func TestLoad_CollectsEveryProblem(t *testing.T) {
	body := "[daemon]\nshutdown_grace = \"10m\"\nbogus = 1\n\n[log]\nlevel = \"verbose\"\n"
	_, err := config.Load(writeConfig(t, body))
	inv := invalid(t, err)
	var codes []string
	for _, e := range inv.Errors {
		codes = append(codes, e.Code)
	}
	sort.Strings(codes)
	want := []string{config.CodeInvalidValue, config.CodeOutOfRange, config.CodeUnknownKey}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("codes = %v, want %v (errors %+v)", codes, want, inv.Errors)
	}
}

// UT-005
func TestLoad_InvalidTOMLReportsLine(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[daemon"))
	inv := invalid(t, err)
	if len(inv.Errors) != 1 || inv.Errors[0].Code != config.CodeParseError || !strings.Contains(inv.Errors[0].Message, "line 1") {
		t.Fatalf("errors = %+v, want one parse_error mentioning line 1", inv.Errors)
	}
}

// UT-006
func TestLoad_ShutdownGraceBoundaries(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
	}{
		{"1s", true},
		{"5m", true},
		{"999ms", false},
		{"5m1s", false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, "[daemon]\nshutdown_grace = \""+tc.value+"\"\n"))
			if tc.ok {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			inv := invalid(t, err)
			if len(inv.Errors) != 1 || inv.Errors[0].Code != config.CodeOutOfRange {
				t.Fatalf("errors = %+v, want one out_of_range", inv.Errors)
			}
		})
	}
}

// UT-007 (US-011, Business Rule 24): a typo never silently falls back to a default. A shutdown_grace
// that is written but is not a duration is invalid, an empty or blank string included.
func TestLoad_ShutdownGraceNotADurationIsInvalid(t *testing.T) {
	cases := []struct {
		name string
		toml string // the value as written in the file
		text string // the text the message quotes
	}{
		{"empty", `""`, ""},
		{"blank", `"  "`, "  "},
		{"no unit", `"30"`, "30"},
		{"word", `"soon"`, "soon"},
		{"integer", `30`, "30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, "[daemon]\nshutdown_grace = "+tc.toml+"\n"))
			inv := invalid(t, err)
			want := config.FieldError{
				Key:     "daemon.shutdown_grace",
				Code:    config.CodeInvalidValue,
				Message: fmt.Sprintf("daemon.shutdown_grace %q is not a duration", tc.text),
			}
			if len(inv.Errors) != 1 || inv.Errors[0] != want {
				t.Fatalf("errors = %+v, want exactly %+v", inv.Errors, want)
			}
		})
	}
}
