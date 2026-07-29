package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const reachableVulnerabilityExitCode = 3

type message struct {
	Config  *config  `json:"config"`
	Finding *finding `json:"finding"`
}

type config struct {
	ScanLevel string `json:"scan_level"`
}

type finding struct {
	OSV   string       `json:"osv"`
	Trace []traceFrame `json:"trace"`
}

type traceFrame struct {
	Function string `json:"function"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: vulnreport <govulncheck-json> [...]")
		os.Exit(2)
	}

	reachable := false
	for _, path := range os.Args[1:] {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		ids, scanErr := reachableFindings(file)
		closeErr := file.Close()
		if scanErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, scanErr)
			os.Exit(1)
		}
		if closeErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, closeErr)
			os.Exit(1)
		}
		if len(ids) > 0 {
			fmt.Printf("%s: reachable vulnerabilities: %s\n", path, strings.Join(ids, ", "))
			reachable = true
		}
	}
	if reachable {
		os.Exit(reachableVulnerabilityExitCode)
	}
}

func reachableFindings(reader io.Reader) ([]string, error) {
	decoder := json.NewDecoder(reader)
	sawConfig := false
	ids := make(map[string]struct{})
	for {
		var msg message
		err := decoder.Decode(&msg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode govulncheck report: %w", err)
		}
		if msg.Config != nil {
			sawConfig = true
			if msg.Config.ScanLevel != "symbol" {
				return nil, fmt.Errorf("scan level is %q, want symbol", msg.Config.ScanLevel)
			}
		}
		if msg.Finding == nil || msg.Finding.OSV == "" || len(msg.Finding.Trace) == 0 {
			continue
		}
		if msg.Finding.Trace[0].Function != "" {
			ids[msg.Finding.OSV] = struct{}{}
		}
	}
	if !sawConfig {
		return nil, errors.New("govulncheck report has no config message")
	}

	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}
