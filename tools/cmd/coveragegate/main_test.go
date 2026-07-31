package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestReadCoverageProfileNormalizesFiltersAndCounts(t *testing.T) {
	profile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/root.go:1.1,2.1 2 1
github.com/alkaid/pitaya/v2/session/live.go:3.1,4.1 3 0
github.com/alkaid/pitaya/v2/session/generated.go:1.1,2.1 50 1
`)

	got, err := readCoverageProfiles([]string{profile}, []string{"session/generated.go"})
	if err != nil {
		t.Fatal(err)
	}
	want := baseline{
		Overall: counts{Covered: 2, Total: 5},
		Packages: map[string]counts{
			".":       {Covered: 2, Total: 2},
			"session": {Covered: 0, Total: 3},
		},
	}
	if !reflect.DeepEqual(got.Summary, want) {
		t.Fatalf("summary = %#v, want %#v", got.Summary, want)
	}
	if _, ok := got.Files["session/generated.go"]; ok {
		t.Fatal("excluded file remains in coverage data")
	}
	if _, ok := got.Files["root.go"]; !ok {
		t.Fatal("module-prefixed root file was not normalized")
	}
}

func TestReadCoverageProfilesUsesCompleteRunFloors(t *testing.T) {
	first := writeProfile(t, `mode: count
github.com/alkaid/pitaya/v2/cluster/a.go:1.1,2.1 1 1
github.com/alkaid/pitaya/v2/cluster/a.go:3.1,4.1 1 0
`)
	second := writeProfile(t, `mode: count
github.com/alkaid/pitaya/v2/cluster/a.go:1.1,2.1 1 0
github.com/alkaid/pitaya/v2/cluster/a.go:3.1,4.1 1 1
`)

	got, err := readCoverageProfiles([]string{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value := got.Summary.Packages["cluster"]; value != (counts{Covered: 1, Total: 2}) {
		t.Fatalf("cluster floor = %#v, want one covered statement from each complete run", value)
	}
	if got.Summary.Overall != (counts{Covered: 1, Total: 2}) {
		t.Fatalf("overall floor = %#v", got.Summary.Overall)
	}
}

func TestReadCoverageProfilesRejectsInconsistentRuns(t *testing.T) {
	first := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/a.go:1.1,2.1 1 1
`)
	differentPackage := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/b/b.go:1.1,2.1 1 1
`)
	differentTotal := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/a.go:1.1,2.1 2 1
`)
	differentFile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/b.go:1.1,2.1 1 1
`)
	differentLayout := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/a.go:2.1,3.1 1 1
`)
	differentMode := writeProfile(t, `mode: count
github.com/alkaid/pitaya/v2/a/a.go:1.1,2.1 1 1
`)

	for _, test := range []struct {
		name    string
		second  string
		message string
	}{
		{name: "package set", second: differentPackage, message: "package sets differ"},
		{name: "statement total", second: differentTotal, message: "total differs"},
		{name: "file set", second: differentFile, message: "file sets differ"},
		{name: "block layout", second: differentLayout, message: "block 0 layout differs"},
		{name: "coverage mode", second: differentMode, message: "coverage modes differ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := readCoverageProfiles([]string{first, test.second}, nil)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestReadCoverageProfilesRejectsImpossibleIndependentFloors(t *testing.T) {
	first := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/a.go:1.1,2.1 1 1
github.com/alkaid/pitaya/v2/b/b.go:1.1,2.1 1 0
`)
	second := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/a/a.go:1.1,2.1 1 0
github.com/alkaid/pitaya/v2/b/b.go:1.1,2.1 1 1
`)

	_, err := readCoverageProfiles([]string{first, second}, nil)
	if err == nil || !strings.Contains(err.Error(), "use one complete low run") {
		t.Fatalf("error = %v, want independent-floor rejection", err)
	}
}

func TestCompareBaselineUsesExactRatios(t *testing.T) {
	expected := baseline{
		Overall:  counts{Covered: 1, Total: 3},
		Packages: map[string]counts{".": {Covered: 1, Total: 3}},
	}
	current := baseline{
		Overall:  counts{Covered: 333333, Total: 1000000},
		Packages: map[string]counts{".": {Covered: 333333, Total: 1000000}},
	}
	if err := compareBaseline(expected, current); err == nil {
		t.Fatal("ratio regression that rounds to the same display percentage passed")
	}

	equal := baseline{
		Overall:  counts{Covered: 2, Total: 6},
		Packages: map[string]counts{".": {Covered: 2, Total: 6}},
	}
	if err := compareBaseline(expected, equal); err != nil {
		t.Fatalf("equal exact ratio failed: %v", err)
	}
}

func TestMeetsPercentUsesExactDecimalTarget(t *testing.T) {
	third := counts{Covered: 1, Total: 3}
	if !meetsPercent(third, 33.33333333333333) {
		t.Fatal("one third did not meet a lower decimal target")
	}
	if meetsPercent(third, 33.33333333333334) {
		t.Fatal("one third met a higher decimal target")
	}
	large := counts{Covered: 100_000_000_000_000_000, Total: 100_000_000_000_000_000}
	if !meetsPercent(large, 100) {
		t.Fatal("large exact count overflowed percent comparison")
	}
}

func TestCompareBaselineRejectsPackageSetChanges(t *testing.T) {
	base := baseline{Overall: counts{Covered: 1, Total: 1}, Packages: map[string]counts{".": {Covered: 1, Total: 1}}}
	tests := []struct {
		name    string
		current baseline
		want    string
	}{
		{
			name: "new package",
			current: baseline{Overall: counts{Covered: 2, Total: 2}, Packages: map[string]counts{
				".":   {Covered: 1, Total: 1},
				"new": {Covered: 1, Total: 1},
			}},
			want: "current-only=[new]",
		},
		{
			name:    "removed package",
			current: baseline{Overall: counts{}, Packages: map[string]counts{}},
			want:    "baseline-only=[.]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := compareBaseline(base, test.current)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseChangedLinesHandlesRenamesNewFilesAndFilters(t *testing.T) {
	diff := `diff --git a/old.go b/renamed.go
similarity index 80%
rename from old.go
rename to renamed.go
--- a/old.go
+++ b/renamed.go
@@ -2 +2,2 @@
-old
+new
+more
diff --git a/dev/null b/pkg/new.go
new file mode 100644
--- /dev/null
+++ b/pkg/new.go
@@ -0,0 +1,3 @@
+package pkg
+
+func f() {}
diff --git a/pkg/ignored_test.go b/pkg/ignored_test.go
--- a/pkg/ignored_test.go
+++ b/pkg/ignored_test.go
@@ -1 +1 @@
-old
+new
diff --git a/generated.go b/generated.go
--- a/generated.go
+++ b/generated.go
@@ -4,0 +5 @@
+generated
diff --git a/delete.go b/delete.go
--- a/delete.go
+++ b/delete.go
@@ -8,2 +7,0 @@
-gone
-gone
diff --git a/literal.go b/literal.go
--- a/literal.go
+++ b/literal.go
@@ -1 +1,2 @@
-old
+++ this added source line starts with two plus signs
+more
`
	got, err := parseChangedLines(strings.NewReader(diff), map[string]struct{}{"generated.go": {}})
	if err != nil {
		t.Fatal(err)
	}
	want := changedLines{
		"renamed.go": {2: {}, 3: {}},
		"pkg/new.go": {1: {}, 2: {}, 3: {}},
		"literal.go": {1: {}, 2: {}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed lines = %#v, want %#v", got, want)
	}
}

func TestMeasureDiffCoverage(t *testing.T) {
	changed := changedLines{"pkg/live.go": {5: {}, 6: {}, 10: {}, 20: {}}}
	profiles := map[string]fileCoverage{
		"pkg/live.go": {Blocks: []profileBlock{
			{StartLine: 5, EndLine: 6, NumStmt: 1, Count: 1},
			{StartLine: 10, EndLine: 10, NumStmt: 1, Count: 0},
		}},
	}
	got, err := measureDiffCoverage(t.TempDir(), changed, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if got != (counts{Covered: 2, Total: 3}) {
		t.Fatalf("diff coverage = %#v, want 2/3", got)
	}
}

func TestMeasureDiffCoverageRejectsExecutableFileWithoutProfile(t *testing.T) {
	for _, test := range []struct {
		name     string
		contents string
	}{
		{name: "function body", contents: "package pkg\nfunc missing() { println(1) }\n"},
		{name: "platform file", contents: "//go:build windows\n\npackage pkg\nfunc missing() { println(1) }\n"},
		{name: "package initializer", contents: "package pkg\nvar missing = func() int { return 1 }()\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "pkg", "missing.go"), []byte(test.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := measureDiffCoverage(repo, changedLines{"pkg/missing.go": {1: {}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "no coverage profile entry") {
				t.Fatalf("error = %v, want missing-profile rejection", err)
			}
		})
	}
}

func TestMeasureDiffCoverageRejectsSymlinkWithoutProfile(t *testing.T) {
	repo := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(target, []byte("package outside\nfunc Outside() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, "linked.go")); err != nil {
		t.Fatal(err)
	}
	_, err := measureDiffCoverage(repo, changedLines{"linked.go": {1: {}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestMeasureDiffCoverageAllowsDeclarationOnlyFileWithoutProfile(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "constants.go"), []byte("package pkg\nconst answer = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := measureDiffCoverage(repo, changedLines{"constants.go": {2: {}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != (counts{}) {
		t.Fatalf("diff coverage = %#v, want empty", got)
	}
}

func TestGitChangedLinesUsesExplicitEndpoints(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	file := filepath.Join(repo, "live.go")
	if err := os.WriteFile(file, []byte("package live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("package live\n\nfunc Live() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "live.go")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := gitChangedLines(repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["live.go"], map[int]struct{}{2: {}, 3: {}}) {
		t.Fatalf("changed lines = %#v", got)
	}
}

func TestGitChangedLinesForcesTextForGoFiles(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.go -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "live.go")
	if err := os.WriteFile(file, []byte("package live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("package live\n\nfunc Live() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "live.go")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := gitChangedLines(repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["live.go"]) == 0 {
		t.Fatalf("Go diff was suppressed by .gitattributes: %#v", got)
	}
}

func TestGitChangedLinesTreatsBoundaryRenameAsAddition(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "live_test.go"), []byte("package live\n\nfunc Live() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "mv", "live_test.go", "live.go")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := gitChangedLines(repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["live.go"]) != 3 {
		t.Fatalf("boundary rename was not treated as a full addition: %#v", got)
	}
}

func TestEnsureCleanRootCheckoutIncludesCoverageVerifier(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	verifierDir := filepath.Join(repo, "tools", "cmd", "coveragegate")
	if err := os.MkdirAll(verifierDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verifier := filepath.Join(verifierDir, "main.go")
	if err := os.WriteFile(verifier, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(verifier, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moduleRoots, err := nestedModuleRoots(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCleanRootCheckout(repo, moduleRoots); err == nil || !strings.Contains(err.Error(), "tools/cmd/coveragegate/main.go") {
		t.Fatalf("verifier dirtiness error = %v", err)
	}
}

func TestGitChangedLinesIgnoresTrackedNestedModules(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	files := map[string]string{
		"go.mod":  "module github.com/alkaid/pitaya/v2\n",
		"root.go": "package root\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "root.go"), []byte("package root\n\nfunc Root() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range separatelyVerifiedModuleRoots {
		if err := os.WriteFile(filepath.Join(repo, root, "module.go"), []byte("package nested\n\nfunc Module() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := gitChangedLines(repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range separatelyVerifiedModuleRoots {
		if _, found := got[path.Join(root, "module.go")]; found {
			t.Fatalf("nested module change entered root diff coverage: %#v", got)
		}
	}
	if _, found := got["root.go"]; !found {
		t.Fatalf("root module change is missing: %#v", got)
	}
}

func TestGitChangedLinesRejectsUnregisteredNestedModule(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "root.go"), []byte("package root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(repo, "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "foo", "go.mod"), []byte("module example.invalid/foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "foo", "foo.go"), []byte("package foo\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	_, err := gitChangedLines(repo, base, head, nil)
	if err == nil || !strings.Contains(err.Error(), "unregistered=[foo]") {
		t.Fatalf("error = %v, want unregistered nested module rejection", err)
	}
}

func TestValidateExcludedFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "generated.go"), []byte("package generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateExcludedFiles(repo, []string{"generated.go"}); err != nil {
		t.Fatalf("regular exclusion rejected: %v", err)
	}
	if err := validateExcludedFiles(repo, []string{"missing.go"}); err == nil || !strings.Contains(err.Error(), "missing.go") {
		t.Fatalf("missing exclusion error = %v", err)
	}
	if err := os.Symlink("generated.go", filepath.Join(repo, "linked.go")); err == nil {
		if err := validateExcludedFiles(repo, []string{"linked.go"}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("symlink exclusion error = %v", err)
		}
	}
}

func TestValidatePolicy(t *testing.T) {
	valid := validPolicy()
	if err := validatePolicy(valid); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*policy)
		want   string
	}{
		{name: "schema", mutate: func(value *policy) { value.SchemaVersion = 2 }, want: "schema_version"},
		{name: "sha", mutate: func(value *policy) { value.BaseSHA = "abc" }, want: "full 40- or 64-character"},
		{name: "platform", mutate: func(value *policy) { value.CoveragePlatform.GOOS = "" }, want: "coverage_platform"},
		{name: "unsorted exclusions", mutate: func(value *policy) { value.ExcludedFiles = []string{"z.go", "a.go"} }, want: "sorted and unique"},
		{name: "duplicate exclusions", mutate: func(value *policy) { value.ExcludedFiles = []string{"a.go", "a.go"} }, want: "sorted and unique"},
		{name: "nonlocal exclusion", mutate: func(value *policy) { value.ExcludedFiles = []string{"../a.go"} }, want: "canonical repo-relative"},
		{name: "non-Go exclusion", mutate: func(value *policy) { value.ExcludedFiles = []string{"generated"} }, want: "not a Go source file"},
		{name: "percent", mutate: func(value *policy) { value.Targets.DiffPercent = 101 }, want: "between 0 and 100"},
		{name: "negative counts", mutate: func(value *policy) { value.Baseline.Packages["."] = counts{Covered: -1, Total: 2} }, want: "nonnegative"},
		{name: "covered exceeds total", mutate: func(value *policy) { value.Baseline.Packages["."] = counts{Covered: 3, Total: 2} }, want: "exceeds total"},
		{name: "overall mismatch", mutate: func(value *policy) { value.Baseline.Overall.Covered++ }, want: "package sum"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configured := validPolicy()
			test.mutate(&configured)
			err := validatePolicy(configured)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodePolicyRequiresFieldsAndRejectsUnknownFields(t *testing.T) {
	for _, input := range []string{
		`{"schema_version":1}`,
		`{"schema_version":1,"unknown":true}`,
	} {
		if _, err := decodePolicy(strings.NewReader(input)); err == nil {
			t.Fatalf("decodePolicy(%s) unexpectedly succeeded", input)
		}
	}
	encoded, err := json.Marshal(validPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "coverage_platform")
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePolicy(bytes.NewReader(encoded)); err == nil || !strings.Contains(err.Error(), "required top-level") {
		t.Fatalf("missing coverage_platform error = %v", err)
	}
}

func TestRunGenerateWritesBoundProvenanceAndInvalidatesItOnFailure(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("coverage policy is intentionally linux/amd64")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	files := map[string]string{
		"go.mod":       "module github.com/alkaid/pitaya/v2\n\ngo 1.26.5\n",
		"root.go":      "package root\n\nfunc Value() int { return 1 }\n",
		"root_test.go": "package root\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(Value()) } }\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "passing")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	configured := validPolicy()
	configured.BaseSHA = head
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	writeJSON(t, policyPath, configured)
	profile := filepath.Join(t.TempDir(), "coverage.out")
	args := []string{"generate", "-repo", repo, "-policy", policyPath, "-profile", profile}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("generate exit = %d, stderr = %s", code, stderr.String())
	}
	if err := validateProfileProvenances([]string{profile}, head, configured.CoveragePlatform); err != nil {
		t.Fatalf("generated provenance rejected: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "root_test.go"), []byte("package root\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { t.Fatal(\"expected failure\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "root_test.go")
	git(t, repo, "commit", "-qm", "failing")
	t.Setenv("GOFLAGS", "-run=^$")
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("failed generate exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(profileProvenancePath(profile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed generation left provenance: %v", err)
	}
}

func TestValidateProfileProvenanceRejectsWrongCommitAndTampering(t *testing.T) {
	profile := writeProfile(t, "mode: set\ngithub.com/alkaid/pitaya/v2/root.go:1.1,2.1 1 1\n")
	platform := coveragePlatform{GOOS: "linux", GOARCH: "amd64"}
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	writeProfileMetadata(t, profile, first, platform)
	if err := validateProfileProvenances([]string{profile}, second, platform); err == nil || !strings.Contains(err.Error(), "names commit") {
		t.Fatalf("wrong-commit error = %v", err)
	}
	if err := os.WriteFile(profile, []byte("mode: set\ngithub.com/alkaid/pitaya/v2/root.go:1.1,2.1 1 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateProfileProvenances([]string{profile}, first, platform); err == nil || !strings.Contains(err.Error(), "profile_sha256") {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestRunUpdateWritesDeterministicPolicy(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "root.go"), []byte("package root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "baseline")
	sha := gitOutput(t, repo, "rev-parse", "HEAD")

	directory := t.TempDir()
	policyPath := filepath.Join(directory, "coverage.json")
	configured := validPolicy()
	configured.BaseSHA = sha
	configured.Baseline = baseline{Overall: counts{}, Packages: map[string]counts{}}
	writeJSON(t, policyPath, configured)
	firstProfile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/root.go:1.1,2.1 1 1
github.com/alkaid/pitaya/v2/root.go:3.1,4.1 1 0
`)
	secondProfile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/root.go:1.1,2.1 1 0
github.com/alkaid/pitaya/v2/root.go:3.1,4.1 1 1
`)
	writeProfileMetadata(t, firstProfile, sha, configured.CoveragePlatform)
	writeProfileMetadata(t, secondProfile, sha, configured.CoveragePlatform)
	args := []string{"update", "-repo", repo, "-policy", policyPath, "-profile", firstProfile, "-profile", secondProfile, "-base-sha", sha}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("first update exit = %d, stderr = %s", code, stderr.String())
	}
	first, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("second update exit = %d, stderr = %s", code, stderr.String())
	}
	second, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("update is nondeterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !bytes.HasSuffix(first, []byte("\n")) || bytes.Contains(first, []byte("\t")) {
		t.Fatalf("policy does not use canonical indented JSON: %q", first)
	}
	updated, err := readPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BaseSHA != sha || updated.Baseline.Overall != (counts{Covered: 1, Total: 2}) {
		t.Fatalf("updated policy = %#v", updated)
	}

	stdout.Reset()
	stderr.Reset()
	missingSHA := strings.Repeat("0", 40)
	missingArgs := append([]string(nil), args...)
	missingArgs[len(missingArgs)-1] = missingSHA
	if code := run(missingArgs, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "resolve base SHA") {
		t.Fatalf("missing SHA exit = %d, stderr = %q", code, stderr.String())
	}

	if err := os.WriteFile(filepath.Join(repo, "root.go"), []byte("package root\n\nconst Changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "root.go")
	git(t, repo, "commit", "-qm", "next")
	nextSHA := gitOutput(t, repo, "rev-parse", "HEAD")
	writeProfileMetadata(t, firstProfile, nextSHA, configured.CoveragePlatform)
	mixedArgs := []string{"update", "-repo", repo, "-policy", policyPath, "-profile", firstProfile, "-profile", secondProfile, "-base-sha", nextSHA}
	stdout.Reset()
	stderr.Reset()
	if code := run(mixedArgs, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "names commit "+sha) {
		t.Fatalf("mixed provenance exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunCheckEndToEnd(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "coverage@example.invalid")
	git(t, repo, "config", "user.name", "Coverage Test")
	writeRegisteredNestedModules(t, repo)
	file := filepath.Join(repo, "live.go")
	if err := os.WriteFile(file, []byte("package live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("package live\n\nfunc Live() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "live.go")
	git(t, repo, "commit", "-qm", "head")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	profile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/live.go:3.1,3.15 1 1
`)
	configured := validPolicy()
	configured.BaseSHA = base
	writeProfileMetadata(t, profile, head, configured.CoveragePlatform)
	configured.Baseline = baseline{
		Overall:  counts{Covered: 1, Total: 1},
		Packages: map[string]counts{".": {Covered: 1, Total: 1}},
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	writeJSON(t, policyPath, configured)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", base, "-head", head,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "diff 100.00% (1/1)") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", head, "-head", head,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "must differ") {
		t.Fatalf("equal endpoints exit = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", base, "-head", base,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "does not match checked out HEAD") {
		t.Fatalf("non-checkout head exit = %d, stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", "HEAD", "-head", head,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "base SHA") {
		t.Fatalf("symbolic base exit = %d, stderr = %q", code, stderr.String())
	}

	writeProfileMetadata(t, profile, base, configured.CoveragePlatform)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", base, "-head", head,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "names commit "+base) {
		t.Fatalf("wrong profile commit exit = %d, stderr = %q", code, stderr.String())
	}
	writeProfileMetadata(t, profile, head, configured.CoveragePlatform)

	unrelated := gitOutput(t, repo, "commit-tree", head+"^{tree}", "-m", "unrelated")
	configured.BaseSHA = unrelated
	writeJSON(t, policyPath, configured)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", profile,
		"-repo", repo, "-base", base, "-head", head,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "is not an ancestor") {
		t.Fatalf("unrelated policy base exit = %d, stderr = %q", code, stderr.String())
	}
	configured.BaseSHA = base

	uncoveredProfile := writeProfile(t, `mode: set
github.com/alkaid/pitaya/v2/live.go:3.1,3.15 1 0
`)
	writeProfileMetadata(t, uncoveredProfile, head, configured.CoveragePlatform)
	configured.Baseline = baseline{
		Overall:  counts{Covered: 0, Total: 1},
		Packages: map[string]counts{".": {Covered: 0, Total: 1}},
	}
	writeJSON(t, policyPath, configured)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"check", "-policy", policyPath, "-profile", uncoveredProfile,
		"-repo", repo, "-base", base, "-head", head,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "diff coverage 0.00% is below 90.00% (0/1)") {
		t.Fatalf("uncovered diff exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunCheckRejectsRepeatedProfiles(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"check", "-policy", "policy.json", "-profile", "one.out", "-profile", "two.out", "-base", "base", "-head", "head",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "exactly one -profile") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunSubcommandHelpSucceeds(t *testing.T) {
	for _, subcommand := range []string{"generate", "check", "update"} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := run([]string{subcommand, "-h"}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s -h exit = %d, stderr = %q", subcommand, code, stderr.String())
		}
	}
}

func TestRunCheckRejectsPlatformMismatch(t *testing.T) {
	configured := validPolicy()
	configured.CoveragePlatform.GOOS = "windows"
	if runtime.GOOS == "windows" {
		configured.CoveragePlatform.GOOS = "linux"
	}
	configured.CoveragePlatform.GOARCH = runtime.GOARCH
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	writeJSON(t, policyPath, configured)
	sha := strings.Repeat("0", 40)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"check", "-policy", policyPath, "-profile", "coverage.out",
		"-base", sha, "-head", sha,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "current runtime") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func validPolicy() policy {
	return policy{
		SchemaVersion: policySchemaVersion,
		BaseSHA:       strings.Repeat("a", 40),
		CoveragePlatform: coveragePlatform{
			GOOS:   "linux",
			GOARCH: "amd64",
		},
		ExcludedFiles: []string{},
		Targets: targets{
			OverallPercent:  70,
			PackagePercent:  map[string]float64{".": 80},
			DiffPercent:     90,
			EnforceAbsolute: false,
		},
		Baseline: baseline{
			Overall:  counts{Covered: 1, Total: 2},
			Packages: map[string]counts{".": {Covered: 1, Total: 2}},
		},
	}
}

func writeProfile(t *testing.T, contents string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return filename
}

func writeJSON(t *testing.T, filename string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRegisteredNestedModules(t *testing.T, repo string) {
	t.Helper()
	for _, root := range separatelyVerifiedModuleRoots {
		directory := filepath.Join(repo, root)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.invalid/"+root+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "module.go"), []byte("package nested\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeProfileMetadata(t *testing.T, profile, commit string, platform coveragePlatform) {
	t.Helper()
	if err := writeProfileProvenance(profile, commit, platform); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
