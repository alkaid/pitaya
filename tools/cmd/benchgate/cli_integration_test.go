package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProductionCLIStampCompareWritesGateReport(t *testing.T) {
	tests := []struct {
		name            string
		candidateNS     int
		wantCode        int
		wantPassed      bool
		wantRegressions int
	}{
		{name: "pass", candidateNS: 105, wantCode: 0, wantPassed: true},
		{name: "regression", candidateNS: 120, wantCode: 3, wantRegressions: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProductionCLIFixture(t, test.candidateNS)
			stampProductionPair(t, fixture)

			reportPath := filepath.Join(fixture.outputDir, "gate.json")
			code, stdout, stderr := runProductionCompare(fixture, reportPath)
			contents, readErr := os.ReadFile(reportPath)
			if code != test.wantCode || readErr != nil {
				t.Fatalf("compare exited %d (want %d), report read error %v\nstdout:\n%s\nstderr:\n%s", code, test.wantCode, readErr, stdout, stderr)
			}
			var report gateReport
			if err := json.Unmarshal(contents, &report); err != nil {
				t.Fatalf("decode gate report: %v", err)
			}
			if report.RunID != fixture.runID || report.BaseSHA != fixture.baseSHA || report.CandidateSHA != fixture.candidateSHA {
				t.Fatalf("gate report identity = run %q base %q candidate %q", report.RunID, report.BaseSHA, report.CandidateSHA)
			}
			if report.SchemaVersion != reportSchemaVersion || report.GateSourceSHA != fixture.gateSourceSHA {
				t.Fatalf("gate report schema/source = %d/%q, want %d/%q", report.SchemaVersion, report.GateSourceSHA, reportSchemaVersion, fixture.gateSourceSHA)
			}
			if report.Passed != test.wantPassed || report.RegressionCount != test.wantRegressions {
				t.Fatalf("gate report outcome = passed %t, regressions %d; want passed %t, regressions %d", report.Passed, report.RegressionCount, test.wantPassed, test.wantRegressions)
			}
			if len(report.Comparisons) != len(requiredMetrics) {
				t.Fatalf("gate report has %d comparisons, want %d", len(report.Comparisons), len(requiredMetrics))
			}
			if test.wantCode == 3 && !strings.Contains(stderr, "benchmark regression") {
				t.Fatalf("regression stderr = %q, want regression diagnostic", stderr)
			}
			expect := "pass"
			if test.wantCode == 3 {
				expect = "regression"
			}
			verifyCode, _, verifyStderr := runProductionVerifyReport(fixture, reportPath, expect)
			if verifyCode != 0 {
				t.Fatalf("verify-report exited %d, stderr %q", verifyCode, verifyStderr)
			}
		})
	}
}

func TestProductionCLICompareRejectsTampering(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *productionCLIFixture)
		want   string
	}{
		{
			name: "sealed result digest",
			tamper: func(t *testing.T, fixture *productionCLIFixture) {
				replaceProductionArtifact(t, fixture.candidateResult, "100 ns/op", "101 ns/op")
			},
			want: "candidate result digest is",
		},
		{
			name: "metadata result digest",
			tamper: func(t *testing.T, fixture *productionCLIFixture) {
				metadata := readProductionMetadata(t, fixture.candidateMetadata)
				metadata.ResultSHA256 = strings.Repeat("0", 64)
				mustWriteFile(t, fixture.candidateMetadata, mustJSON(t, metadata))
			},
			want: "candidate result digest is",
		},
		{
			name: "embedded provenance",
			tamper: func(t *testing.T, fixture *productionCLIFixture) {
				replaceProductionArtifact(t, fixture.baseResult, "bench-run-id: "+fixture.runID+"\n", "bench-run-id: forged-run\n")
				contents := mustReadProductionFile(t, fixture.baseResult)
				digest := sha256.Sum256(contents)
				metadata := readProductionMetadata(t, fixture.baseMetadata)
				// Refreshing the sidecar digest forces compare past the outer seal and
				// proves the independently embedded identity is still enforced.
				metadata.ResultSHA256 = fmt.Sprintf("%x", digest[:])
				mustWriteFile(t, fixture.baseMetadata, mustJSON(t, metadata))
			},
			want: "base embedded provenance: bench-run-id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProductionCLIFixture(t, 100)
			stampProductionPair(t, fixture)
			test.tamper(t, fixture)

			reportPath := filepath.Join(fixture.outputDir, "tampered-gate.json")
			code, _, stderr := runProductionCompare(fixture, reportPath)
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Fatalf("tampered compare exited %d, stderr %q; want failure containing %q", code, stderr, test.want)
			}
			if _, err := os.Lstat(reportPath); !os.IsNotExist(err) {
				t.Fatalf("tampered comparison report exists or cannot be inspected: %v", err)
			}
		})
	}
}

func TestProductionCLIStampRejectsRawMTimeOutsideRunInterval(t *testing.T) {
	fixture := newProductionCLIFixture(t, 100)
	writeProductionRunnerProbe(t, fixture, "base")
	outside := fixture.baseStarted.Add(-3 * time.Second)
	setProductionModTime(t, fixture.baseRaw, outside)
	*fixture.clock = fixture.baseFinished.Add(time.Second)

	var stdout, stderr bytes.Buffer
	code := run(fixture.stampArgs("base"), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "outside run interval") {
		t.Fatalf("stamp exited %d, stderr %q; want raw mtime rejection", code, stderr.String())
	}
	for _, filename := range []string{fixture.baseResult, fixture.baseMetadata} {
		if _, err := os.Lstat(filename); !os.IsNotExist(err) {
			t.Fatalf("rejected stamp output %s exists or cannot be inspected: %v", filename, err)
		}
	}
}

func TestProductionCLIStampRejectsInvalidOrDriftedRunnerProbe(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *productionCLIFixture)
		want   string
	}{
		{
			name: "invalid JSON",
			mutate: func(t *testing.T, fixture *productionCLIFixture) {
				mustWriteFile(t, fixture.baseRunnerProbe, []byte("{"))
			},
			want: "read runner probe",
		},
		{
			name: "runner state drift",
			mutate: func(t *testing.T, fixture *productionCLIFixture) {
				var probe runnerProvenance
				if err := json.Unmarshal(mustReadProductionFile(t, fixture.baseRunnerProbe), &probe); err != nil {
					t.Fatal(err)
				}
				probe.GOGC = "changed-after-harness"
				mustWriteFile(t, fixture.baseRunnerProbe, mustJSON(t, probe))
			},
			want: "runner state differs after harness",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProductionCLIFixture(t, 100)
			writeProductionRunnerProbe(t, fixture, "base")
			test.mutate(t, fixture)
			*fixture.clock = fixture.baseFinished.Add(time.Second)

			var stdout, stderr bytes.Buffer
			if code := run(fixture.stampArgs("base"), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stamp exited %d, stderr %q; want failure containing %q", code, stderr.String(), test.want)
			}
			for _, filename := range []string{fixture.baseResult, fixture.baseMetadata} {
				if _, err := os.Lstat(filename); !os.IsNotExist(err) {
					t.Fatalf("rejected stamp output %s exists or cannot be inspected: %v", filename, err)
				}
			}
		})
	}
}

type productionCLIFixture struct {
	basePolicyPath       string
	candidatePolicyPath  string
	baseRepo             string
	candidateRepo        string
	baseSHA              string
	candidateSHA         string
	baseRaw              string
	candidateRaw         string
	baseRunnerProbe      string
	candidateRunnerProbe string
	baseResult           string
	candidateResult      string
	baseMetadata         string
	candidateMetadata    string
	outputDir            string
	runID                string
	runnerID             string
	gateSourceSHA        string
	baseStarted          time.Time
	baseFinished         time.Time
	candidateStarted     time.Time
	candidateFinished    time.Time
	clock                *time.Time
}

func newProductionCLIFixture(t *testing.T, candidateNS int) *productionCLIFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("production runner provenance currently requires Linux /proc and cgroup data")
	}
	installProductionProvenanceCommands(t)

	source := t.TempDir()
	gitCommand(t, source, "init", "-q")
	configureGitIdentity(t, source)
	policy := validTestPolicy()
	policy.BenchmarkNames = []string{"Alpha"}
	policy.ApparatusPaths = []string{"apparatus.txt"}
	mustWriteFile(t, filepath.Join(source, "apparatus.txt"), []byte("fixed benchmark apparatus\n"))
	mustWriteFile(t, filepath.Join(source, "benchmark-policy.json"), mustJSON(t, policy))
	mustWriteFile(t, filepath.Join(source, "workload.txt"), []byte("base implementation\n"))
	gitCommand(t, source, "add", "apparatus.txt", "benchmark-policy.json", "workload.txt")
	gitCommand(t, source, "commit", "-q", "-m", "base")
	baseSHA := gitOutput(t, source, "rev-parse", "HEAD")
	// The candidate is a real descendant while the policy-owned apparatus stays
	// byte-identical, which is the comparison boundary enforced by production.
	mustWriteFile(t, filepath.Join(source, "workload.txt"), []byte("candidate implementation\n"))
	gitCommand(t, source, "add", "workload.txt")
	gitCommand(t, source, "commit", "-q", "-m", "candidate")
	candidateSHA := gitOutput(t, source, "rev-parse", "HEAD")

	checkouts := t.TempDir()
	baseRepo := filepath.Join(checkouts, "base")
	candidateRepo := filepath.Join(checkouts, "candidate")
	gitGlobalCommand(t, "clone", "-q", source, baseRepo)
	gitGlobalCommand(t, "clone", "-q", source, candidateRepo)
	gitCommand(t, baseRepo, "checkout", "-q", "--detach", baseSHA)
	gitCommand(t, candidateRepo, "checkout", "-q", "--detach", candidateSHA)
	if err := requireCleanCheckout(baseRepo); err != nil {
		t.Fatalf("base checkout: %v", err)
	}
	if err := requireCleanCheckout(candidateRepo); err != nil {
		t.Fatalf("candidate checkout: %v", err)
	}

	outputDir := t.TempDir()

	anchor := time.Now().UTC().Truncate(time.Second)
	clock := new(time.Time)
	*clock = anchor
	previousNowUTC := nowUTC
	nowUTC = func() time.Time { return *clock }
	t.Cleanup(func() { nowUTC = previousNowUTC })

	fixture := &productionCLIFixture{
		basePolicyPath:       filepath.Join(baseRepo, "benchmark-policy.json"),
		candidatePolicyPath:  filepath.Join(candidateRepo, "benchmark-policy.json"),
		baseRepo:             baseRepo,
		candidateRepo:        candidateRepo,
		baseSHA:              baseSHA,
		candidateSHA:         candidateSHA,
		baseRaw:              filepath.Join(outputDir, "base.raw.txt"),
		candidateRaw:         filepath.Join(outputDir, "candidate.raw.txt"),
		baseRunnerProbe:      filepath.Join(outputDir, "base.runner.json"),
		candidateRunnerProbe: filepath.Join(outputDir, "candidate.runner.json"),
		baseResult:           filepath.Join(outputDir, "base.txt"),
		candidateResult:      filepath.Join(outputDir, "candidate.txt"),
		baseMetadata:         filepath.Join(outputDir, "base.json"),
		candidateMetadata:    filepath.Join(outputDir, "candidate.json"),
		outputDir:            outputDir,
		runID:                "production-cli-run",
		runnerID:             "production-cli-runner",
		gateSourceSHA:        testGateSourceSHA,
		baseStarted:          anchor.Add(-4 * time.Minute),
		baseFinished:         anchor.Add(-3 * time.Minute),
		candidateStarted:     anchor.Add(-2 * time.Minute),
		candidateFinished:    anchor.Add(-time.Minute),
		clock:                clock,
	}
	mustWriteFile(t, fixture.baseRaw, productionBenchmarkOutput(100))
	mustWriteFile(t, fixture.candidateRaw, productionBenchmarkOutput(candidateNS))
	setProductionModTime(t, fixture.baseRaw, fixture.baseStarted.Add(30*time.Second))
	setProductionModTime(t, fixture.candidateRaw, fixture.candidateStarted.Add(30*time.Second))
	return fixture
}

func (fixture *productionCLIFixture) stampArgs(role string) []string {
	repo, raw, runnerProbe, result, metadata, commit := fixture.baseRepo, fixture.baseRaw, fixture.baseRunnerProbe, fixture.baseResult, fixture.baseMetadata, fixture.baseSHA
	policyPath := fixture.basePolicyPath
	started, finished := fixture.baseStarted, fixture.baseFinished
	if role == "candidate" {
		repo, raw, runnerProbe, result, metadata, commit = fixture.candidateRepo, fixture.candidateRaw, fixture.candidateRunnerProbe, fixture.candidateResult, fixture.candidateMetadata, fixture.candidateSHA
		policyPath = fixture.candidatePolicyPath
		started, finished = fixture.candidateStarted, fixture.candidateFinished
	}
	return []string{
		"stamp", "-policy", policyPath, "-repo", repo,
		"-raw", raw, "-runner-probe", runnerProbe, "-result", result, "-metadata", metadata,
		"-role", role, "-commit", commit, "-run-id", fixture.runID, "-runner-id", fixture.runnerID,
		"-gate-source-sha", fixture.gateSourceSHA,
		"-started-at", timestamp(started), "-finished-at", timestamp(finished),
	}
}

func (fixture *productionCLIFixture) compareArgs(reportPath string) []string {
	return []string{
		"compare", "-base-policy", fixture.basePolicyPath, "-candidate-policy", fixture.candidatePolicyPath,
		"-base-repo", fixture.baseRepo, "-candidate-repo", fixture.candidateRepo,
		"-base", fixture.baseResult, "-candidate", fixture.candidateResult,
		"-base-metadata", fixture.baseMetadata, "-candidate-metadata", fixture.candidateMetadata,
		"-base-sha", fixture.baseSHA, "-candidate-sha", fixture.candidateSHA,
		"-gate-source-sha", fixture.gateSourceSHA, "-run-id", fixture.runID, "-runner-id", fixture.runnerID, "-report", reportPath,
	}
}

func stampProductionPair(t *testing.T, fixture *productionCLIFixture) {
	t.Helper()
	for _, measurement := range []struct {
		role string
		now  time.Time
	}{
		{role: "base", now: fixture.baseFinished.Add(time.Second)},
		{role: "candidate", now: fixture.candidateFinished.Add(time.Second)},
	} {
		*fixture.clock = measurement.now
		writeProductionRunnerProbe(t, fixture, measurement.role)
		var stdout, stderr bytes.Buffer
		if code := run(fixture.stampArgs(measurement.role), &stdout, &stderr); code != 0 {
			t.Fatalf("%s stamp exited %d\nstdout:\n%s\nstderr:\n%s", measurement.role, code, stdout.String(), stderr.String())
		}
	}
}

func writeProductionRunnerProbe(t *testing.T, fixture *productionCLIFixture, role string) {
	t.Helper()
	filename := fixture.baseRunnerProbe
	if role == "candidate" {
		filename = fixture.candidateRunnerProbe
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe-runner", "-runner-id", fixture.runnerID}, &stdout, &stderr); code != 0 {
		t.Fatalf("%s runner probe exited %d, stderr %q", role, code, stderr.String())
	}
	mustWriteFile(t, filename, stdout.Bytes())
}

func runProductionCompare(fixture *productionCLIFixture, reportPath string) (int, string, string) {
	*fixture.clock = fixture.candidateFinished.Add(time.Minute)
	var stdout, stderr bytes.Buffer
	code := run(fixture.compareArgs(reportPath), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runProductionVerifyReport(fixture *productionCLIFixture, reportPath, expect string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	policy, policyDigest, err := readPolicy(fixture.basePolicyPath)
	if err != nil {
		return 1, "", fmt.Sprintf("read fixture policy: %v", err)
	}
	apparatusDigest, err := apparatusDigestForPolicy(fixture.baseRepo, policy)
	if err != nil {
		return 1, "", fmt.Sprintf("fingerprint fixture apparatus: %v", err)
	}
	code := run([]string{
		"verify-report", "-report", reportPath,
		"-policy", fixture.basePolicyPath,
		"-base-result", fixture.baseResult, "-candidate-result", fixture.candidateResult,
		"-base-metadata", fixture.baseMetadata, "-candidate-metadata", fixture.candidateMetadata,
		"-base-sha", fixture.baseSHA, "-candidate-sha", fixture.candidateSHA,
		"-gate-source-sha", fixture.gateSourceSHA, "-run-id", fixture.runID,
		"-policy-sha256", policyDigest, "-apparatus-sha256", apparatusDigest,
		"-runner-id", fixture.runnerID, "-expect", expect,
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func productionBenchmarkOutput(nsPerOp int) []byte {
	var output strings.Builder
	fmt.Fprintf(&output, "goos: %s\ngoarch: %s\npkg: %s\ncpu: Production CLI Test CPU\n\n", runtime.GOOS, runtime.GOARCH, testBenchmarkPackage)
	for range minimumSamples {
		fmt.Fprintf(&output, "BenchmarkAlpha-8 1 %d ns/op 64 B/op 1 allocs/op\n", nsPerOp)
	}
	output.WriteString("PASS\n")
	return []byte(output.String())
}

func installProductionProvenanceCommands(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	// Only external provenance discovery is deterministic here; Git, stamp,
	// sealing, comparison, and report publication all execute production code.
	scripts := map[string]string{
		"go":     "#!/bin/sh\n[ \"$1\" = env ] && [ \"$2\" = -json ] || exit 2\nprintf '%s\\n' '{\"GOAMD64\":\"v1\",\"GOEXPERIMENT\":\"\",\"CGO_ENABLED\":\"1\",\"GOFLAGS\":\"\",\"GOTOOLCHAIN\":\"auto\"}'\n",
		"docker": "#!/bin/sh\ncase \"$1\" in\n  version) printf '27.0.0\\n' ;;\n  compose) printf '2.29.0\\n' ;;\n  *) exit 2 ;;\nesac\n",
	}
	for name, script := range scripts {
		filename := filepath.Join(directory, name)
		mustWriteFile(t, filename, []byte(script))
		if err := os.Chmod(filename, 0o755); err != nil {
			t.Fatalf("chmod fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func replaceProductionArtifact(t *testing.T, filename, old, replacement string) {
	t.Helper()
	contents := mustReadProductionFile(t, filename)
	changed := bytes.Replace(contents, []byte(old), []byte(replacement), 1)
	if bytes.Equal(changed, contents) {
		t.Fatalf("artifact %s does not contain %q", filename, old)
	}
	mustWriteFile(t, filename, changed)
}

func readProductionMetadata(t *testing.T, filename string) resultProvenance {
	t.Helper()
	var metadata resultProvenance
	if err := json.Unmarshal(mustReadProductionFile(t, filename), &metadata); err != nil {
		t.Fatalf("decode metadata %s: %v", filename, err)
	}
	return metadata
}

func mustReadProductionFile(t *testing.T, filename string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return contents
}

func setProductionModTime(t *testing.T, filename string, modified time.Time) {
	t.Helper()
	if err := os.Chtimes(filename, modified, modified); err != nil {
		t.Fatalf("set mtime for %s: %v", filename, err)
	}
}
