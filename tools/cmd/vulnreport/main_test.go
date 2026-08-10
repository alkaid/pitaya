package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testAdvisory = "GO-2026-5932"

func TestReachableFindings(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"progress":{"message":"Scanning packages..."}}`,
		`{"osv":{"id":"GO-2026-1000"}}`,
		`{"finding":{"osv":"GO-2026-1000","trace":[{"module":"example.com/module"}]}}`,
		`{"finding":{"osv":"GO-2026-1002","trace":[{"function":"Read"},{"function":"main"}]}}`,
		`{"finding":{"osv":"GO-2026-1001","trace":[{"function":"init"}]}}`,
		`{"finding":{"osv":"GO-2026-1002","trace":[{"function":"Read"}]}}`,
	}, "\n")

	got, err := reachableFindings(strings.NewReader(report), "root")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GO-2026-1001", "GO-2026-1002"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableFindings() = %v, want %v", got, want)
	}
}

func TestReachableFindingsRejectsNonSymbolReport(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(configText("package")), "root")
	if err == nil || !strings.Contains(err.Error(), "want symbol") {
		t.Fatalf("reachableFindings() error = %v, want scan-level error", err)
	}
}

func TestReachableFindingsRejectsMissingConfig(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(""), "root")
	if err == nil || !strings.Contains(err.Error(), "no config") {
		t.Fatalf("reachableFindings() error = %v, want missing-config error", err)
	}
}

func TestReachableFindingsRejectsMalformedReport(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(configText("symbol")+"\n{"), "root")
	if err == nil || !strings.Contains(err.Error(), "decode govulncheck report") {
		t.Fatalf("reachableFindings() error = %v, want decode error", err)
	}
}

func TestReachableFindingsReturnsFindingsBeforeMalformedTail(t *testing.T) {
	got, err := reachableFindings(strings.NewReader(reportText("root", []string{testAdvisory})+"{"), "root")
	if err == nil || !strings.Contains(err.Error(), "decode govulncheck report") {
		t.Fatalf("reachableFindings() error = %v, want decode error", err)
	}
	if want := []string{testAdvisory}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableFindings() = %v, want findings parsed before the error: %v", got, want)
	}
}

func TestReachableFindingsRejectsMalformedFinding(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"finding":{"osv":"GO-TEST"}}`,
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "malformed finding") {
		t.Fatalf("reachableFindings() error = %v, want malformed-finding error", err)
	}
}

func TestReachableFindingsRejectsDuplicateConfig(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		configText("symbol"),
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "multiple config") {
		t.Fatalf("reachableFindings() error = %v, want duplicate-config error", err)
	}
}

func TestReachableFindingsRejectsDuplicateJSONKey(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"finding":{"osv":"GO-2026-9999","trace":[{"function":"Read"}]},"finding":null}`,
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), `duplicate JSON object key "finding"`) {
		t.Fatalf("reachableFindings() error = %v, want duplicate-key error", err)
	}
}

func TestReachableFindingsRejectsCaseFoldedDuplicateJSONKey(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"finding":{"osv":"GO-2026-9999","trace":[{"function":"Read"}],"TRACE":[{"function":""}]}}`,
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), `duplicate JSON object key "TRACE"`) {
		t.Fatalf("reachableFindings() error = %v, want case-folded duplicate-key error", err)
	}
}

func TestReachableFindingsRejectsUnicodeFoldedDuplicateJSONKey(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"finding":{"osv":"GO-2026-9999","o\u017fv":"GO-2026-5932","trace":[{"function":"Read"}]}}`,
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("reachableFindings() error = %v, want Unicode-folded duplicate-key error", err)
	}
}

func TestReachableFindingsRejectsMultipleProtocolFields(t *testing.T) {
	report := strings.Join([]string{
		configText("symbol"),
		sbomText("root"),
		`{"progress":{},"finding":{"osv":"GO-2026-9999","trace":[{"function":"Read"}]}}`,
	}, "\n")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "want exactly one") {
		t.Fatalf("reachableFindings() error = %v, want protocol-union error", err)
	}
}

func TestReachableFindingsRejectsMismatchedSBOM(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(reportText("xk6-pitaya", []string{testAdvisory})), "root")
	if err == nil || !strings.Contains(err.Error(), `want "github.com/alkaid/pitaya/v2" for root`) {
		t.Fatalf("reachableFindings() error = %v, want SBOM identity error", err)
	}
}

func TestReachableFindingsRejectsMissingSBOM(t *testing.T) {
	report := configText("symbol") + "\n" + `{"progress":{}}`
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "no SBOM") {
		t.Fatalf("reachableFindings() error = %v, want missing-SBOM error", err)
	}
}

func TestReachableFindingsRejectsInvalidConfig(t *testing.T) {
	valid := configText("symbol")
	tests := []struct {
		name   string
		report string
		want   string
	}{
		{name: "protocol", report: strings.Replace(valid, `"v1.0.0"`, `"v2.0.0"`, 1), want: "protocol version"},
		{name: "scanner", report: strings.Replace(valid, `"govulncheck"`, `"other"`, 1), want: "scanner identity"},
		{name: "scanner version", report: strings.Replace(valid, `"v1.6.0"`, `""`, 1), want: "scanner identity"},
		{name: "mode", report: strings.Replace(valid, `"source"`, `"binary"`, 1), want: "scan mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := reachableFindings(strings.NewReader(test.report), "root")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("reachableFindings() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReachableFindingsRequiresConfigFirst(t *testing.T) {
	report := sbomText("root") + "\n" + configText("symbol")
	_, err := reachableFindings(strings.NewReader(report), "root")
	if err == nil || !strings.Contains(err.Error(), "config must be the first") {
		t.Fatalf("reachableFindings() error = %v, want first-config error", err)
	}
}

func TestParseExceptions(t *testing.T) {
	text := exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05")
	exceptions, err := parseExceptions(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(exceptions) != len(expectedModules) {
		t.Fatalf("parseExceptions() returned %d pairs, want %d", len(exceptions), len(expectedModules))
	}
	got := exceptions[findingKey{Advisory: testAdvisory, Module: "root"}]
	if got.Owner != "@alkaid" || got.Reason != "No fixed version is available." || got.Mitigation != "Production uses ordinary etcd only; OpenPGP remote configuration is disabled." {
		t.Fatalf("parseExceptions() metadata = %#v", got)
	}
}

func TestParseExceptionsRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "field count", text: "GO-2026-5932|root", want: "got 2 fields"},
		{name: "empty field", text: "GO-2026-5932|root|@alkaid||mitigation|2027-08-05", want: "field 4 is empty"},
		{name: "advisory", text: exceptionText("CVE-2026-1", "root", "2027-08-05"), want: "invalid advisory"},
		{name: "owner", text: "GO-2026-5932|root|alkaid|reason|mitigation|2027-08-05", want: "invalid owner"},
		{name: "expiry", text: exceptionText(testAdvisory, "root", "2027-02-30"), want: "invalid expiry"},
		{name: "unknown module", text: exceptionText(testAdvisory, "server", "2027-08-05"), want: "unknown module"},
		{name: "duplicate module", text: exceptionText(testAdvisory, "root,root", "2027-08-05"), want: "duplicate module"},
		{
			name: "duplicate pair",
			text: exceptionText(testAdvisory, "root,pitaya-cli", "2027-08-05") + exceptionText(testAdvisory, "root,xk6-pitaya", "2027-08-05"),
			want: "duplicate exception",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseExceptions(strings.NewReader(test.text))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseExceptions() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseReportPathsRequiresExactModuleSet(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "missing", values: []string{"root=a", "pitaya-cli=b"}, want: `missing module "xk6-pitaya"`},
		{name: "unknown", values: []string{"root=a", "pitaya-cli=b", "server=c"}, want: `unknown module "server"`},
		{name: "duplicate", values: []string{"root=a", "root=b", "pitaya-cli=c", "xk6-pitaya=d"}, want: `duplicate module "root"`},
		{name: "invalid", values: []string{"root", "pitaya-cli=b", "xk6-pitaya=c"}, want: "want name=path"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseReportPaths(test.values)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseReportPaths() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunRejectsInvalidScanErrorArguments(t *testing.T) {
	baseArgs, _ := writeRunFixture(t, "# no exceptions\n", map[string][]string{})
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "unknown", values: []string{"--scan-error", "server"}, want: `unknown module "server"`},
		{name: "duplicate", values: []string{"--scan-error", "root", "--scan-error", "root"}, want: `duplicate module "root"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string{}, baseArgs...), test.values...)
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
			if code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("run() = %d, stderr = %q; want scan-error argument failure %q", code, stderr.String(), test.want)
			}
		})
	}
}

func TestRunAcceptsFindingThroughExpiryDate(t *testing.T) {
	findings := findingsForEveryModule(testAdvisory)
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"), findings)

	var stdout, stderr bytes.Buffer
	// 07:59:59 in UTC+8 is the final second of the inclusive UTC expiry date.
	code := run(args, &stdout, &stderr, time.Date(2027, time.August, 6, 7, 59, 59, 0, time.FixedZone("test", 8*60*60)))
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), ": ACCEPTED RISK through 2027-08-05"); got != len(expectedModules) {
		t.Fatalf("accepted output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
	if got := strings.Count(stdout.String(), "not fixed"); got != len(expectedModules) {
		t.Fatalf("not-fixed output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunRejectsExpiredAcceptedRisk(t *testing.T) {
	findings := findingsForEveryModule(testAdvisory)
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"), findings)

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2027, time.August, 6, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := strings.Count(stdout.String(), ": EXPIRED ACCEPTED RISK"); got != len(expectedModules) {
		t.Fatalf("expired output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
	if !strings.Contains(stderr.String(), "expired on 2027-08-05") {
		t.Fatalf("stderr = %q, want expiry error", stderr.String())
	}
}

func TestRunReturnsThreeForUnacceptedFinding(t *testing.T) {
	findings := findingsForEveryModule(testAdvisory)
	findings["root"] = append(findings["root"], "GO-2026-9999")
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"), findings)

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != reachableVulnerabilityExitCode {
		t.Fatalf("run() = %d, want %d; stderr:\n%s", code, reachableVulnerabilityExitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "root: reachable vulnerability GO-2026-9999: UNACCEPTED") {
		t.Fatalf("stdout missing unaccepted finding:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), testAdvisory+": ACCEPTED RISK") {
		t.Fatalf("stdout missing accepted finding:\n%s", stdout.String())
	}
}

func TestRunRejectsUnusedExceptionPair(t *testing.T) {
	findings := findingsForEveryModule(testAdvisory)
	findings["xk6-pitaya"] = nil
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"), findings)

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unused exception GO-2026-5932 for module xk6-pitaya") {
		t.Fatalf("stderr missing exact unused pair:\n%s", stderr.String())
	}
}

func TestRunDoesNotMarkExceptionUnusedAfterScannerFailure(t *testing.T) {
	findings := findingsForEveryModule(testAdvisory)
	findings["xk6-pitaya"] = nil
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"), findings)
	args = append(args, "--scan-error", "xk6-pitaya")

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "xk6-pitaya: govulncheck scanner returned non-zero") {
		t.Fatalf("stderr missing scanner failure:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "unused exception "+testAdvisory+" for module xk6-pitaya") {
		t.Fatalf("scanner failure was misclassified as an unused exception:\n%s", stderr.String())
	}
}

func TestRunPrintsFindingsEmittedBeforeScannerFailure(t *testing.T) {
	args, _ := writeRunFixture(
		t,
		exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"),
		findingsForEveryModule(testAdvisory),
	)
	args = append(args, "--scan-error", "root")

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := strings.Count(stdout.String(), ": ACCEPTED RISK through 2027-08-05"); got != len(expectedModules) {
		t.Fatalf("accepted output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
}

func TestRunRejectsUnknownException(t *testing.T) {
	args, _ := writeRunFixture(t, exceptionText("GO-2026-9999", "root", "2027-08-05"), map[string][]string{})

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 || !strings.Contains(stderr.String(), "unused exception GO-2026-9999 for module root") {
		t.Fatalf("run() = %d, stderr = %q; want unused-exception failure", code, stderr.String())
	}
}

func TestRunRejectsMalformedReport(t *testing.T) {
	args, paths := writeRunFixture(t, "# no exceptions\n", map[string][]string{})
	writeFile(t, paths["root"], configText("symbol")+"\n{")

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 || !strings.Contains(stderr.String(), "decode govulncheck report") {
		t.Fatalf("run() = %d, stderr = %q; want malformed-report failure", code, stderr.String())
	}
}

func TestRunPrintsValidFindingsDuringPartialReportFailure(t *testing.T) {
	args, paths := writeRunFixture(
		t,
		exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"),
		findingsForEveryModule(testAdvisory),
	)
	writeFile(t, paths["xk6-pitaya"], reportText("xk6-pitaya", []string{testAdvisory})+"{")

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := strings.Count(stdout.String(), ": ACCEPTED RISK through 2027-08-05"); got != len(expectedModules) {
		t.Fatalf("accepted output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
	if !strings.Contains(stderr.String(), "decode govulncheck report") {
		t.Fatalf("stderr = %q, want report error", stderr.String())
	}
}

func TestRunPrintsFindingsWhenExceptionPolicyIsMalformed(t *testing.T) {
	args, _ := writeRunFixture(t, "invalid exception\n", findingsForEveryModule(testAdvisory))

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := strings.Count(stdout.String(), ": POLICY UNAVAILABLE (not fixed)"); got != len(expectedModules) {
		t.Fatalf("policy-unavailable output count = %d, want %d; stdout:\n%s", got, len(expectedModules), stdout.String())
	}
	if !strings.Contains(stderr.String(), "got 1 fields") {
		t.Fatalf("stderr = %q, want exception parse error", stderr.String())
	}
}

func TestRunRejectsReportWithWrongModuleIdentity(t *testing.T) {
	args, paths := writeRunFixture(
		t,
		exceptionText(testAdvisory, strings.Join(expectedModules, ","), "2027-08-05"),
		findingsForEveryModule(testAdvisory),
	)
	xk6Report, err := os.ReadFile(paths["xk6-pitaya"])
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, paths["root"], string(xk6Report))

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `want "github.com/alkaid/pitaya/v2" for root`) {
		t.Fatalf("stderr = %q, want SBOM identity error", stderr.String())
	}
}

func TestRunRejectsInvalidExceptionScope(t *testing.T) {
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, "server", "2027-08-05"), findingsForEveryModule(testAdvisory))

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 || !strings.Contains(stderr.String(), `unknown module "server"`) {
		t.Fatalf("run() = %d, stderr = %q; want invalid-scope failure", code, stderr.String())
	}
}

func TestRunRejectsFindingOutsideExceptionScope(t *testing.T) {
	args, _ := writeRunFixture(t, exceptionText(testAdvisory, "root", "2027-08-05"), findingsForEveryModule(testAdvisory))

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC))
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := strings.Count(stdout.String(), ": OUTSIDE ACCEPTED SCOPE (not fixed)"); got != 2 {
		t.Fatalf("outside-scope output count = %d, want 2; stdout:\n%s", got, stdout.String())
	}
	if strings.Contains(stdout.String(), ": UNACCEPTED") {
		t.Fatalf("scoped configuration error was classified as an unaccepted advisory:\n%s", stdout.String())
	}
}

func exceptionText(advisory, modules, expiry string) string {
	return fmt.Sprintf("%s|%s|@alkaid|No fixed version is available.|Production uses ordinary etcd only; OpenPGP remote configuration is disabled.|%s\n", advisory, modules, expiry)
}

func findingsForEveryModule(advisory string) map[string][]string {
	result := make(map[string][]string, len(expectedModules))
	for _, module := range expectedModules {
		result[module] = []string{advisory}
	}
	return result
}

func writeRunFixture(t *testing.T, exceptions string, findings map[string][]string) ([]string, map[string]string) {
	t.Helper()
	directory := t.TempDir()
	exceptionPath := filepath.Join(directory, "exceptions.txt")
	writeFile(t, exceptionPath, exceptions)
	args := []string{"--exceptions", exceptionPath}
	paths := make(map[string]string, len(expectedModules))
	for _, module := range expectedModules {
		path := filepath.Join(directory, module+".json")
		writeFile(t, path, reportText(module, findings[module]))
		paths[module] = path
		args = append(args, "--module", module+"="+path)
	}
	return args, paths
}

func reportText(module string, advisories []string) string {
	var report strings.Builder
	report.WriteString(configText("symbol"))
	report.WriteByte('\n')
	report.WriteString(sbomText(module))
	report.WriteByte('\n')
	for _, advisory := range advisories {
		fmt.Fprintf(&report, "{\"finding\":{\"osv\":%q,\"trace\":[{\"function\":\"Read\"}]}}\n", advisory)
	}
	return report.String()
}

func configText(scanLevel string) string {
	return fmt.Sprintf(`{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.6.0","scan_level":%q,"scan_mode":"source"}}`, scanLevel)
}

func sbomText(module string) string {
	return fmt.Sprintf(`{"SBOM":{"modules":[{"path":%q}]}}`, expectedModulePaths[module])
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
