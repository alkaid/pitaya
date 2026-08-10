package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const testBenchmarkPackage = "example.com/bench"

func TestParseBenchmarkDataNormalizesOfficialUnits(t *testing.T) {
	data := parseTestBenchmarkArtifact(t, "BenchmarkNormalize-8 1 250 ns/op 2 MB/op 3 allocs/op")
	key := benchmarkKey{Package: testBenchmarkPackage, Name: "Normalize-8"}
	metrics, ok := data.Samples[key]
	if !ok {
		t.Fatalf("missing parsed benchmark %s", key)
	}
	timeFactor := 1e-9
	if got, want := metrics["sec/op"], []float64{250 * timeFactor}; !equalFloats(got, want) {
		t.Fatalf("time samples = %v, want %v", got, want)
	}
	if got, want := metrics["B/op"], []float64{2e6}; !equalFloats(got, want) {
		t.Fatalf("byte samples = %v, want %v", got, want)
	}
	if got, want := metrics["allocs/op"], []float64{3}; !equalFloats(got, want) {
		t.Fatalf("allocation samples = %v, want %v", got, want)
	}
}

func TestParseBenchmarkDataRejectsInvalidRequiredMetrics(t *testing.T) {
	tests := []struct {
		name         string
		measurements string
		want         string
	}{
		{name: "missing time", measurements: "8 B/op 1 allocs/op", want: "missing required metric time/op"},
		{name: "missing bytes", measurements: "10 ns/op 1 allocs/op", want: "missing required informational metric B/op"},
		{name: "missing allocations", measurements: "10 ns/op 8 B/op", want: "missing required metric allocs/op"},
		{name: "duplicate canonical time", measurements: "10 ns/op 0.00000001 sec/op 8 B/op 1 allocs/op", want: "repeats metric \"sec/op\""},
		{name: "NaN time", measurements: "NaN ns/op 8 B/op 1 allocs/op", want: "metric \"sec/op\" is non-finite"},
		{name: "positive infinity time", measurements: "+Inf ns/op 8 B/op 1 allocs/op", want: "is non-finite"},
		{name: "negative time", measurements: "-1 ns/op 8 B/op 1 allocs/op", want: "metric \"sec/op\" is negative"},
		{name: "zero time", measurements: "0 sec/op 8 B/op 1 allocs/op", want: "has zero time/op"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := testBenchmarkArtifact("BenchmarkInvalid-8 1 " + test.measurements)
			if err := validateBenchmarkFraming([]byte(artifact)); err != nil {
				t.Fatalf("test artifact framing is invalid: %v", err)
			}
			_, err := parseBenchmarkData(strings.NewReader(artifact), "invalid.txt")
			requireErrorContains(t, err, test.want)
		})
	}
}

func TestValidateBenchmarkFramingRejectsNonBenchmarkOutput(t *testing.T) {
	header := "goos: linux\ngoarch: amd64\npkg: " + testBenchmarkPackage + "\ncpu: Test CPU\n\n"
	result := "BenchmarkStrict-8 1 10 ns/op 8 B/op 1 allocs/op\n"
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "FAIL", body: header + result + "FAIL\n", want: "unexpected benchmark output line \"FAIL\""},
		{name: "panic", body: header + result + "panic: benchmark crashed\nFAIL\n", want: "unexpected benchmark output line \"panic: benchmark crashed\""},
		{name: "unknown line", body: header + result + "ordinary log output\nPASS\n", want: "unexpected benchmark output line \"ordinary log output\""},
		{name: "single-field benchmark start", body: header + "BenchmarkUnexpected\n" + result + "PASS\n", want: "BenchmarkUnexpected"},
		{name: "malformed benchmark result", body: header + "BenchmarkUnexpected-8 not-an-int 10 ns/op\n" + result + "PASS\n", want: "BenchmarkUnexpected-8"},
		{name: "missing PASS", body: header + result, want: "0 PASS terminators"},
		{name: "content after PASS", body: header + result + "PASS\nextra\n", want: "non-empty content follows PASS"},
		{name: "extra config", body: header + "branch: main\n" + result + "PASS\n", want: "unexpected benchmark output line \"branch: main\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireErrorContains(t, validateBenchmarkFraming([]byte(test.body)), test.want)
		})
	}
}

func TestValidateExpectedBenchmarkSetRequiresExactPolicySet(t *testing.T) {
	data := testBenchmarkData("Alpha-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))
	addTestBenchmark(&data, "Beta/size=1-8", repeatedFloat(20, 10), repeatedFloat(2, 10), repeatedFloat(1, 10))
	policy := benchmarkPolicy{
		Package:        testBenchmarkPackage,
		BenchmarkNames: []string{"Alpha", "Beta"},
	}
	if err := validateExpectedBenchmarkSet(data, policy); err != nil {
		t.Fatalf("exact policy benchmark set failed: %v", err)
	}

	t.Run("missing benchmark", func(t *testing.T) {
		missing := testBenchmarkData("Alpha-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))
		requireErrorContains(t, validateExpectedBenchmarkSet(missing, policy), "missing=[Beta] unexpected=[]")
	})
	t.Run("extra benchmark", func(t *testing.T) {
		extra := cloneBenchmarkData(data)
		addTestBenchmark(&extra, "Gamma-8", repeatedFloat(30, 10), repeatedFloat(3, 10), repeatedFloat(1, 10))
		requireErrorContains(t, validateExpectedBenchmarkSet(extra, policy), "missing=[] unexpected=[Gamma]")
	})
	t.Run("duplicate canonical benchmark", func(t *testing.T) {
		duplicate := cloneBenchmarkData(data)
		addTestBenchmark(&duplicate, "Alpha-16", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))
		requireErrorContains(t, validateExpectedBenchmarkSet(duplicate, policy), "multiple benchmark identities normalize to \"Alpha\"")
	})
}

func TestValidateSampleCountsRequiresExactlyTen(t *testing.T) {
	valid := testBenchmarkData("Samples-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))
	if err := validateSampleCounts(valid, 10); err != nil {
		t.Fatalf("ten samples failed: %v", err)
	}

	for _, test := range []struct {
		name  string
		count int
	}{
		{name: "nine", count: 9},
		{name: "eleven", count: 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := testBenchmarkData("Samples-8", repeatedFloat(10, test.count), repeatedFloat(1, test.count), repeatedFloat(0, test.count))
			requireErrorContains(t, validateSampleCounts(invalid, 10), "samples, want exactly 10")
		})
	}
}

func TestCompareBenchmarkDataUsesExactRegressionBoundary(t *testing.T) {
	base := testBenchmarkData("Threshold-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	tests := []struct {
		name       string
		candidate  float64
		regression bool
	}{
		{name: "below ten percent", candidate: math.Nextafter(11, math.Inf(-1)), regression: false},
		{name: "exactly ten percent", candidate: 11, regression: false},
		{name: "one ulp representation margin", candidate: math.Nextafter(11, math.Inf(1)), regression: false},
		{name: "above representation margin", candidate: math.Nextafter(math.Nextafter(11, math.Inf(1)), math.Inf(1)), regression: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := testBenchmarkData("Threshold-8", repeatedFloat(test.candidate, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
			comparisons, err := compareBenchmarkData(base, candidate, 10, 10)
			if err != nil {
				t.Fatal(err)
			}
			comparison := requireMetricComparison(t, comparisons, "time/op")
			if comparison.Regression != test.regression {
				t.Fatalf("time regression = %t, want %t (candidate %.17g)", comparison.Regression, test.regression, test.candidate)
			}
		})
	}
}

func TestCompareBenchmarkDataAllowsNormalizedTenPercentBoundary(t *testing.T) {
	base := testBenchmarkData("Threshold-8", repeatedFloat(100e-9, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	candidate := testBenchmarkData("Threshold-8", repeatedFloat(110e-9, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))

	comparisons, err := compareBenchmarkData(base, candidate, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := requireMetricComparison(t, comparisons, "time/op"); comparison.Regression {
		t.Fatalf("normalized 100 ns/op -> 110 ns/op marked as regression: %+v", comparison)
	}
}

func TestCompareBenchmarkDataBlocksAnyAllocationFromZeroBaseline(t *testing.T) {
	base := testBenchmarkData("ZeroAlloc-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))
	candidateAllocs := repeatedFloat(0, 10)
	candidateAllocs[len(candidateAllocs)-1] = 1
	candidate := testBenchmarkData("ZeroAlloc-8", repeatedFloat(10, 10), repeatedFloat(1, 10), candidateAllocs)

	comparisons, err := compareBenchmarkData(base, candidate, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	comparison := requireMetricComparison(t, comparisons, "allocs/op")
	if comparison.CandidateMedian != 0 {
		t.Fatalf("candidate median = %v, want zero to exercise any-positive policy", comparison.CandidateMedian)
	}
	if !comparison.Blocking || !comparison.Regression {
		t.Fatalf("zero-baseline allocation comparison = %+v, want blocking regression", comparison)
	}
}

func TestCompareBenchmarkDataRejectsMixedZeroAllocationBaseline(t *testing.T) {
	baseAllocs := repeatedFloat(0, 10)
	baseAllocs[len(baseAllocs)-1] = 1
	base := testBenchmarkData("MixedZero-8", repeatedFloat(10, 10), repeatedFloat(1, 10), baseAllocs)
	candidate := testBenchmarkData("MixedZero-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(0, 10))

	_, err := compareBenchmarkData(base, candidate, 10, 10)
	requireErrorContains(t, err, "allocs/op base median is zero but the base samples are not all zero")
}

func TestCompareBenchmarkDataRequiresButDoesNotBlockOnBytes(t *testing.T) {
	base := testBenchmarkData("Bytes-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	candidate := testBenchmarkData("Bytes-8", repeatedFloat(10, 10), repeatedFloat(100, 10), repeatedFloat(1, 10))

	comparisons, err := compareBenchmarkData(base, candidate, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	comparison := requireMetricComparison(t, comparisons, "B/op")
	if comparison.Blocking || comparison.Regression {
		t.Fatalf("B/op comparison = %+v, want informational result", comparison)
	}

	delete(candidate.Samples[benchmarkKey{Package: testBenchmarkPackage, Name: "Bytes-8"}], "B/op")
	_, err = compareBenchmarkData(base, candidate, 10, 10)
	requireErrorContains(t, err, "candidate samples: "+testBenchmarkPackage+":BenchmarkBytes-8 is missing required metric B/op")
}

func TestCompareBenchmarkDataRejectsBenchmarkSetMismatch(t *testing.T) {
	base := testBenchmarkData("BaseOnly-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	candidate := testBenchmarkData("CandidateOnly-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))

	_, err := compareBenchmarkData(base, candidate, 10, 10)
	requireErrorContains(t, err, "base-only=["+testBenchmarkPackage+":BenchmarkBaseOnly-8] candidate-only=["+testBenchmarkPackage+":BenchmarkCandidateOnly-8]")
}

func TestCompareBenchmarkDataRejectsSampleCountMismatch(t *testing.T) {
	base := testBenchmarkData("Mismatch-8", repeatedFloat(10, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	candidate := testBenchmarkData("Mismatch-8", repeatedFloat(10, 9), repeatedFloat(1, 9), repeatedFloat(1, 9))

	_, err := compareBenchmarkData(base, candidate, 10, 10)
	requireErrorContains(t, err, "candidate samples: "+testBenchmarkPackage+":BenchmarkMismatch-8 time/op has 9 samples, want exactly 10")
}

func TestCompareBenchmarkDataKeepsExtremeFiniteValuesJSONSafe(t *testing.T) {
	base := testBenchmarkData("Extreme-8", repeatedFloat(math.SmallestNonzeroFloat64, 10), repeatedFloat(1, 10), repeatedFloat(1, 10))
	candidate := testBenchmarkData("Extreme-8", repeatedFloat(math.MaxFloat64, 10), repeatedFloat(math.MaxFloat64, 10), repeatedFloat(math.MaxFloat64, 10))

	comparisons, err := compareBenchmarkData(base, candidate, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, comparison := range comparisons {
		if comparison.DeltaPercent != nil && (math.IsInf(*comparison.DeltaPercent, 0) || math.IsNaN(*comparison.DeltaPercent)) {
			t.Fatalf("%s retained a non-finite delta: %+v", comparison.Metric, comparison)
		}
	}
	if _, err := json.Marshal(gateReport{Comparisons: comparisons}); err != nil {
		t.Fatalf("marshal extreme finite comparisons: %v", err)
	}
}

func parseTestBenchmarkArtifact(t *testing.T, resultLines ...string) benchmarkData {
	t.Helper()
	artifact := testBenchmarkArtifact(resultLines...)
	if err := validateBenchmarkFraming([]byte(artifact)); err != nil {
		t.Fatalf("benchmark framing: %v", err)
	}
	data, err := parseBenchmarkData(strings.NewReader(artifact), "test.txt")
	if err != nil {
		t.Fatalf("parse benchmark data: %v", err)
	}
	return data
}

func testBenchmarkArtifact(resultLines ...string) string {
	lines := []string{
		"goos: linux",
		"goarch: amd64",
		"pkg: " + testBenchmarkPackage,
		"cpu: Test CPU",
		"",
	}
	lines = append(lines, resultLines...)
	lines = append(lines, "PASS", "")
	return strings.Join(lines, "\n")
}

func testBenchmarkData(name string, times, bytes, allocs []float64) benchmarkData {
	data := benchmarkData{
		Environment: benchmarkEnvironment{GOOS: "linux", GOARCH: "amd64", CPU: "Test CPU"},
		Samples:     make(map[benchmarkKey]map[string][]float64),
	}
	addTestBenchmark(&data, name, times, bytes, allocs)
	return data
}

func addTestBenchmark(data *benchmarkData, name string, times, bytes, allocs []float64) {
	data.Samples[benchmarkKey{Package: testBenchmarkPackage, Name: name}] = map[string][]float64{
		"sec/op":    append([]float64(nil), times...),
		"B/op":      append([]float64(nil), bytes...),
		"allocs/op": append([]float64(nil), allocs...),
	}
}

func cloneBenchmarkData(data benchmarkData) benchmarkData {
	cloned := benchmarkData{Environment: data.Environment, Samples: make(map[benchmarkKey]map[string][]float64, len(data.Samples))}
	for key, metrics := range data.Samples {
		cloned.Samples[key] = make(map[string][]float64, len(metrics))
		for metric, values := range metrics {
			cloned.Samples[key][metric] = append([]float64(nil), values...)
		}
	}
	return cloned
}

func repeatedFloat(value float64, count int) []float64 {
	values := make([]float64, count)
	for index := range values {
		values[index] = value
	}
	return values
}

func requireMetricComparison(t *testing.T, comparisons []metricComparison, metric string) metricComparison {
	t.Helper()
	for _, comparison := range comparisons {
		if comparison.Metric == metric {
			return comparison
		}
	}
	t.Fatalf("missing %s comparison in %+v", metric, comparisons)
	return metricComparison{}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want substring %q", err, want)
	}
}

func equalFloats(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
