package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testBaseSHA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCandidateSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testGateSourceSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestRejectDuplicateJSONKeysRejectsExactAndCaseFoldedKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "top-level exact", raw: `{"run_id":"one","run_id":"two"}`},
		{name: "top-level case-folded", raw: `{"run_id":"one","RUN_ID":"two"}`},
		{name: "nested exact", raw: `{"runner":{"id":"one","id":"two"}}`},
		{name: "nested case-folded", raw: `{"runner":{"id":"one","ID":"two"}}`},
		{name: "array object", raw: `[{"name":"one","NAME":"two"}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := rejectDuplicateJSONKeys([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
				t.Fatalf("rejectDuplicateJSONKeys(%s) error = %v, want duplicate-key error", test.raw, err)
			}
		})
	}

	if err := rejectDuplicateJSONKeys([]byte(`{"runner":{"id":"one"},"RUNNER_ID":"two"}`)); err != nil {
		t.Fatalf("distinct keys were rejected: %v", err)
	}
}

func TestDecodeStrictJSONRejectsUnknownAndTrailingValues(t *testing.T) {
	type document struct {
		Name string `json:"name"`
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown field", raw: `{"name":"value","extra":true}`, want: "unknown field"},
		{name: "second value", raw: `{"name":"value"} {"name":"other"}`, want: "multiple JSON values"},
		{name: "trailing token", raw: `{"name":"value"} x`, want: "invalid character"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var target document
			err := decodeStrictJSON([]byte(test.raw), &target)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeStrictJSON(%s) error = %v, want substring %q", test.raw, err, test.want)
			}
		})
	}
}

func TestReadPolicyRejectsAmbiguousIncompleteAndTrailingJSON(t *testing.T) {
	valid := mustJSON(t, validTestPolicy())
	missing := jsonObjectWithout(t, valid, "profile")
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "case-folded duplicate",
			raw:  injectJSONObjectField(t, valid, `"PROFILE":"other"`),
			want: "duplicate JSON object key",
		},
		{
			name: "unknown field",
			raw:  injectJSONObjectField(t, valid, `"unexpected":true`),
			want: "unknown field",
		},
		{name: "missing required field", raw: missing, want: "profile must match"},
		{name: "second value", raw: append(append([]byte(nil), valid...), []byte("\n{}")...), want: "JSON has trailing data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "policy.json")
			mustWriteFile(t, filename, test.raw)
			_, _, err := readPolicy(filename)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readPolicy error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestReadProvenanceRejectsAmbiguousIncompleteAndTrailingJSON(t *testing.T) {
	valid := mustJSON(t, validTestProvenance("base", testBaseSHA, validTestRunner(), validTestConfig(), time.Now().UTC()))
	missing := jsonObjectWithout(t, valid, "result_sha256")
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "case-folded duplicate",
			raw:  injectJSONObjectField(t, valid, `"RUN_ID":"other"`),
			want: "duplicate JSON object key",
		},
		{
			name: "unknown field",
			raw:  injectJSONObjectField(t, valid, `"unexpected":true`),
			want: "unknown field",
		},
		{name: "missing required field", raw: missing, want: "result_sha256"},
		{name: "second value", raw: append(append([]byte(nil), valid...), []byte("\nnull")...), want: "JSON has trailing data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "metadata.json")
			mustWriteFile(t, filename, test.raw)
			_, err := readProvenance(filename)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readProvenance error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateProvenanceRejectsInvalidRequiredFields(t *testing.T) {
	valid := validTestProvenance("base", testBaseSHA, validTestRunner(), validTestConfig(), time.Now().UTC())
	tests := []struct {
		name   string
		mutate func(*resultProvenance)
		want   string
	}{
		{name: "schema", mutate: func(value *resultProvenance) { value.SchemaVersion++ }, want: "schema_version"},
		{name: "role", mutate: func(value *resultProvenance) { value.Role = "control" }, want: "invalid role"},
		{name: "run ID", mutate: func(value *resultProvenance) { value.RunID = "" }, want: "run ID"},
		{name: "short commit", mutate: func(value *resultProvenance) { value.CommitSHA = "deadbeef" }, want: "commit_sha"},
		{name: "gate source", mutate: func(value *resultProvenance) { value.GateSourceSHA = "deadbeef" }, want: "gate_source_sha"},
		{name: "result digest", mutate: func(value *resultProvenance) { value.ResultSHA256 = "ABC" }, want: "result_sha256"},
		{name: "non-canonical timestamp", mutate: func(value *resultProvenance) { value.StartedAt = "2026-08-11T08:00:00+00:00" }, want: "started_at"},
		{name: "runner", mutate: func(value *resultProvenance) { value.Runner.ID = "" }, want: "runner ID"},
		{name: "configuration", mutate: func(value *resultProvenance) { value.Benchmark.Count = 0 }, want: "benchmark count"},
		{name: "configuration gate source", mutate: func(value *resultProvenance) { value.Benchmark.GateSourceSHA = strings.Repeat("d", 40) }, want: "benchmark names"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := valid
			test.mutate(&metadata)
			err := validateProvenance(metadata)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateProvenance error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseUTCTimestampRequiresCanonicalUTC(t *testing.T) {
	valid := "2026-08-11T08:09:10.123456789Z"
	parsed, err := parseUTCTimestamp(valid)
	if err != nil {
		t.Fatalf("parseUTCTimestamp(valid) error = %v", err)
	}
	if parsed.Format(time.RFC3339Nano) != valid {
		t.Fatalf("parsed timestamp = %s, want %s", parsed.Format(time.RFC3339Nano), valid)
	}

	for _, value := range []string{
		"2026-08-11T08:09:10+00:00",
		"2026-08-11T16:09:10+08:00",
		"2026-08-11T08:09:10.000Z",
		"2026-08-11 08:09:10Z",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseUTCTimestamp(value); err == nil {
				t.Fatalf("parseUTCTimestamp(%q) succeeded, want canonical-UTC error", value)
			}
		})
	}
}

func TestValidateProvenancePairRejectsIdentityAndConfigurationMismatch(t *testing.T) {
	configured := validTestConfig()
	runner := validTestRunner()
	now := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	base, candidate := validTestPair(runner, configured, now)

	tests := []struct {
		name           string
		mutate         func(*resultProvenance, *resultProvenance)
		configured     benchmarkConfig
		requestedRunID string
		requestedID    string
		want           string
	}{
		{name: "roles", mutate: func(base, _ *resultProvenance) { base.Role = "candidate" }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "roles are"},
		{name: "commits", mutate: func(base, _ *resultProvenance) { base.CommitSHA = testCandidateSHA }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "commit mismatch"},
		{name: "run ID in metadata", mutate: func(_, candidate *resultProvenance) { candidate.RunID = "other-run" }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "run ID mismatch"},
		{name: "requested run ID", mutate: func(_, _ *resultProvenance) {}, configured: configured, requestedRunID: "other-run", requestedID: "runner-1", want: "run ID mismatch"},
		{name: "runner ID", mutate: func(_, candidate *resultProvenance) { candidate.Runner.ID = "other-runner" }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "runner ID mismatch"},
		{name: "requested runner ID", mutate: func(_, _ *resultProvenance) {}, configured: configured, requestedRunID: "run-1", requestedID: "other-runner", want: "runner ID mismatch"},
		{name: "runner provenance", mutate: func(_, candidate *resultProvenance) { candidate.Runner.KernelRelease = "different" }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "runner provenance differs"},
		{name: "candidate configuration", mutate: func(_, candidate *resultProvenance) { candidate.Benchmark.Count++ }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "benchmark configuration mismatch"},
		{name: "invalid base timestamp", mutate: func(base, _ *resultProvenance) { base.StartedAt = "not-a-time" }, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "base metadata: started_at"},
		{name: "apparatus digest", mutate: func(base, candidate *resultProvenance) {
			base.Benchmark.ApparatusSHA256 = strings.Repeat("e", 64)
			candidate.Benchmark.ApparatusSHA256 = strings.Repeat("e", 64)
		}, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "benchmark configuration mismatch"},
		{name: "gate source", mutate: func(base, candidate *resultProvenance) {
			base.GateSourceSHA = strings.Repeat("d", 40)
			base.Benchmark.GateSourceSHA = base.GateSourceSHA
			candidate.GateSourceSHA = base.GateSourceSHA
			candidate.Benchmark.GateSourceSHA = base.GateSourceSHA
		}, configured: configured, requestedRunID: "run-1", requestedID: "runner-1", want: "benchmark configuration mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseMetadata, candidateMetadata := base, candidate
			test.mutate(&baseMetadata, &candidateMetadata)
			err := validateProvenancePair(baseMetadata, candidateMetadata, testBaseSHA, testCandidateSHA, test.requestedRunID, test.requestedID, test.configured, now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateProvenancePair error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateProvenancePairRejectsRuntimeAndTimestampViolations(t *testing.T) {
	installFakeDocker(t)
	runner, err := currentRunner("runner-1", "Benchgate Test CPU")
	if err != nil {
		t.Skipf("current runner provenance is unavailable in this environment: %v", err)
	}
	configured := validTestConfig()
	now := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	base, candidate := validTestPair(runner, configured, now)
	if err := validateProvenancePair(base, candidate, testBaseSHA, testCandidateSHA, "run-1", "runner-1", configured, now); err != nil {
		t.Fatalf("valid provenance pair rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*resultProvenance, *resultProvenance)
		want   string
	}{
		{
			name: "runtime differs",
			mutate: func(base, candidate *resultProvenance) {
				base.Runner.GoVersion = "go0.invalid"
				candidate.Runner.GoVersion = "go0.invalid"
			},
			want: "comparison runtime differs",
		},
		{
			name: "stale base",
			mutate: func(base, _ *resultProvenance) {
				base.StartedAt = timestamp(now.Add(-maximumArtifactAge - time.Nanosecond))
			},
			want: "older than",
		},
		{
			name: "future stamp",
			mutate: func(base, _ *resultProvenance) {
				base.StampedAt = timestamp(now.Add(maximumFutureSkew + time.Nanosecond))
			},
			want: "in the future",
		},
		{
			name:   "base finish not after start",
			mutate: func(base, _ *resultProvenance) { base.FinishedAt = base.StartedAt },
			want:   "base run timestamps",
		},
		{
			name: "base stamp before finish",
			mutate: func(base, _ *resultProvenance) {
				finished, parseErr := parseUTCTimestamp(base.FinishedAt)
				if parseErr != nil {
					t.Fatalf("parse fixture timestamp: %v", parseErr)
				}
				base.StampedAt = timestamp(finished.Add(-time.Nanosecond))
			},
			want: "base run timestamps",
		},
		{
			name: "base stamp too late",
			mutate: func(base, _ *resultProvenance) {
				finished, parseErr := parseUTCTimestamp(base.FinishedAt)
				if parseErr != nil {
					t.Fatalf("parse fixture timestamp: %v", parseErr)
				}
				base.StampedAt = timestamp(finished.Add(maximumFutureSkew + time.Nanosecond))
			},
			want: "base run timestamps",
		},
		{
			name:   "candidate finish not after start",
			mutate: func(_, candidate *resultProvenance) { candidate.FinishedAt = candidate.StartedAt },
			want:   "candidate run timestamps",
		},
		{
			name: "overlap",
			mutate: func(base, candidate *resultProvenance) {
				started, parseErr := parseUTCTimestamp(candidate.StartedAt)
				if parseErr != nil {
					t.Fatalf("parse fixture timestamp: %v", parseErr)
				}
				base.FinishedAt = timestamp(started.Add(time.Nanosecond))
				base.StampedAt = base.FinishedAt
			},
			want: "runs overlap",
		},
		{
			name: "artifact span",
			mutate: func(base, candidate *resultProvenance) {
				base.StartedAt = timestamp(now.Add(-maximumArtifactAge))
				base.FinishedAt = timestamp(now.Add(-maximumArtifactAge + time.Minute))
				base.StampedAt = timestamp(now.Add(-maximumArtifactAge + 2*time.Minute))
				candidate.StartedAt = timestamp(now.Add(-time.Minute))
				candidate.FinishedAt = timestamp(now)
				candidate.StampedAt = timestamp(now.Add(time.Minute))
			},
			want: "artifact span exceeds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseMetadata, candidateMetadata := base, candidate
			test.mutate(&baseMetadata, &candidateMetadata)
			err := validateProvenancePair(baseMetadata, candidateMetadata, testBaseSHA, testCandidateSHA, "run-1", "runner-1", configured, now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateProvenancePair error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateEmbeddedProvenanceRejectsEveryMismatch(t *testing.T) {
	metadata := validTestProvenance("base", testBaseSHA, validTestRunner(), validTestConfig(), time.Now().UTC())
	embedded := embeddedProvenance(metadata)
	data := benchmarkData{Embedded: embedded}
	if err := validateEmbeddedProvenance(data, metadata); err != nil {
		t.Fatalf("matching embedded provenance rejected: %v", err)
	}
	if !artifactHasEmbeddedProvenance(data) {
		t.Fatal("artifactHasEmbeddedProvenance returned false for a sealed artifact")
	}

	for _, key := range embeddedProvenanceKeys {
		t.Run(key, func(t *testing.T) {
			changed := make(map[string]string, len(embedded))
			for name, value := range embedded {
				changed[name] = value
			}
			changed[key] += "-changed"
			err := validateEmbeddedProvenance(benchmarkData{Embedded: changed}, metadata)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("validateEmbeddedProvenance error = %v, want mismatch for %s", err, key)
			}
		})
	}

	if artifactHasEmbeddedProvenance(benchmarkData{Embedded: map[string]string{"bench-role": ""}}) {
		t.Fatal("artifactHasEmbeddedProvenance returned true for empty provenance values")
	}
}

func TestValidatePolicyRejectsEmptyDuplicateAndUnsortedSets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*benchmarkPolicy)
		want   string
	}{
		{name: "empty benchmark set", mutate: func(policy *benchmarkPolicy) { policy.BenchmarkNames = nil }, want: "benchmark_names must not be empty"},
		{name: "duplicate benchmark", mutate: func(policy *benchmarkPolicy) { policy.BenchmarkNames = []string{"Alpha", "Alpha"} }, want: "strictly sorted and unique"},
		{name: "unsorted benchmarks", mutate: func(policy *benchmarkPolicy) { policy.BenchmarkNames = []string{"Beta", "Alpha"} }, want: "strictly sorted and unique"},
		{name: "empty apparatus set", mutate: func(policy *benchmarkPolicy) { policy.ApparatusPaths = nil }, want: "apparatus_paths must not be empty"},
		{name: "duplicate apparatus", mutate: func(policy *benchmarkPolicy) { policy.ApparatusPaths = []string{"a.txt", "a.txt"} }, want: "strictly sorted and unique"},
		{name: "unsorted apparatus", mutate: func(policy *benchmarkPolicy) { policy.ApparatusPaths = []string{"b.txt", "a.txt"} }, want: "strictly sorted and unique"},
		{name: "repository parent apparatus", mutate: func(policy *benchmarkPolicy) { policy.ApparatusPaths = []string{".."} }, want: "clean repository-relative"},
		{name: "backslash apparatus", mutate: func(policy *benchmarkPolicy) { policy.ApparatusPaths = []string{`dir\file.go`} }, want: "clean repository-relative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := validTestPolicy()
			test.mutate(&policy)
			err := validatePolicy(policy)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validatePolicy error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateExpectedBenchmarkSetFailsClosed(t *testing.T) {
	key := benchmarkKey{Package: validTestPolicy().Package, Name: "Alpha"}
	data := benchmarkData{Samples: map[benchmarkKey]map[string][]float64{key: {}}}
	tests := []struct {
		name   string
		mutate func(*benchmarkData, *benchmarkPolicy)
		want   string
	}{
		{name: "missing expected", mutate: func(_ *benchmarkData, policy *benchmarkPolicy) { policy.BenchmarkNames = []string{"Alpha", "Beta"} }, want: "missing=[Beta]"},
		{name: "unexpected actual", mutate: func(_ *benchmarkData, policy *benchmarkPolicy) { policy.BenchmarkNames = []string{"Beta"} }, want: "unexpected=[Alpha]"},
		{name: "wrong package", mutate: func(data *benchmarkData, _ *benchmarkPolicy) {
			data.Samples = map[benchmarkKey]map[string][]float64{{Package: "wrong/package", Name: "Alpha"}: {}}
		}, want: "uses package"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyData := benchmarkData{Samples: make(map[benchmarkKey]map[string][]float64, len(data.Samples))}
			for sampleKey, metrics := range data.Samples {
				copyData.Samples[sampleKey] = metrics
			}
			policy := validTestPolicy()
			policy.BenchmarkNames = []string{"Alpha"}
			test.mutate(&copyData, &policy)
			err := validateExpectedBenchmarkSet(copyData, policy)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateExpectedBenchmarkSet error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestApparatusDigestChangesWithTrackedContent(t *testing.T) {
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	mustWriteFile(t, filepath.Join(repo, "a.txt"), []byte("alpha\n"))
	mustWriteFile(t, filepath.Join(repo, "b.txt"), []byte("beta\n"))
	gitCommand(t, repo, "add", "a.txt", "b.txt")
	gitCommand(t, repo, "commit", "-q", "-m", "apparatus")

	paths := []string{"a.txt", "b.txt"}
	before, err := apparatusDigest(repo, paths)
	if err != nil {
		t.Fatalf("apparatusDigest(before) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(repo, "a.txt"), []byte("alpha changed\n"))
	after, err := apparatusDigest(repo, paths)
	if err != nil {
		t.Fatalf("apparatusDigest(after) error = %v", err)
	}
	if before == after {
		t.Fatalf("apparatus digest did not change after tracked file mutation: %s", before)
	}

	mustWriteFile(t, filepath.Join(repo, "untracked.txt"), []byte("untracked\n"))
	if _, err := apparatusDigest(repo, []string{"untracked.txt"}); err == nil || !strings.Contains(err.Error(), "not a tracked file") {
		t.Fatalf("apparatusDigest(untracked) error = %v, want not-tracked error", err)
	}
}

func TestRunFingerprintRequiresInvocationToMatchPolicy(t *testing.T) {
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	mustWriteFile(t, filepath.Join(repo, "a.txt"), []byte("alpha\n"))
	mustWriteFile(t, filepath.Join(repo, "b.txt"), []byte("beta\n"))
	gitCommand(t, repo, "add", "a.txt", "b.txt")
	gitCommand(t, repo, "commit", "-q", "-m", "apparatus")

	directory := t.TempDir()
	policyPath := filepath.Join(directory, "policy.json")
	mustWriteFile(t, policyPath, mustJSON(t, validTestPolicy()))
	args := []string{
		"fingerprint", "-policy", policyPath, "-repo", repo,
		"-pattern", ".", "-benchtime", "1s", "-count", "10", "-timeout", "20m",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("matching fingerprint exited %d: %s", code, stderr.String())
	}
	if fields := strings.Fields(stdout.String()); len(fields) != 2 {
		t.Fatalf("fingerprint output = %q, want policy and apparatus digests", stdout.String())
	}

	args[len(args)-5] = "2s"
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "invocation does not match policy") {
		t.Fatalf("mismatched fingerprint exited %d, stderr %q", code, stderr.String())
	}
}

func TestRunCompareRejectsChangedApparatusBeforeReadingArtifacts(t *testing.T) {
	baseRepo, candidateRepo, baseSHA, candidateSHA := createApparatusChangeRepositories(t)
	directory := t.TempDir()
	policy := validTestPolicy()
	policy.ApparatusPaths = []string{"a.txt"}
	policyPath := filepath.Join(directory, "policy.json")
	mustWriteFile(t, policyPath, mustJSON(t, policy))
	args := []string{
		"compare", "-base-policy", policyPath, "-candidate-policy", policyPath,
		"-base-repo", baseRepo, "-candidate-repo", candidateRepo,
		"-base", filepath.Join(directory, "missing-base"), "-candidate", filepath.Join(directory, "missing-candidate"),
		"-base-metadata", filepath.Join(directory, "missing-base-metadata"), "-candidate-metadata", filepath.Join(directory, "missing-candidate-metadata"),
		"-base-sha", baseSHA, "-candidate-sha", candidateSHA,
		"-gate-source-sha", testGateSourceSHA, "-run-id", "run-1", "-runner-id", "runner-1", "-report", filepath.Join(directory, "report.json"),
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "measurement apparatus differs") {
		t.Fatalf("run(compare changed apparatus) = %d, stderr %q; want apparatus mismatch before artifact reads", code, stderr.String())
	}
}

func TestRunCompareRejectsPolicyMismatchBeforeReadingRepositories(t *testing.T) {
	directory := t.TempDir()
	basePolicy := validTestPolicy()
	candidatePolicy := validTestPolicy()
	candidatePolicy.BenchmarkNames = []string{"Alpha"}
	candidatePolicy.ApparatusPaths = []string{"b.txt"}
	basePolicyPath := filepath.Join(directory, "base-policy.json")
	candidatePolicyPath := filepath.Join(directory, "candidate-policy.json")
	mustWriteFile(t, basePolicyPath, mustJSON(t, basePolicy))
	mustWriteFile(t, candidatePolicyPath, mustJSON(t, candidatePolicy))
	args := []string{
		"compare", "-base-policy", basePolicyPath, "-candidate-policy", candidatePolicyPath,
		"-base-repo", filepath.Join(directory, "missing-base-repo"), "-candidate-repo", filepath.Join(directory, "missing-candidate-repo"),
		"-base", filepath.Join(directory, "missing-base"), "-candidate", filepath.Join(directory, "missing-candidate"),
		"-base-metadata", filepath.Join(directory, "missing-base-metadata"), "-candidate-metadata", filepath.Join(directory, "missing-candidate-metadata"),
		"-base-sha", testBaseSHA, "-candidate-sha", testCandidateSHA,
		"-gate-source-sha", testGateSourceSHA, "-run-id", "run-1", "-runner-id", "runner-1", "-report", filepath.Join(directory, "report.json"),
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "benchmark policies differ") {
		t.Fatalf("run(compare changed policy) = %d, stderr %q; want policy mismatch before repository reads", code, stderr.String())
	}
}

func TestGitCheckoutAndAncestryHelpers(t *testing.T) {
	repo, first, second := createTwoCommitRepository(t)

	resolved, err := resolveCommit(repo, "HEAD")
	if err != nil {
		t.Fatalf("resolveCommit(HEAD) error = %v", err)
	}
	if resolved != second || len(resolved) != 40 {
		t.Fatalf("resolveCommit(HEAD) = %q, want full SHA %q", resolved, second)
	}
	if err := validateSHA(first); err != nil {
		t.Fatalf("validateSHA(full SHA) error = %v", err)
	}
	for _, invalid := range []string{first[:12], strings.ToUpper(first), strings.Repeat("g", 40)} {
		if err := validateSHA(invalid); err == nil {
			t.Fatalf("validateSHA(%q) succeeded", invalid)
		}
	}
	if err := validateCheckout(repo, second); err != nil {
		t.Fatalf("validateCheckout(clean HEAD) error = %v", err)
	}
	if err := validateCheckout(repo, first); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("validateCheckout(wrong SHA) error = %v, want mismatch", err)
	}
	if err := requireAncestor(repo, first, second); err != nil {
		t.Fatalf("requireAncestor(first, second) error = %v", err)
	}
	if err := requireAncestor(repo, second, first); err == nil || !strings.Contains(err.Error(), "is not an ancestor") {
		t.Fatalf("requireAncestor(second, first) error = %v, want rejection", err)
	}

	mustWriteFile(t, filepath.Join(repo, "tracked.txt"), []byte("dirty\n"))
	if err := requireCleanCheckout(repo); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("requireCleanCheckout(dirty tracked file) error = %v, want dirty-checkout error", err)
	}
}

func TestRequireCleanCheckoutRejectsUntrackedFiles(t *testing.T) {
	repo, _, _ := createTwoCommitRepository(t)
	mustWriteFile(t, filepath.Join(repo, "untracked.txt"), []byte("untracked\n"))
	if err := requireCleanCheckout(repo); err == nil || !strings.Contains(err.Error(), "untracked.txt") {
		t.Fatalf("requireCleanCheckout(untracked file) error = %v, want file in diagnostic", err)
	}
}

func TestExclusiveWritersRefuseExistingOutputs(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "result.txt")
	if err := writeBytesExclusive(filename, []byte("first\n"), 0o640); err != nil {
		t.Fatalf("writeBytesExclusive(first) error = %v", err)
	}
	if err := writeBytesExclusive(filename, []byte("second\n"), 0o600); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("writeBytesExclusive(existing) error = %v, want refusal", err)
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read preserved output: %v", err)
	}
	if string(contents) != "first\n" {
		t.Fatalf("existing output was changed to %q", contents)
	}

	jsonPath := filepath.Join(directory, "report.json")
	mustWriteFile(t, jsonPath, []byte("preserve\n"))
	if err := writeJSONExclusive(jsonPath, map[string]string{"status": "new"}); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("writeJSONExclusive(existing) error = %v, want refusal", err)
	}
}

func TestRunRejectsInvalidCLIAndExistingOutputsBeforeExternalChecks(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "missing subcommand", wantCode: 2, wantErr: "usage:"},
		{name: "unknown subcommand", args: []string{"unknown"}, wantCode: 2, wantErr: "unknown subcommand"},
		{name: "incomplete compare", args: []string{"compare", "-run-id", "run-1"}, wantCode: 2, wantErr: "Usage of compare"},
		{name: "invalid stamp role", args: []string{
			"stamp", "-policy", "missing", "-repo", "missing", "-raw", "missing", "-runner-probe", "missing-probe", "-result", "result", "-metadata", "metadata",
			"-role", "control", "-commit", testBaseSHA, "-run-id", "run-1", "-runner-id", "runner-1",
			"-gate-source-sha", testGateSourceSHA,
			"-started-at", "2026-08-11T08:00:00Z", "-finished-at", "2026-08-11T08:01:00Z",
		}, wantCode: 2, wantErr: "role must be base or candidate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code != test.wantCode || !strings.Contains(stderr.String(), test.wantErr) {
				t.Fatalf("run(%v) = %d, stderr %q; want code %d and substring %q", test.args, code, stderr.String(), test.wantCode, test.wantErr)
			}
		})
	}

	t.Run("compare report exists", func(t *testing.T) {
		directory := t.TempDir()
		report := filepath.Join(directory, "report.json")
		mustWriteFile(t, report, []byte("preserve\n"))
		args := []string{
			"compare", "-base-policy", filepath.Join(directory, "missing-policy"), "-candidate-policy", filepath.Join(directory, "missing-policy"),
			"-base-repo", filepath.Join(directory, "missing-base-repo"), "-candidate-repo", filepath.Join(directory, "missing-candidate-repo"),
			"-base", filepath.Join(directory, "missing-base"), "-candidate", filepath.Join(directory, "missing-candidate"),
			"-base-metadata", filepath.Join(directory, "missing-base-metadata"), "-candidate-metadata", filepath.Join(directory, "missing-candidate-metadata"),
			"-base-sha", testBaseSHA, "-candidate-sha", testCandidateSHA,
			"-gate-source-sha", testGateSourceSHA, "-run-id", "run-1", "-runner-id", "runner-1", "-report", report,
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "refusing to replace existing path") {
			t.Fatalf("run(compare existing report) = %d, stderr %q; want refusal before repository checks", code, stderr.String())
		}
	})

	t.Run("stamp metadata exists", func(t *testing.T) {
		directory := t.TempDir()
		policyPath := filepath.Join(directory, "policy.json")
		mustWriteFile(t, policyPath, mustJSON(t, validTestPolicy()))
		metadataPath := filepath.Join(directory, "metadata.json")
		mustWriteFile(t, metadataPath, []byte("preserve\n"))
		args := []string{
			"stamp", "-policy", policyPath, "-repo", filepath.Join(directory, "missing-repo"),
			"-raw", filepath.Join(directory, "missing-raw"), "-runner-probe", filepath.Join(directory, "missing-probe"), "-result", filepath.Join(directory, "result.txt"), "-metadata", metadataPath,
			"-role", "base", "-commit", testBaseSHA, "-run-id", "run-1", "-runner-id", "runner-1",
			"-gate-source-sha", testGateSourceSHA,
			"-started-at", "2026-08-11T08:00:00Z", "-finished-at", "2026-08-11T08:01:00Z",
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "refusing to replace existing path") {
			t.Fatalf("run(stamp existing metadata) = %d, stderr %q; want refusal before repository checks", code, stderr.String())
		}
	})

	t.Run("stamp result exists", func(t *testing.T) {
		directory := t.TempDir()
		policyPath := filepath.Join(directory, "policy.json")
		mustWriteFile(t, policyPath, mustJSON(t, validTestPolicy()))
		resultPath := filepath.Join(directory, "result.txt")
		mustWriteFile(t, resultPath, []byte("preserve\n"))
		args := []string{
			"stamp", "-policy", policyPath, "-repo", filepath.Join(directory, "missing-repo"),
			"-raw", filepath.Join(directory, "missing-raw"), "-runner-probe", filepath.Join(directory, "missing-probe"), "-result", resultPath, "-metadata", filepath.Join(directory, "metadata.json"),
			"-role", "base", "-commit", testBaseSHA, "-run-id", "run-1", "-runner-id", "runner-1",
			"-gate-source-sha", testGateSourceSHA,
			"-started-at", "2026-08-11T08:00:00Z", "-finished-at", "2026-08-11T08:01:00Z",
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "refusing to replace existing path") {
			t.Fatalf("run(stamp existing result) = %d, stderr %q; want refusal before repository checks", code, stderr.String())
		}
	})
}

func TestRunClassifiesCommandLineErrorsAsUsage(t *testing.T) {
	stampArgs := func() []string {
		return []string{
			"stamp", "-policy", "policy.json", "-repo", "repo", "-raw", "raw.txt", "-runner-probe", "probe.json", "-result", "result.txt", "-metadata", "metadata.json",
			"-role", "base", "-commit", testBaseSHA, "-run-id", "run-1", "-runner-id", "runner-1",
			"-gate-source-sha", testGateSourceSHA,
			"-started-at", "2026-08-11T08:00:00Z", "-finished-at", "2026-08-11T08:01:00Z",
		}
	}
	compareArgs := func() []string {
		return []string{
			"compare", "-base-policy", "base-policy.json", "-candidate-policy", "candidate-policy.json",
			"-base-repo", "base-repo", "-candidate-repo", "candidate-repo", "-base", "base.txt", "-candidate", "candidate.txt",
			"-base-metadata", "base.json", "-candidate-metadata", "candidate.json",
			"-base-sha", testBaseSHA, "-candidate-sha", testCandidateSHA,
			"-gate-source-sha", testGateSourceSHA, "-run-id", "run-1", "-runner-id", "runner-1", "-report", "report.json",
		}
	}
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
		name    string
		args    []string
		wantErr string
	}{
		{name: "fingerprint unknown flag", args: []string{"fingerprint", "-unknown"}, wantErr: "flag provided but not defined"},
		{name: "stamp unknown flag", args: []string{"stamp", "-unknown"}, wantErr: "flag provided but not defined"},
		{name: "compare unknown flag", args: []string{"compare", "-unknown"}, wantErr: "flag provided but not defined"},
		{name: "fingerprint missing flag value", args: []string{"fingerprint", "-policy"}, wantErr: "flag needs an argument"},
		{name: "stamp missing flag value", args: []string{"stamp", "-commit"}, wantErr: "flag needs an argument"},
		{name: "compare missing flag value", args: []string{"compare", "-base-sha"}, wantErr: "flag needs an argument"},
		{name: "fingerprint invalid integer", args: []string{"fingerprint", "-count", "ten"}, wantErr: "invalid value"},
		{name: "stamp invalid SHA", args: replaceValue(stampArgs(), "-commit", "deadbeef"), wantErr: "commit SHA"},
		{name: "stamp invalid gate source SHA", args: replaceValue(stampArgs(), "-gate-source-sha", "deadbeef"), wantErr: "gate source SHA"},
		{name: "stamp invalid run ID", args: replaceValue(stampArgs(), "-run-id", "invalid ID"), wantErr: "run ID"},
		{name: "stamp invalid timestamp", args: replaceValue(stampArgs(), "-started-at", "not-a-time"), wantErr: "started-at"},
		{name: "stamp reversed timestamps", args: replaceValue(stampArgs(), "-finished-at", "2026-08-11T07:59:00Z"), wantErr: "finished-at must be after started-at"},
		{name: "compare invalid SHA", args: replaceValue(compareArgs(), "-candidate-sha", "deadbeef"), wantErr: "candidate SHA"},
		{name: "compare invalid gate source SHA", args: replaceValue(compareArgs(), "-gate-source-sha", "deadbeef"), wantErr: "gate source SHA"},
		{name: "compare invalid runner ID", args: replaceValue(compareArgs(), "-runner-id", "invalid ID"), wantErr: "runner ID"},
		{name: "compare identical SHAs", args: replaceValue(compareArgs(), "-candidate-sha", testBaseSHA), wantErr: "base and candidate SHAs must differ"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), test.wantErr) {
				t.Fatalf("run(%v) = %d, stderr %q; want usage exit and substring %q", test.args, code, stderr.String(), test.wantErr)
			}
		})
	}

	for _, command := range []string{"fingerprint", "probe-runner", "stamp", "compare", "verify-report"} {
		t.Run(command+" help", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{command, "-h"}, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%s -h) = %d, stderr %q; want success", command, code, stderr.String())
			}
		})
	}
}

func validTestPolicy() benchmarkPolicy {
	return benchmarkPolicy{
		SchemaVersion:  policySchemaVersion,
		Profile:        "test-profile",
		Package:        "example.com/bench",
		Pattern:        ".",
		Benchtime:      "1s",
		Count:          minimumSamples,
		Timeout:        "20m",
		Tags:           []string{"benchmark"},
		TestRun:        "^$",
		Benchmem:       true,
		BenchmarkNames: []string{"Alpha", "Beta"},
		ApparatusPaths: []string{"a.txt", "b.txt"},
	}
}

func validTestConfig() benchmarkConfig {
	return benchmarkConfig{
		Profile:         "test-profile",
		Package:         "example.com/bench",
		Pattern:         ".",
		Benchtime:       "1s",
		Count:           minimumSamples,
		Timeout:         "20m",
		PolicySHA256:    strings.Repeat("c", 64),
		ApparatusSHA256: strings.Repeat("d", 64),
		GateSourceSHA:   testGateSourceSHA,
	}
}

func validTestRunner() runnerProvenance {
	digest := strings.Repeat("1", 64)
	return runnerProvenance{
		ID:                   "runner-1",
		GoVersion:            "go1.26.5",
		GOOS:                 "linux",
		GOARCH:               "amd64",
		NumCPU:               8,
		GOMAXPROCS:           8,
		CPU:                  "Benchgate Test CPU",
		CPUFingerprint:       digest,
		MachineIDSHA256:      strings.Repeat("2", 64),
		BootIDSHA256:         strings.Repeat("3", 64),
		KernelRelease:        "6.8.0-test",
		CPUConstraintsSHA256: strings.Repeat("4", 64),
		GOAMD64:              "v1",
		GOEXPERIMENT:         "<empty>",
		CGOEnabled:           "1",
		GOFLAGS:              "<empty>",
		GODEBUG:              "<empty>",
		GOGC:                 "<empty>",
		GOMEMLIMIT:           "<empty>",
		GOTOOLCHAIN:          "auto",
		DockerVersion:        "27.0.0",
		DockerComposeVersion: "2.29.0",
	}
}

func validTestProvenance(role, commit string, runner runnerProvenance, configured benchmarkConfig, now time.Time) resultProvenance {
	started := now.Add(-30 * time.Minute)
	finished := now.Add(-29 * time.Minute)
	stamped := now.Add(-28 * time.Minute)
	if role == "candidate" {
		started = now.Add(-15 * time.Minute)
		finished = now.Add(-14 * time.Minute)
		stamped = now.Add(-13 * time.Minute)
	}
	return resultProvenance{
		SchemaVersion: metadataSchemaVersion,
		Role:          role,
		RunID:         "run-1",
		CommitSHA:     commit,
		GateSourceSHA: configured.GateSourceSHA,
		ResultSHA256:  strings.Repeat("f", 64),
		StartedAt:     timestamp(started),
		FinishedAt:    timestamp(finished),
		StampedAt:     timestamp(stamped),
		Runner:        runner,
		Benchmark:     configured,
	}
}

func validTestPair(runner runnerProvenance, configured benchmarkConfig, now time.Time) (resultProvenance, resultProvenance) {
	return validTestProvenance("base", testBaseSHA, runner, configured, now),
		validTestProvenance("candidate", testCandidateSHA, runner, configured, now)
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func installFakeDocker(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	filename := filepath.Join(directory, "docker")
	script := []byte("#!/bin/sh\ncase \"$1\" in\n  version) printf '27.0.0\\n' ;;\n  compose) printf '2.29.0\\n' ;;\n  *) exit 1 ;;\nesac\n")
	mustWriteFile(t, filename, script)
	if err := os.Chmod(filename, 0o755); err != nil {
		t.Fatalf("chmod fake docker: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func createTwoCommitRepository(t *testing.T) (string, string, string) {
	t.Helper()
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	filename := filepath.Join(repo, "tracked.txt")
	mustWriteFile(t, filename, []byte("first\n"))
	gitCommand(t, repo, "add", "tracked.txt")
	gitCommand(t, repo, "commit", "-q", "-m", "first")
	first := gitOutput(t, repo, "rev-parse", "HEAD")
	mustWriteFile(t, filename, []byte("second\n"))
	gitCommand(t, repo, "add", "tracked.txt")
	gitCommand(t, repo, "commit", "-q", "-m", "second")
	second := gitOutput(t, repo, "rev-parse", "HEAD")
	return repo, first, second
}

func createApparatusChangeRepositories(t *testing.T) (string, string, string, string) {
	t.Helper()
	source := t.TempDir()
	gitCommand(t, source, "init", "-q")
	configureGitIdentity(t, source)
	filename := filepath.Join(source, "a.txt")
	mustWriteFile(t, filename, []byte("base apparatus\n"))
	gitCommand(t, source, "add", "a.txt")
	gitCommand(t, source, "commit", "-q", "-m", "base apparatus")
	baseSHA := gitOutput(t, source, "rev-parse", "HEAD")
	mustWriteFile(t, filename, []byte("candidate apparatus\n"))
	gitCommand(t, source, "add", "a.txt")
	gitCommand(t, source, "commit", "-q", "-m", "candidate apparatus")
	candidateSHA := gitOutput(t, source, "rev-parse", "HEAD")

	clones := t.TempDir()
	baseRepo := filepath.Join(clones, "base")
	candidateRepo := filepath.Join(clones, "candidate")
	gitGlobalCommand(t, "clone", "-q", source, baseRepo)
	gitGlobalCommand(t, "clone", "-q", source, candidateRepo)
	gitCommand(t, baseRepo, "checkout", "-q", "--detach", baseSHA)
	return baseRepo, candidateRepo, baseSHA, candidateSHA
}

func configureGitIdentity(t *testing.T, repo string) {
	t.Helper()
	gitCommand(t, repo, "config", "user.name", "Benchgate Test")
	gitCommand(t, repo, "config", "user.email", "benchgate@example.invalid")
}

func gitCommand(t *testing.T, repo string, args ...string) {
	t.Helper()
	_ = gitOutput(t, repo, args...)
}

func gitGlobalCommand(t *testing.T, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
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

func mustWriteFile(t *testing.T, filename string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(filename, contents, 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return contents
}

func jsonObjectWithout(t *testing.T, raw []byte, key string) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("unmarshal fixture JSON: %v", err)
	}
	delete(object, key)
	return mustJSON(t, object)
}

func injectJSONObjectField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		t.Fatalf("fixture is not a JSON object: %s", raw)
	}
	result := make([]byte, 0, len(raw)+len(field)+1)
	result = append(result, '{')
	result = append(result, field...)
	result = append(result, ',')
	result = append(result, raw[1:]...)
	return result
}
