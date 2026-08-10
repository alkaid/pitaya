package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
)

const maxBenchmarkBytes = 64 << 20

var requiredMetrics = []string{"sec/op", "B/op", "allocs/op"}

type benchmarkEnvironment struct {
	GOOS   string
	GOARCH string
	CPU    string
}

type benchmarkKey struct {
	Package string
	Name    string
}

func (key benchmarkKey) String() string {
	return key.Package + ":Benchmark" + key.Name
}

type benchmarkData struct {
	Environment benchmarkEnvironment
	Embedded    map[string]string
	Samples     map[benchmarkKey]map[string][]float64
}

type benchmarkArtifact struct {
	Data    benchmarkData
	Digest  string
	ModTime time.Time
}

type metricComparison struct {
	Benchmark string `json:"benchmark"`
	Metric    string `json:"metric"`
	Blocking  bool   `json:"blocking"`
	// Samples let verify-report recompute medians, the inclusive threshold, and
	// the all-zero allocation invariant instead of trusting summary booleans.
	BaseSamples      []float64 `json:"base_samples"`
	CandidateSamples []float64 `json:"candidate_samples"`
	BaseMedian       float64   `json:"base_median"`
	CandidateMedian  float64   `json:"candidate_median"`
	DeltaPercent     *float64  `json:"delta_percent,omitempty"`
	Regression       bool      `json:"regression"`
}

func loadBenchmarkArtifact(filename string) (benchmarkArtifact, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return benchmarkArtifact{}, err
	}
	if !info.Mode().IsRegular() {
		return benchmarkArtifact{}, errors.New("not a regular file")
	}

	file, err := os.Open(filename)
	if err != nil {
		return benchmarkArtifact{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return benchmarkArtifact{}, err
	}
	if !opened.Mode().IsRegular() {
		return benchmarkArtifact{}, errors.New("opened artifact is not a regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBenchmarkBytes+1))
	if err != nil {
		return benchmarkArtifact{}, err
	}
	if len(contents) > maxBenchmarkBytes {
		return benchmarkArtifact{}, fmt.Errorf("artifact exceeds %d bytes", maxBenchmarkBytes)
	}
	if err := validateBenchmarkFraming(contents); err != nil {
		return benchmarkArtifact{}, err
	}

	data, err := parseBenchmarkData(bytes.NewReader(contents), filename)
	if err != nil {
		return benchmarkArtifact{}, err
	}
	digest := sha256.Sum256(contents)
	return benchmarkArtifact{Data: data, Digest: fmt.Sprintf("%x", digest[:]), ModTime: opened.ModTime().UTC()}, nil
}

func validateBenchmarkFraming(contents []byte) error {
	if !utf8.Valid(contents) {
		return errors.New("benchmark artifact is not valid UTF-8")
	}
	allowed := map[string]bool{"goos": true, "goarch": true, "pkg": true, "cpu": true}
	for _, key := range embeddedProvenanceKeys {
		allowed[key] = true
	}
	// benchfmt intentionally tolerates non-result text, including one-field
	// Benchmark start markers. A blocking gate requires every such line to be an
	// official result and exactly one terminal PASS, or it could report false green.
	seen := make(map[string]int)
	benchmarkLines := 0
	passLines := 0
	passed := false
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if passed {
			return fmt.Errorf("non-empty content follows PASS: %q", line)
		}
		if line == "PASS" {
			passLines++
			passed = true
			continue
		}
		if strings.HasPrefix(line, "Benchmark") {
			if err := validateBenchmarkResultLine(line); err != nil {
				return fmt.Errorf("invalid benchmark result line %q: %w", line, err)
			}
			benchmarkLines++
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !allowed[key] || value == "" || (value[0] != ' ' && value[0] != '\t') || strings.TrimSpace(value) == "" {
			return fmt.Errorf("unexpected benchmark output line %q", line)
		}
		seen[key]++
		if seen[key] != 1 {
			return fmt.Errorf("benchmark config %q appears more than once", key)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if passLines != 1 || !passed {
		return fmt.Errorf("benchmark artifact has %d PASS terminators, want exactly one", passLines)
	}
	if benchmarkLines == 0 {
		return errors.New("benchmark artifact has no result lines")
	}
	for _, key := range []string{"goos", "goarch", "pkg", "cpu"} {
		if seen[key] != 1 {
			return fmt.Errorf("benchmark config %q appears %d times, want exactly once", key, seen[key])
		}
	}
	embeddedCount := 0
	for _, key := range embeddedProvenanceKeys {
		embeddedCount += seen[key]
	}
	if embeddedCount != 0 && embeddedCount != len(embeddedProvenanceKeys) {
		return fmt.Errorf("benchmark artifact has %d/%d embedded provenance fields", embeddedCount, len(embeddedProvenanceKeys))
	}
	return nil
}

func validateBenchmarkResultLine(line string) error {
	reader := benchfmt.NewReader(strings.NewReader(line+"\n"), "benchmark-output")
	results := 0
	for reader.Scan() {
		switch record := reader.Result().(type) {
		case *benchfmt.Result:
			results++
		case *benchfmt.SyntaxError:
			return record
		default:
			return fmt.Errorf("unexpected benchmark record %T", record)
		}
	}
	if err := reader.Err(); err != nil {
		return err
	}
	if results != 1 {
		return fmt.Errorf("parsed as %d results, want exactly one", results)
	}
	return nil
}

func sealBenchmarkResult(rawPath, resultPath string, embedded map[string]string) error {
	raw, err := readRegularFile(rawPath, maxBenchmarkBytes)
	if err != nil {
		return err
	}
	if err := validateBenchmarkFraming(raw); err != nil {
		return err
	}
	// Embed provenance in the benchfmt stream as well as the JSON sidecar so a
	// result cannot be detached from its identity and silently paired elsewhere.
	var output bytes.Buffer
	for _, key := range embeddedProvenanceKeys {
		value := embedded[key]
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("embedded provenance %s is empty or multiline", key)
		}
		fmt.Fprintf(&output, "%s: %s\n", key, value)
	}
	output.WriteByte('\n')
	output.Write(raw)
	if output.Len() > maxBenchmarkBytes {
		return fmt.Errorf("sealed artifact exceeds %d bytes", maxBenchmarkBytes)
	}
	return writeBytesExclusive(resultPath, output.Bytes(), 0o644)
}

func parseBenchmarkData(input io.Reader, filename string) (benchmarkData, error) {
	data := benchmarkData{Samples: make(map[benchmarkKey]map[string][]float64)}
	reader := benchfmt.NewReader(input, filename)
	results := 0
	for reader.Scan() {
		switch record := reader.Result().(type) {
		case *benchfmt.SyntaxError:
			return benchmarkData{}, record
		case *benchfmt.Result:
			results++
			if err := addBenchmarkResult(&data, record); err != nil {
				file, line := record.Pos()
				return benchmarkData{}, fmt.Errorf("%s:%d: %w", file, line, err)
			}
		default:
			return benchmarkData{}, fmt.Errorf("%s: unexpected benchmark record %T", filename, record)
		}
	}
	if err := reader.Err(); err != nil {
		return benchmarkData{}, err
	}
	if results == 0 {
		return benchmarkData{}, errors.New("no benchmark results")
	}
	return data, nil
}

func addBenchmarkResult(data *benchmarkData, result *benchfmt.Result) error {
	if result.Iters <= 0 {
		return fmt.Errorf("benchmark %q has non-positive iteration count %d", result.Name, result.Iters)
	}
	pkg := result.GetConfig("pkg")
	goos := result.GetConfig("goos")
	goarch := result.GetConfig("goarch")
	cpu := result.GetConfig("cpu")
	for key, value := range map[string]string{"pkg": pkg, "goos": goos, "goarch": goarch, "cpu": cpu} {
		if value == "" {
			return fmt.Errorf("benchmark %q is missing %s configuration", result.Name, key)
		}
	}
	if len(result.Name) == 0 {
		return errors.New("benchmark name is empty")
	}
	if err := mergeEnvironment(&data.Environment, benchmarkEnvironment{GOOS: goos, GOARCH: goarch, CPU: cpu}); err != nil {
		return fmt.Errorf("benchmark %q: %w", result.Name, err)
	}
	embedded := make(map[string]string, len(embeddedProvenanceKeys))
	for _, key := range embeddedProvenanceKeys {
		embedded[key] = result.GetConfig(key)
	}
	if data.Embedded == nil {
		data.Embedded = embedded
	} else {
		for _, key := range embeddedProvenanceKeys {
			if data.Embedded[key] != embedded[key] {
				return fmt.Errorf("benchmark %q changes embedded %s from %q to %q", result.Name, key, data.Embedded[key], embedded[key])
			}
		}
	}

	values := make(map[string]float64, len(result.Values))
	for _, value := range result.Values {
		if _, duplicate := values[value.Unit]; duplicate {
			return fmt.Errorf("benchmark %q repeats metric %q", result.Name, value.Unit)
		}
		if math.IsNaN(value.Value) || math.IsInf(value.Value, 0) {
			return fmt.Errorf("benchmark %q metric %q is non-finite", result.Name, value.Unit)
		}
		if value.Value < 0 {
			return fmt.Errorf("benchmark %q metric %q is negative", result.Name, value.Unit)
		}
		values[value.Unit] = value.Value
	}
	if value, ok := values["sec/op"]; !ok {
		return fmt.Errorf("benchmark %q is missing required metric time/op", result.Name)
	} else if value == 0 {
		return fmt.Errorf("benchmark %q has zero time/op", result.Name)
	}
	if _, ok := values["allocs/op"]; !ok {
		return fmt.Errorf("benchmark %q is missing required metric allocs/op", result.Name)
	}
	if _, ok := values["B/op"]; !ok {
		return fmt.Errorf("benchmark %q is missing required informational metric B/op", result.Name)
	}

	key := benchmarkKey{Package: pkg, Name: result.Name.String()}
	metrics := data.Samples[key]
	if metrics == nil {
		metrics = make(map[string][]float64, len(requiredMetrics))
		data.Samples[key] = metrics
	}
	for _, metric := range requiredMetrics {
		metrics[metric] = append(metrics[metric], values[metric])
	}
	return nil
}

func mergeEnvironment(current *benchmarkEnvironment, next benchmarkEnvironment) error {
	if current.GOOS == "" {
		*current = next
		return nil
	}
	if *current != next {
		return fmt.Errorf("benchmark environment changed from %s/%s %q to %s/%s %q",
			current.GOOS, current.GOARCH, current.CPU, next.GOOS, next.GOARCH, next.CPU)
	}
	return nil
}

func validateSampleCounts(data benchmarkData, count int) error {
	for _, key := range sortedBenchmarkKeys(data.Samples) {
		metrics := data.Samples[key]
		for _, metric := range requiredMetrics {
			values, ok := metrics[metric]
			if !ok {
				return fmt.Errorf("%s is missing required metric %s", key, displayMetric(metric))
			}
			if len(values) != count {
				return fmt.Errorf("%s %s has %d samples, want exactly %d", key, displayMetric(metric), len(values), count)
			}
		}
	}
	return nil
}

func validateExpectedBenchmarkSet(data benchmarkData, policy benchmarkPolicy) error {
	actual := make(map[string]struct{}, len(data.Samples))
	for key := range data.Samples {
		if key.Package != policy.Package {
			return fmt.Errorf("benchmark %s uses package %q, want %q", key, key.Package, policy.Package)
		}
		if key.Name == "" || len(key.Name) > 512 || !utf8.ValidString(key.Name) || strings.IndexFunc(key.Name, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
			return fmt.Errorf("benchmark name %q is empty, too long, or contains control characters", key.Name)
		}
		baseName := string(benchfmt.Name([]byte(key.Name)).Base())
		if _, duplicate := actual[baseName]; duplicate {
			return fmt.Errorf("multiple benchmark identities normalize to %q", baseName)
		}
		actual[baseName] = struct{}{}
	}
	expected := make(map[string]struct{}, len(policy.BenchmarkNames))
	for _, name := range policy.BenchmarkNames {
		expected[name] = struct{}{}
	}
	var expectedOnly, actualOnly []string
	for name := range expected {
		if _, ok := actual[name]; !ok {
			expectedOnly = append(expectedOnly, name)
		}
	}
	for name := range actual {
		if _, ok := expected[name]; !ok {
			actualOnly = append(actualOnly, name)
		}
	}
	sort.Strings(expectedOnly)
	sort.Strings(actualOnly)
	if len(expectedOnly) > 0 || len(actualOnly) > 0 {
		return fmt.Errorf("benchmark set does not match policy: missing=%v unexpected=%v", expectedOnly, actualOnly)
	}
	return nil
}

func compareBenchmarkData(base, candidate benchmarkData, count int, thresholdPercent float64) ([]metricComparison, error) {
	if base.Environment != candidate.Environment {
		return nil, fmt.Errorf("raw benchmark environments differ: base %s/%s %q, candidate %s/%s %q",
			base.Environment.GOOS, base.Environment.GOARCH, base.Environment.CPU,
			candidate.Environment.GOOS, candidate.Environment.GOARCH, candidate.Environment.CPU)
	}
	if err := validateSampleCounts(base, count); err != nil {
		return nil, fmt.Errorf("base samples: %w", err)
	}
	if err := validateSampleCounts(candidate, count); err != nil {
		return nil, fmt.Errorf("candidate samples: %w", err)
	}
	baseKeys := sortedBenchmarkKeys(base.Samples)
	if missingBase, missingCandidate := benchmarkSetDifference(base.Samples, candidate.Samples); len(missingBase) > 0 || len(missingCandidate) > 0 {
		return nil, fmt.Errorf("benchmark sets differ: base-only=%v candidate-only=%v", missingBase, missingCandidate)
	}

	comparisons := make([]metricComparison, 0, len(baseKeys)*len(requiredMetrics))
	for _, key := range baseKeys {
		for _, metric := range requiredMetrics {
			baseMedian := median(base.Samples[key][metric])
			candidateMedian := median(candidate.Samples[key][metric])
			comparison := metricComparison{
				Benchmark:        key.String(),
				Metric:           displayMetric(metric),
				Blocking:         metric != "B/op",
				BaseSamples:      append([]float64(nil), base.Samples[key][metric]...),
				CandidateSamples: append([]float64(nil), candidate.Samples[key][metric]...),
				BaseMedian:       baseMedian,
				CandidateMedian:  candidateMedian,
			}
			// B/op remains required evidence but is informational. For allocs/op,
			// an all-zero baseline is a stronger invariant than a median ratio.
			if metric == "allocs/op" && allZero(base.Samples[key][metric]) {
				comparison.Regression = anyPositive(candidate.Samples[key][metric])
			} else if baseMedian == 0 {
				if metric == "allocs/op" {
					return nil, fmt.Errorf("%s allocs/op base median is zero but the base samples are not all zero", key)
				}
			} else {
				delta := (candidateMedian/baseMedian - 1) * 100
				if !math.IsNaN(delta) && !math.IsInf(delta, 0) {
					comparison.DeltaPercent = &delta
				}
				if comparison.Blocking {
					comparison.Regression = exceedsRegressionThreshold(baseMedian, candidateMedian, thresholdPercent)
				}
			}
			comparisons = append(comparisons, comparison)
		}
	}
	return comparisons, nil
}

func allZero(values []float64) bool {
	for _, value := range values {
		if value != 0 {
			return false
		}
	}
	return true
}

func anyPositive(values []float64) bool {
	for _, value := range values {
		if value > 0 {
			return true
		}
	}
	return false
}

func exceedsRegressionThreshold(base, candidate, thresholdPercent float64) bool {
	// benchfmt normalizes source units into float64 values. Permit one ULP above
	// the computed inclusive boundary so 100 ns/op -> 110 ns/op is not rejected
	// solely because the two decimal inputs rounded in different directions.
	allowed := base * (1 + thresholdPercent/100)
	return candidate > math.Nextafter(allowed, math.Inf(1))
}

func median(values []float64) float64 {
	sampleValues := append([]float64(nil), values...)
	sample := benchmath.NewSample(sampleValues, &benchmath.DefaultThresholds)
	return benchmath.AssumeNothing.Summary(sample, 0.95).Center
}

func sortedBenchmarkKeys(samples map[benchmarkKey]map[string][]float64) []benchmarkKey {
	keys := make([]benchmarkKey, 0, len(samples))
	for key := range samples {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Package != keys[j].Package {
			return keys[i].Package < keys[j].Package
		}
		return keys[i].Name < keys[j].Name
	})
	return keys
}

func benchmarkSetDifference(base, candidate map[benchmarkKey]map[string][]float64) ([]string, []string) {
	var baseOnly, candidateOnly []string
	for key := range base {
		if _, ok := candidate[key]; !ok {
			baseOnly = append(baseOnly, key.String())
		}
	}
	for key := range candidate {
		if _, ok := base[key]; !ok {
			candidateOnly = append(candidateOnly, key.String())
		}
	}
	sort.Strings(baseOnly)
	sort.Strings(candidateOnly)
	return baseOnly, candidateOnly
}

func displayMetric(metric string) string {
	if metric == "sec/op" {
		return "time/op"
	}
	return metric
}

func comparisonRegressions(comparisons []metricComparison) int {
	regressions := 0
	for _, comparison := range comparisons {
		if comparison.Regression {
			regressions++
		}
	}
	return regressions
}

func formatDelta(comparison metricComparison) string {
	if comparison.DeltaPercent != nil {
		return fmt.Sprintf("%+.2f%%", *comparison.DeltaPercent)
	}
	if !comparison.Regression && comparison.CandidateMedian == 0 {
		return "0.00%"
	}
	return "+Inf"
}

func formatMedian(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.9f", value), "0"), ".")
}
