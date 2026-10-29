package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/felipmateus/harness"

// allowed lists, per package, the internal packages it may import directly.
// A package listed with nil may import no internal package. Packages that do not
// exist yet pass vacuously; the rules apply as soon as they appear.
var allowed = map[string][]string{
	"cmd/harness":           {"internal/cli"},
	"internal/cli":          {"internal/api/contract", "internal/home", "internal/config", "internal/doctor", "internal/version", "internal/redact", "internal/daemon"},
	"internal/api":          {"internal/api/contract", "internal/stream", "internal/ledger", "internal/admission", "internal/redact"},
	"internal/stream":       {"internal/ledger", "internal/broker"},
	"internal/broker":       {"internal/ledger"},
	"internal/doctor":       {"internal/api/contract", "internal/config", "internal/home", "internal/ledger"},
	"internal/logging":      {"internal/redact"},
	"internal/ledger":       nil,
	"internal/home":         nil,
	"internal/config":       nil,
	"internal/admission":    nil,
	"internal/redact":       nil,
	"internal/version":      nil,
	"internal/api/contract": nil,
}

type pkg struct {
	ImportPath string
	Imports    []string
	Deps       []string
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", module+"/...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func rel(path string) string { return strings.TrimPrefix(path, module+"/") }

func internalOnly(paths []string) []string {
	var out []string
	for _, p := range paths {
		if r := rel(p); r != p && (strings.HasPrefix(r, "internal/") || strings.HasPrefix(r, "cmd/")) {
			out = append(out, r)
		}
	}
	return out
}

// UT-090
func TestBoundaries(t *testing.T) {
	pkgs := listPackages(t)
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages")
	}
	for _, p := range pkgs {
		name := rel(p.ImportPath)
		imports := internalOnly(p.Imports)

		// Production code never imports test helpers.
		for _, imp := range imports {
			if strings.HasPrefix(imp, "internal/testutil") {
				t.Errorf("%s imports test helper %s", name, imp)
			}
		}

		switch name {
		case "internal/daemon":
			for _, imp := range imports {
				if imp == "internal/cli" || strings.HasPrefix(imp, "cmd/") {
					t.Errorf("internal/daemon must not import %s", imp)
				}
			}
			continue
		}

		rule, listed := allowed[name]
		if !listed {
			continue
		}
		permitted := map[string]bool{}
		for _, a := range rule {
			permitted[a] = true
		}
		for _, imp := range imports {
			if !permitted[imp] {
				t.Errorf("%s may not import %s (allowed: %v)", name, imp, rule)
			}
		}
	}

	// Transitive rule: only cmd/harness and internal/cli may depend on internal/daemon.
	for _, p := range pkgs {
		name := rel(p.ImportPath)
		if name == "cmd/harness" || name == "internal/cli" || name == "internal/daemon" {
			continue
		}
		for _, dep := range internalOnly(p.Deps) {
			if dep == "internal/daemon" {
				t.Errorf("%s depends on internal/daemon", name)
			}
		}
	}
}

// The rules table must stay in sync with the spec: every row has an owner here.
func TestBoundaryTableCoversSpecRows(t *testing.T) {
	want := []string{
		"cmd/harness", "internal/admission", "internal/api", "internal/api/contract", "internal/broker",
		"internal/cli", "internal/config", "internal/doctor", "internal/home", "internal/ledger",
		"internal/logging", "internal/redact", "internal/stream", "internal/version",
	}
	var got []string
	for k := range allowed {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rules for %v, want %v", got, want)
	}
}
