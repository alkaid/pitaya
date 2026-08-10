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

func TestRunVerifyReportAcceptsSelfConsistentOutcomesWithoutModifyingReport(t *testing.T) {
	for _, test := range []struct {
		name   string
		passed bool
		expect string
	}{
		{name: "pass", passed: true, expect: "pass"},
		{name: "regression", passed: false, expect: "regression"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "gate.json")
			original := mustJSON(t, validTestGateReport(test.passed))
			mustWriteFile(t, reportPath, original)

			var stdout, stderr bytes.Buffer
			if code := run(validVerifyReportArgs(reportPath, test.expect), &stdout, &stderr); code != 0 {
				t.Fatalf("verify-report exited %d, stderr %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "gate report verified") {
				t.Fatalf("verify-report stdout = %q", stdout.String())
			}
			after, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("verify-report modified the report")
			}
		})
	}
}

func TestRunVerifyReportRecomputesZeroAllocationInvariantFromSamples(t *testing.T) {
	report := validTestGateReport(true)
	allocations := &report.Comparisons[2]
	allocations.BaseSamples = repeatedFloat(0, minimumSamples)
	allocations.CandidateSamples = repeatedFloat(0, minimumSamples)
	allocations.CandidateSamples[len(allocations.CandidateSamples)-1] = 1
	allocations.BaseMedian = 0
	allocations.CandidateMedian = 0
	allocations.DeltaPercent = nil
	allocations.Regression = true
	report.Passed = false
	report.RegressionCount = 1

	reportPath := filepath.Join(t.TempDir(), "zero-alloc.json")
	mustWriteFile(t, reportPath, mustJSON(t, report))
	var stdout, stderr bytes.Buffer
	if code := run(validVerifyReportArgs(reportPath, "regression"), &stdout, &stderr); code != 0 {
		t.Fatalf("verify zero-allocation report exited %d, stderr %q", code, stderr.String())
	}

	allocations.Regression = false
	report.Passed = true
	report.RegressionCount = 0
	forgedPath := filepath.Join(t.TempDir(), "forged-zero-alloc.json")
	mustWriteFile(t, forgedPath, mustJSON(t, report))
	stdout.Reset()
	stderr.Reset()
	if code := run(validVerifyReportArgs(forgedPath, "pass"), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "samples require true") {
		t.Fatalf("forged zero-allocation report exited %d, stderr %q", code, stderr.String())
	}
}

func TestRunVerifyReportRejectsArtifactMismatch(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "gate.json")
	mustWriteFile(t, reportPath, mustJSON(t, validTestGateReport(true)))
	args := validVerifyReportArgs(reportPath, "pass")
	for index, argument := range args {
		if argument == "-base-result" {
			mustWriteFile(t, args[index+1], []byte("replaced base result\n"))
			break
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "base result SHA-256") {
		t.Fatalf("artifact mismatch exited %d, stderr %q", code, stderr.String())
	}
}

func TestValidateReportBenchmarkSetRequiresPolicyInventory(t *testing.T) {
	report := validTestGateReport(true)
	policy := validTestPolicy()
	policy.BenchmarkNames = []string{"Alpha", "Beta"}
	if err := validateReportBenchmarkSet(report.Comparisons, policy); err == nil || !strings.Contains(err.Error(), "missing=[Beta]") {
		t.Fatalf("benchmark inventory error = %v", err)
	}
}

func TestRunVerifyReportRejectsContentAndOutcomeMismatch(t *testing.T) {
	tests := []struct {
		name   string
		passed bool
		expect string
		mutate func(*gateReport)
		want   string
	}{
		{name: "schema", passed: true, expect: "pass", mutate: func(report *gateReport) { report.SchemaVersion-- }, want: "schema_version"},
		{name: "base identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.BaseSHA = strings.Repeat("d", 40) }, want: "gate report identity"},
		{name: "candidate identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.CandidateSHA = strings.Repeat("d", 40) }, want: "gate report identity"},
		{name: "run identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.RunID = "other-run" }, want: "gate report identity"},
		{name: "runner identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Runner.ID = "other-runner" }, want: "runner ID"},
		{name: "gate source identity", passed: true, expect: "pass", mutate: func(report *gateReport) {
			report.GateSourceSHA = strings.Repeat("d", 40)
			report.Benchmark.GateSourceSHA = report.GateSourceSHA
		}, want: "source SHA"},
		{name: "nested gate source mismatch", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Benchmark.GateSourceSHA = strings.Repeat("d", 40) }, want: "benchmark gate_source_sha"},
		{name: "artifact digest", passed: true, expect: "pass", mutate: func(report *gateReport) { report.BaseResultSHA256 = "bad" }, want: "base_result_sha256"},
		{name: "policy identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Benchmark.PolicySHA256 = strings.Repeat("e", 64) }, want: "policy SHA-256"},
		{name: "apparatus identity", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Benchmark.ApparatusSHA256 = strings.Repeat("e", 64) }, want: "apparatus SHA-256"},
		{name: "internal regression count", passed: false, expect: "regression", mutate: func(report *gateReport) { report.RegressionCount = 0 }, want: "regression_count"},
		{name: "passed contradicts comparisons", passed: false, expect: "regression", mutate: func(report *gateReport) { report.Passed = true }, want: "passed=true"},
		{name: "forged passing regression", passed: false, expect: "pass", mutate: func(report *gateReport) {
			report.Comparisons[0].Regression = false
			report.RegressionCount = 0
			report.Passed = true
		}, want: "samples require true"},
		{name: "forged median", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Comparisons[0].CandidateMedian = 100 }, want: "samples produce"},
		{name: "forged delta", passed: true, expect: "pass", mutate: func(report *gateReport) { *report.Comparisons[0].DeltaPercent = 4 }, want: "samples produce"},
		{name: "exit zero cannot pair with passed false", passed: false, expect: "pass", mutate: func(*gateReport) {}, want: "expected pass"},
		{name: "exit three cannot pair with passed true", passed: true, expect: "regression", mutate: func(*gateReport) {}, want: "expected regression"},
		{name: "comparison shape", passed: true, expect: "pass", mutate: func(report *gateReport) { report.Comparisons = report.Comparisons[:2] }, want: "multiple of"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validTestGateReport(test.passed)
			test.mutate(&report)
			reportPath := filepath.Join(t.TempDir(), "gate.json")
			mustWriteFile(t, reportPath, mustJSON(t, report))
			var stdout, stderr bytes.Buffer
			if code := run(validVerifyReportArgs(reportPath, test.expect), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("verify-report exited %d, stderr %q; want content failure containing %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestRunVerifyReportRejectsAmbiguousAndUnknownJSON(t *testing.T) {
	valid := mustJSON(t, validTestGateReport(true))
	withoutPassed := jsonObjectWithout(t, valid, "passed")
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "case-folded duplicate", raw: injectJSONObjectField(t, valid, `"RUN_ID":"other"`), want: "duplicate JSON object key"},
		{name: "case-folded alias", raw: injectJSONObjectField(t, withoutPassed, `"PASSED":true`), want: `omits required field "passed"`},
		{name: "unknown field", raw: injectJSONObjectField(t, valid, `"unexpected":true`), want: "unknown field"},
		{name: "missing outcome", raw: withoutPassed, want: `omits required field "passed"`},
		{name: "missing comparison boolean", raw: bytes.Replace(valid, []byte(`,"blocking":true`), nil, 1), want: `omits required field "blocking"`},
		{name: "second value", raw: append(append([]byte(nil), valid...), []byte("\n{}")...), want: "JSON has trailing data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "gate.json")
			mustWriteFile(t, reportPath, test.raw)
			var stdout, stderr bytes.Buffer
			if code := run(validVerifyReportArgs(reportPath, "pass"), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("verify-report exited %d, stderr %q; want strict JSON failure containing %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestRunVerifyReportClassifiesCLIUsage(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "gate.json")
	mustWriteFile(t, reportPath, mustJSON(t, validTestGateReport(true)))
	replaceValue := func(args []string, flagName, value string) []string {
		for index := range args {
			if args[index] == flagName {
				args[index+1] = value
				return args
			}
		}
		t.Fatalf("fixture flag %q not found", flagName)
		return nil
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing flags", args: []string{"verify-report", "-report", reportPath}, want: "Usage of verify-report"},
		{name: "unknown flag", args: []string{"verify-report", "-unknown"}, want: "flag provided but not defined"},
		{name: "invalid base SHA", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-base-sha", "deadbeef"), want: "base SHA"},
		{name: "invalid source SHA", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-gate-source-sha", "DEADBEEF"), want: "gate source SHA"},
		{name: "invalid policy digest", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-policy-sha256", "DEADBEEF"), want: "policy SHA-256"},
		{name: "invalid apparatus digest", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-apparatus-sha256", "DEADBEEF"), want: "apparatus SHA-256"},
		{name: "invalid run ID", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-run-id", "bad ID"), want: "run ID"},
		{name: "invalid expect", args: replaceValue(validVerifyReportArgs(reportPath, "pass"), "-expect", "failure"), want: "expect must be pass or regression"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("verify-report exited %d, stderr %q; want usage failure containing %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestRunProbeRunnerProducesStableStrictJSON(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production runner provenance requires Linux")
	}
	installFakeDocker(t)
	var firstOut, firstErr bytes.Buffer
	if code := run([]string{"probe-runner", "-runner-id", "runner-1"}, &firstOut, &firstErr); code != 0 {
		t.Fatalf("first probe exited %d, stderr %q", code, firstErr.String())
	}
	var secondOut, secondErr bytes.Buffer
	if code := run([]string{"probe-runner", "-runner-id", "runner-1"}, &secondOut, &secondErr); code != 0 {
		t.Fatalf("second probe exited %d, stderr %q", code, secondErr.String())
	}
	if !bytes.Equal(firstOut.Bytes(), secondOut.Bytes()) {
		t.Fatalf("runner probes differ:\nfirst: %s\nsecond: %s", firstOut.Bytes(), secondOut.Bytes())
	}
	var runner runnerProvenance
	if err := decodeStrictJSON(firstOut.Bytes(), &runner); err != nil {
		t.Fatalf("decode runner probe: %v", err)
	}
	if err := validateRunner(runner); err != nil {
		t.Fatalf("validate runner probe: %v", err)
	}
	if runner.ID != "runner-1" || runner.CPU != "<probe>" {
		t.Fatalf("runner probe identity = %q/%q", runner.ID, runner.CPU)
	}
}

func TestRunProbeRunnerRejectsInvalidCLI(t *testing.T) {
	for _, args := range [][]string{
		{"probe-runner"},
		{"probe-runner", "-runner-id", "invalid ID"},
		{"probe-runner", "-runner-id", "runner-1", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("run(%v) = %d, stderr %q; want usage exit", args, code, stderr.String())
		}
	}
}

func TestValidateRunnerProbeProjectsOnlyArtifactCPU(t *testing.T) {
	observed := validTestRunner()
	probe := observed
	probe.CPU = "<probe>"
	if err := validateRunnerProbe(probe, observed); err != nil {
		t.Fatalf("matching runner probe rejected: %v", err)
	}
	probe.CPUFingerprint = strings.Repeat("e", 64)
	if err := validateRunnerProbe(probe, observed); err == nil || !strings.Contains(err.Error(), "runner state differs") {
		t.Fatalf("CPU fingerprint drift error = %v", err)
	}
}

func validVerifyReportArgs(reportPath, expect string) []string {
	directory := filepath.Dir(reportPath)
	policyPath := filepath.Join(directory, "verify-policy.json")
	baseResultPath := filepath.Join(directory, "verify-base.txt")
	candidateResultPath := filepath.Join(directory, "verify-candidate.txt")
	baseMetadataPath := filepath.Join(directory, "verify-base.meta.json")
	candidateMetadataPath := filepath.Join(directory, "verify-candidate.meta.json")
	for path, contents := range map[string][]byte{
		policyPath:            validVerifyPolicyBytes(),
		baseResultPath:        verifyArtifactBytes("base result"),
		candidateResultPath:   verifyArtifactBytes("candidate result"),
		baseMetadataPath:      verifyArtifactBytes("base metadata"),
		candidateMetadataPath: verifyArtifactBytes("candidate metadata"),
	} {
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			panic(err)
		}
	}
	policyDigest := testDigest(validVerifyPolicyBytes())
	return []string{
		"verify-report", "-report", reportPath,
		"-policy", policyPath,
		"-base-result", baseResultPath, "-candidate-result", candidateResultPath,
		"-base-metadata", baseMetadataPath, "-candidate-metadata", candidateMetadataPath,
		"-base-sha", testBaseSHA, "-candidate-sha", testCandidateSHA,
		"-gate-source-sha", testGateSourceSHA, "-run-id", "run-1",
		"-policy-sha256", policyDigest,
		"-apparatus-sha256", strings.Repeat("d", 64),
		"-runner-id", "runner-1", "-expect", expect,
	}
}

func validTestGateReport(passed bool) gateReport {
	timeDelta, byteDelta, allocDelta := testReportDelta(100, 105), 0.0, 0.0
	comparisons := []metricComparison{
		{Benchmark: "example.com/bench:BenchmarkAlpha-8", Metric: "time/op", Blocking: true, BaseSamples: repeatedFloat(100, minimumSamples), CandidateSamples: repeatedFloat(105, minimumSamples), BaseMedian: 100, CandidateMedian: 105, DeltaPercent: &timeDelta},
		{Benchmark: "example.com/bench:BenchmarkAlpha-8", Metric: "B/op", BaseSamples: repeatedFloat(64, minimumSamples), CandidateSamples: repeatedFloat(64, minimumSamples), BaseMedian: 64, CandidateMedian: 64, DeltaPercent: &byteDelta},
		{Benchmark: "example.com/bench:BenchmarkAlpha-8", Metric: "allocs/op", Blocking: true, BaseSamples: repeatedFloat(1, minimumSamples), CandidateSamples: repeatedFloat(1, minimumSamples), BaseMedian: 1, CandidateMedian: 1, DeltaPercent: &allocDelta},
	}
	regressions := 0
	if !passed {
		regressionDelta := testReportDelta(100, 120)
		comparisons[0].CandidateSamples = repeatedFloat(120, minimumSamples)
		comparisons[0].CandidateMedian = 120
		comparisons[0].DeltaPercent = &regressionDelta
		comparisons[0].Regression = true
		regressions = 1
	}
	policy := validTestPolicy()
	policy.BenchmarkNames = []string{"Alpha"}
	configured := configFromPolicy(policy, testDigest(validVerifyPolicyBytes()), strings.Repeat("d", 64), testGateSourceSHA)
	return gateReport{
		SchemaVersion:           reportSchemaVersion,
		RunID:                   "run-1",
		BaseSHA:                 testBaseSHA,
		CandidateSHA:            testCandidateSHA,
		GateSourceSHA:           testGateSourceSHA,
		BaseResultSHA256:        testDigest(verifyArtifactBytes("base result")),
		CandidateResultSHA256:   testDigest(verifyArtifactBytes("candidate result")),
		BaseMetadataSHA256:      testDigest(verifyArtifactBytes("base metadata")),
		CandidateMetadataSHA256: testDigest(verifyArtifactBytes("candidate metadata")),
		GeneratedAt:             time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		MinimumSamples:          minimumSamples,
		RegressionPercent:       regressionPercent,
		Runner:                  validTestRunner(),
		Benchmark:               configured,
		Passed:                  passed,
		RegressionCount:         regressions,
		Comparisons:             comparisons,
	}
}

func validVerifyPolicyBytes() []byte {
	policy := validTestPolicy()
	policy.BenchmarkNames = []string{"Alpha"}
	contents, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	return append(contents, '\n')
}

func verifyArtifactBytes(name string) []byte {
	return []byte("benchgate verify fixture: " + name + "\n")
}

func testDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return fmt.Sprintf("%x", digest[:])
}

func testReportDelta(base, candidate float64) float64 {
	return (candidate/base - 1) * 100
}

func TestGateReportJSONCarriesRequiredSourceIdentity(t *testing.T) {
	contents := mustJSON(t, validTestGateReport(true))
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document["gate_source_sha"]; !ok {
		t.Fatal("gate report omits gate_source_sha")
	}
}

func TestConfigFromPolicyCarriesRequiredSourceIdentity(t *testing.T) {
	configured := configFromPolicy(validTestPolicy(), strings.Repeat("a", 64), strings.Repeat("b", 64), testGateSourceSHA)
	if configured.GateSourceSHA != testGateSourceSHA {
		t.Fatalf("config gate source SHA = %q, want %q", configured.GateSourceSHA, testGateSourceSHA)
	}
	if err := validateBenchmarkConfig(configured); err != nil {
		t.Fatalf("validate benchmark config: %v", err)
	}
}
