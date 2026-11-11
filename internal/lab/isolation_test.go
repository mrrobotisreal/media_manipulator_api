package lab_test

// Licence isolation (MM138 decision 4, ADR-0021). These tests fail the build
// when a product path could reach a LabOnly profile, tool or result.
// (b), (c), (d) and (f) live beside the handlers they exercise
// (internal/handlers/lab_isolation_test.go); (e) is split between
// internal/burst/lab_test.go (the planner) and here (the registry side).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
)

// goFiles lists non-test Go files under the repo's internal/ and cmd/ trees.
func goFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				out = append(out, filepath.ToSlash(p))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("source scan found %d files; the walk is broken", len(out))
	}
	return out
}

// labAware are the only non-test files allowed to name the "lab_" tool prefix.
var labAware = map[string]bool{
	"../lab/registry.go":          true,
	"../lab/runner.go":            true,
	"../lab/gate.go":              true,
	"../handlers/lab.go":          true,
	"../handlers/lab_guard.go":    true,
	"../handlers/share.go":        true,
	"../share/share.go":           true,
	"../../cmd/api/lab_wiring.go": true,
}

// (a) Every product burst profile, every GPURequest Kind literal and every Medi
// tool/option manifest stays clear of the Lab.
func TestIsolation_A_ProductNeverMapsToLabOnly(t *testing.T) {
	// 1. The product table holds no LabOnly row, and no lab Kind resolves through it.
	for _, p := range burst.Profiles() {
		if p.LabOnly || lab.IsLabTool(p.Key) {
			t.Errorf("product profile %q is LabOnly or lab-named", p.Key)
		}
	}
	for _, m := range lab.Models() {
		if _, ok := burst.Lookup(m.Tool()); ok {
			t.Errorf("burst.Lookup(%q) resolved: a product path could reach the lab profile", m.Tool())
		}
		if burst.Burstable(m.Tool()) {
			t.Errorf("%q must not be Burstable for the trigger path", m.Tool())
		}
		if p, ok := burst.LookupAny(m.Tool()); !ok || !p.LabOnly {
			t.Errorf("LookupAny(%q) must resolve to a LabOnly profile: %+v", m.Tool(), p)
		}
	}

	// 2. Every GPURequest Kind literal and every "lab_" string outside the Lab's own files.
	kindRe := regexp.MustCompile(`Kind:\s+(?:"([^"]+)"|[A-Za-z_.]+\s*\+\s*"([^"]*)")`)
	labLit := regexp.MustCompile(`"lab_[a-z0-9_]*"`)
	seen := 0
	for _, f := range goFiles(t) {
		if strings.HasPrefix(f, "../lab/") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !labAware[f] && labLit.MatchString(src) {
			t.Errorf("%s names a lab_ tool literal but is not a Lab file", f)
		}
		for _, mm := range kindRe.FindAllStringSubmatch(src, -1) {
			kind := mm[1] + mm[2]
			seen++
			if lab.IsLabTool(kind) {
				t.Errorf("%s: GPURequest-style Kind %q names a lab tool", f, kind)
			}
			if p, ok := burst.Lookup(kind); ok && p.LabOnly {
				t.Errorf("%s: Kind %q maps to a LabOnly profile", f, kind)
			}
		}
	}
	if seen < 5 {
		t.Fatalf("the Kind scan matched only %d literals; the pattern drifted", seen)
	}

	// 3. Medi: its tool definitions, tool pages and option manifests never name the Lab.
	for _, surface := range []string{"guide", "copilot"} {
		for _, td := range assistant.ToolsFor(surface, assistant.NewOpsValidator()) {
			if lab.IsLabTool(td.Name) || strings.Contains(strings.ToLower(td.Description), "/lab") {
				t.Errorf("Medi tool %q mentions the Lab", td.Name)
			}
		}
	}
	dirs := []string{"../assistant/knowledge/tools", "../assistant/knowledge/pages", "../assistant/knowledge/editors", "../assistant/knowledge/recipes"}
	files := 0
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			files++
			b, _ := os.ReadFile(p)
			low := strings.ToLower(string(b))
			if strings.HasPrefix(strings.ToLower(e.Name()), "lab") || strings.Contains(low, "lab_") || strings.Contains(low, "/lab\"") || strings.Contains(low, "(/lab)") {
				t.Errorf("Medi knowledge file %s mentions the Lab", p)
			}
			return nil
		})
	}
	routes, err := os.ReadFile("../assistant/knowledge/routes.json")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`"/(?:[a-z-]+/)?lab[/"]`).Match(routes) {
		t.Error("Medi routes.json lists a lab route")
	}
	if files < 50 {
		t.Fatalf("Medi knowledge scan read %d files; the walk is broken", files)
	}
}

// (e), registry side: a lab profile is never in the product table, so the
// product trigger (Planner.Launch, which resolves through burst.Lookup) cannot
// even name it; the planner-side refusal (LabOnly profile without the lab flag,
// and the lab flag on a product profile) is TestLaunchDirect_Scope in
// internal/burst.
func TestIsolation_E_RegistryProfilesAreLabOnlyAndOutsideTheProductTable(t *testing.T) {
	for _, m := range lab.Models() {
		p := m.Profile()
		if !p.LabOnly {
			t.Errorf("%s: generated profile is not LabOnly", m.Key)
		}
		for _, pp := range burst.Profiles() {
			if pp.Key == p.Key {
				t.Errorf("%s: also present in the product profile table", m.Key)
			}
		}
	}
}
