package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestReachableFindings(t *testing.T) {
	report := strings.Join([]string{
		`{"config":{"scan_level":"symbol"}}`,
		`{"finding":{"osv":"GO-MODULE","trace":[{"module":"example.com/module"}]}}`,
		`{"finding":{"osv":"GO-REACHABLE-B","trace":[{"function":"Read"},{"function":"main"}]}}`,
		`{"finding":{"osv":"GO-REACHABLE-A","trace":[{"function":"init"}]}}`,
		`{"finding":{"osv":"GO-REACHABLE-B","trace":[{"function":"Read"}]}}`,
	}, "\n")

	got, err := reachableFindings(strings.NewReader(report))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GO-REACHABLE-A", "GO-REACHABLE-B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableFindings() = %v, want %v", got, want)
	}
}

func TestReachableFindingsRejectsNonSymbolReport(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(`{"config":{"scan_level":"package"}}`))
	if err == nil || !strings.Contains(err.Error(), "want symbol") {
		t.Fatalf("reachableFindings() error = %v, want scan-level error", err)
	}
}

func TestReachableFindingsRejectsMissingConfig(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(`{"finding":{"osv":"GO-TEST","trace":[{"function":"Read"}]}}`))
	if err == nil || !strings.Contains(err.Error(), "no config") {
		t.Fatalf("reachableFindings() error = %v, want missing-config error", err)
	}
}

func TestReachableFindingsRejectsMalformedReport(t *testing.T) {
	_, err := reachableFindings(strings.NewReader(`{"config":{"scan_level":"symbol"}} {`))
	if err == nil || !strings.Contains(err.Error(), "decode govulncheck report") {
		t.Fatalf("reachableFindings() error = %v, want decode error", err)
	}
}
