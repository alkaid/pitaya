package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadManifest(t *testing.T) {
	t.Parallel()

	path := writeManifest(t, `{
  "schema_version": 1,
  "targets": [
    {"package": "./a", "target": "FuzzA"},
    {"package": "./b/nested", "target": "FuzzB"}
  ]
}`)
	manifest, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Targets) != 2 || manifest.Targets[1].Package != "./b/nested" || manifest.Targets[1].Target != "FuzzB" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
}

func TestLoadManifestRejectsInvalidPolicies(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		manifest string
		want     string
	}{
		"unknown field": {
			`{"schema_version":1,"targets":[{"package":"./a","target":"FuzzA","extra":true}]}`,
			"unknown field",
		},
		"wrong schema": {
			`{"schema_version":2,"targets":[{"package":"./a","target":"FuzzA"}]}`,
			"unsupported",
		},
		"empty": {
			`{"schema_version":1,"targets":[]}`,
			"no targets",
		},
		"duplicate": {
			`{"schema_version":1,"targets":[{"package":"./a","target":"FuzzA"},{"package":"./a","target":"FuzzA"}]}`,
			"duplicate",
		},
		"unsorted": {
			`{"schema_version":1,"targets":[{"package":"./b","target":"FuzzB"},{"package":"./a","target":"FuzzA"}]}`,
			"sorted",
		},
		"invalid package": {
			`{"schema_version":1,"targets":[{"package":"../a","target":"FuzzA"}]}`,
			"invalid package",
		},
		"package wildcard": {
			`{"schema_version":1,"targets":[{"package":"./...","target":"FuzzA"}]}`,
			"invalid package",
		},
		"invalid target": {
			`{"schema_version":1,"targets":[{"package":"./a","target":"TestA"}]}`,
			"invalid target",
		},
		"lowercase fuzz suffix": {
			`{"schema_version":1,"targets":[{"package":"./a","target":"Fuzzlower"}]}`,
			"invalid target",
		},
		"trailing value": {
			`{"schema_version":1,"targets":[{"package":"./a","target":"FuzzA"}]} {}`,
			"multiple JSON values",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadManifest(writeManifest(t, test.manifest))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadManifest() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDiscoverFuzzTargetsFromPackages(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	firstDir := filepath.Join(repo, "first")
	secondDir := filepath.Join(repo, "second")
	thirdDir := filepath.Join(repo, "third")
	for _, directory := range []string{firstDir, secondDir, thirdDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(firstDir, "first_test.go"), `package first
import "testing"
func FuzzFirst(f *testing.F) {}
func Fuzzlower(t *testing.T) {}
`)
	writeFile(t, filepath.Join(secondDir, "second_test.go"), `package second_test
import test "testing"
func FuzzSecond(f *test.F) {}
`)
	writeFile(t, filepath.Join(thirdDir, "third_test.go"), `package third
import . "testing"
func FuzzThird(*F) () {}
`)

	targets, err := discoverFuzzTargetsFromPackages(repo, []goListPackage{
		{Dir: firstDir, TestGoFiles: []string{"first_test.go"}},
		{Dir: secondDir, XTestGoFiles: []string{"second_test.go"}},
		{Dir: thirdDir, TestGoFiles: []string{"third_test.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []fuzzTarget{
		{Package: "./first", Target: "FuzzFirst"},
		{Package: "./second", Target: "FuzzSecond"},
		{Package: "./third", Target: "FuzzThird"},
	}
	if !equalTargets(targets, want) {
		t.Fatalf("discovered targets = %#v, want %#v", targets, want)
	}
}

func TestDiscoverFuzzTargetsRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "invalid_test.go"), `package invalid
import "testing"
func FuzzInvalid(t *testing.T) {}
`)
	_, err := discoverFuzzTargetsFromPackages(repo, []goListPackage{{
		Dir: repo, TestGoFiles: []string{"invalid_test.go"},
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid fuzz target signature") {
		t.Fatalf("expected invalid signature error, got %v", err)
	}
}

func TestDiscoverFuzzTargetsRejectsInactiveTarget(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "active_test.go"), `package platform
import "testing"
func FuzzActive(f *testing.F) {}
`)
	writeFile(t, filepath.Join(repo, "platform_windows_test.go"), `package platform
import "testing"
func FuzzWindows(f *testing.F) {}
`)

	_, err := discoverFuzzTargetsFromPackages(repo, []goListPackage{{
		Dir:         repo,
		TestGoFiles: []string{"active_test.go"},
	}})
	if err == nil || !strings.Contains(err.Error(), "inactive fuzz target FuzzWindows") {
		t.Fatalf("expected inactive fuzz target error, got %v", err)
	}
}

func TestDiscoverFuzzTargetsRejectsBuildConstrainedTarget(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/fuzzplatform\n\ngo 1.26.5\n")
	activeDir := filepath.Join(repo, "active")
	inactiveDir := filepath.Join(repo, "inactive")
	for _, directory := range []string{activeDir, inactiveDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(activeDir, "active.go"), "package active\n")
	writeFile(t, filepath.Join(inactiveDir, "impl.go"), `//go:build fuzzgate_inactive

package inactive
`)
	writeFile(t, filepath.Join(inactiveDir, "impl_test.go"), `//go:build fuzzgate_inactive

package inactive

import "testing"

func FuzzInactive(f *testing.F) {}
`)

	_, err := discoverFuzzTargets(commandOptions{repo: repo, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "inactive fuzz target FuzzInactive") {
		t.Fatalf("expected build-constrained fuzz target error, got %v", err)
	}
}

func TestConsumeEventsAndValidateSeeds(t *testing.T) {
	t.Parallel()

	input := encodeEvents(t,
		testEvent{Action: "run", Test: "FuzzTarget"},
		testEvent{Action: "run", Test: "FuzzTarget/seed#0"},
		testEvent{Action: "pass", Test: "FuzzTarget/seed#0"},
		testEvent{Action: "run", Test: "FuzzTarget/0123456789abcdef"},
		testEvent{Action: "pass", Test: "FuzzTarget/0123456789abcdef"},
		testEvent{Action: "output", Test: "FuzzTarget", Output: "fuzz: elapsed: 0s, gathering baseline coverage: 6/6 completed, now fuzzing with 2 workers\n"},
		testEvent{Action: "output", Test: "FuzzTarget", Output: "fuzz: elapsed: 1s, execs: 42 (42/sec)\n"},
		testEvent{Action: "pass", Test: "FuzzTarget"},
	)
	var raw, display bytes.Buffer
	summary, err := consumeEvents(strings.NewReader(input), &raw, &display, "FuzzTarget")
	if err != nil {
		t.Fatal(err)
	}
	if err := summary.validateSeeds("FuzzTarget"); err != nil {
		t.Fatal(err)
	}
	if err := summary.validateFuzz("FuzzTarget"); err != nil {
		t.Fatal(err)
	}
	if summary.maxExecs != 42 || !strings.Contains(display.String(), "execs: 42") || raw.Len() == 0 {
		t.Fatalf("unexpected event summary: %#v", summary)
	}
}

func TestFuzzValidationRejectsBaselineOnlyExecutions(t *testing.T) {
	t.Parallel()

	summary := eventSummary{
		targetRun:  true,
		targetPass: true,
		mutating:   true,
		baseline:   12,
		maxExecs:   12,
	}
	if err := summary.validateFuzz("FuzzTarget"); err == nil || !strings.Contains(err.Error(), "zero mutation") {
		t.Fatalf("expected baseline-only failure, got %v", err)
	}
}

func TestSeedValidationRejectsHollowGreen(t *testing.T) {
	t.Parallel()

	summary := eventSummary{
		targetRun:  true,
		targetPass: true,
		corpusRun:  map[string]bool{},
		corpusPass: map[string]bool{},
		corpusBad:  map[string]bool{},
	}
	if err := summary.validateSeeds("FuzzTarget"); err == nil || !strings.Contains(err.Error(), "no seed") {
		t.Fatalf("expected missing-seed failure, got %v", err)
	}
	if err := summary.validateFuzz("FuzzTarget"); err == nil || !strings.Contains(err.Error(), "zero mutation") {
		t.Fatalf("expected zero-execution failure, got %v", err)
	}
}

func TestSeedValidationRejectsSkippedSeed(t *testing.T) {
	t.Parallel()

	summary := eventSummary{
		targetRun:  true,
		targetPass: true,
		corpusRun:  map[string]bool{"FuzzTarget/corpus-hash": true},
		corpusPass: map[string]bool{"FuzzTarget/corpus-hash": true},
		corpusBad:  map[string]bool{"FuzzTarget/corpus-hash": true},
	}
	if err := summary.validateSeeds("FuzzTarget"); err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("expected skipped-corpus failure, got %v", err)
	}
}

func TestConsumeEventsIgnoresUnscopedFuzzProgress(t *testing.T) {
	t.Parallel()

	input := encodeEvents(t,
		testEvent{Action: "run", Test: "FuzzTarget"},
		testEvent{Action: "output", Output: "fuzz: elapsed: 1s, execs: 42 (42/sec)\n"},
		testEvent{Action: "output", Test: "FuzzTarget", Output: "application log execs: 99\n"},
		testEvent{Action: "pass", Test: "FuzzTarget"},
	)
	summary, err := consumeEvents(strings.NewReader(input), nil, &bytes.Buffer{}, "FuzzTarget")
	if err != nil {
		t.Fatal(err)
	}
	if err := summary.validateFuzz("FuzzTarget"); err == nil || !strings.Contains(err.Error(), "zero mutation") {
		t.Fatalf("expected zero-execution failure, got %v", err)
	}
}

func TestSelectTargets(t *testing.T) {
	t.Parallel()

	targets := []fuzzTarget{{Package: "./a", Target: "FuzzA"}, {Package: "./b", Target: "FuzzB"}}
	selected, err := selectTargets(targets, "./b", "FuzzB")
	if err != nil || len(selected) != 1 || selected[0] != targets[1] {
		t.Fatalf("selectTargets() = %#v, %v", selected, err)
	}
	if _, err := selectTargets(targets, "./b", "FuzzMissing"); err == nil {
		t.Fatal("expected an unregistered target error")
	}
}

func TestTargetArtifactDirIsBoundedAndCollisionFree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targets := []fuzzTarget{
		{Package: "./" + strings.Repeat("p", 512), Target: "FuzzLongPackage"},
		{Package: "./case", Target: "FuzzFoo"},
		{Package: "./case", Target: "FuzzFOO"},
	}
	seenFolded := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		directory := targetArtifactDir(root, target)
		relative, err := filepath.Rel(root, directory)
		if err != nil {
			t.Fatal(err)
		}
		components := strings.Split(filepath.ToSlash(relative), "/")
		if len(components) != 3 || components[0] != "targets" {
			t.Fatalf("artifact directory has unexpected shape: %s", directory)
		}
		if len(components[1]) != len("pkg-")+sha256.Size*2 {
			t.Fatalf("package artifact component has length %d: %s", len(components[1]), components[1])
		}
		if len(components[2]) != len("target-")+sha256.Size*2 {
			t.Fatalf("target artifact component has length %d: %s", len(components[2]), components[2])
		}
		for _, component := range append(components, "target.json") {
			if strings.HasPrefix(component, ".") {
				t.Fatalf("artifact component is hidden: %s", component)
			}
			if len(component) > 255 {
				t.Fatalf("artifact component exceeds NAME_MAX: %s", component)
			}
		}
		folded := strings.ToLower(filepath.Clean(directory))
		if _, ok := seenFolded[folded]; ok {
			t.Fatalf("case-folded artifact directory collision: %s", directory)
		}
		seenFolded[folded] = struct{}{}

		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeTargetArtifactMetadata(directory, target); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(filepath.Join(directory, "target.json"))
		if err != nil {
			t.Fatal(err)
		}
		var decoded fuzzTarget
		if err := json.Unmarshal(contents, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != target {
			t.Fatalf("artifact metadata = %#v, want %#v", decoded, target)
		}
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()

	if got, err := parseDuration("fuzztime", "2m", time.Hour); err != nil || got != 2*time.Minute {
		t.Fatalf("parseDuration() = %s, %v", got, err)
	}
	for _, value := range []string{"0s", "-1s", "61m", "invalid"} {
		if _, err := parseDuration("fuzztime", value, time.Hour); err == nil {
			t.Fatalf("parseDuration(%q) unexpectedly succeeded", value)
		}
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() exit = %d, want 2", code)
	}
}

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeFile(t, path, contents)
	return path
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func encodeEvents(t *testing.T, events ...testEvent) string {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return output.String()
}
