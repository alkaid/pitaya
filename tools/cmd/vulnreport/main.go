package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	reachableVulnerabilityExitCode = 3
	dateLayout                     = "2006-01-02"
)

var (
	// This closed set mirrors scripts/scan-deps.sh. A mismatch fails closed;
	// each report is also bound to its root module through the SBOM below.
	expectedModules     = []string{"root", "pitaya-cli", "xk6-pitaya"}
	expectedModulePaths = map[string]string{
		"root":       "github.com/alkaid/pitaya/v2",
		"pitaya-cli": "github.com/alkaid/pitaya/pitaya-cli/v2",
		"xk6-pitaya": "github.com/alkaid/pitaya/xk6-pitaya",
	}
	advisoryPattern = regexp.MustCompile(`^GO-[0-9]{4}-[0-9]{4,}$`)
)

type reportMessage struct {
	Kind    string
	Config  *config
	SBOM    *sbom
	Finding *finding
}

type config struct {
	ProtocolVersion string `json:"protocol_version"`
	ScannerName     string `json:"scanner_name"`
	ScannerVersion  string `json:"scanner_version"`
	ScanLevel       string `json:"scan_level"`
	ScanMode        string `json:"scan_mode"`
}

type sbom struct {
	Modules []sbomModule `json:"modules"`
}

type sbomModule struct {
	Path string `json:"path"`
}

type finding struct {
	OSV   string       `json:"osv"`
	Trace []traceFrame `json:"trace"`
}

type traceFrame struct {
	Function string `json:"function"`
}

type reportPath struct {
	Module string
	Path   string
}

type exception struct {
	Advisory   string
	Module     string
	Owner      string
	Reason     string
	Mitigation string
	Expiry     time.Time
	Line       int
}

type findingKey struct {
	Advisory string
	Module   string
}

type moduleFlag []string

func (m *moduleFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *moduleFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now()))
}

func run(args []string, stdout, stderr io.Writer, now time.Time) int {
	flags := flag.NewFlagSet("vulnreport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var exceptionPath string
	var moduleValues moduleFlag
	var scanErrorValues moduleFlag
	flags.StringVar(&exceptionPath, "exceptions", "", "pipe-delimited accepted-risk file")
	flags.Var(&moduleValues, "module", "module=govulncheck-json (repeat for every module)")
	flags.Var(&scanErrorValues, "scan-error", "module whose scanner returned non-zero (repeat as needed)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: vulnreport --exceptions <file> --module root=<json> --module pitaya-cli=<json> --module xk6-pitaya=<json> [--scan-error <module>]...")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if exceptionPath == "" || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	reports, err := parseReportPaths(moduleValues)
	if err != nil {
		fmt.Fprintf(stderr, "report arguments: %v\n", err)
		return 1
	}
	scanErrors, err := parseScanErrors(scanErrorValues)
	if err != nil {
		fmt.Fprintf(stderr, "scan error arguments: %v\n", err)
		return 1
	}

	findings := make(map[string][]string, len(reports))
	validReports := make(map[string]bool, len(reports))
	configurationError := len(scanErrors) != 0
	for _, module := range expectedModules {
		if scanErrors[module] {
			fmt.Fprintf(stderr, "%s: govulncheck scanner returned non-zero\n", module)
		}
	}
	for _, report := range reports {
		file, openErr := os.Open(report.Path)
		if openErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", report.Path, openErr)
			configurationError = true
			continue
		}
		ids, scanErr := reachableFindings(file, report.Module)
		closeErr := file.Close()
		findings[report.Module] = ids
		if reportErr := errors.Join(scanErr, closeErr); reportErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", report.Path, reportErr)
			configurationError = true
			continue
		}
		if !scanErrors[report.Module] {
			validReports[report.Module] = true
		}
	}

	exceptions, err := loadExceptions(exceptionPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", exceptionPath, err)
		printPolicyUnavailableFindings(stdout, findings)
		return 1
	}

	// Accepted risks only suppress exit code 3 until expiry. Findings remain
	// visible and are never represented as fixed.
	used := make(map[findingKey]bool, len(exceptions))
	exceptedAdvisories := make(map[string]struct{}, len(exceptions))
	for key := range exceptions {
		exceptedAdvisories[key.Advisory] = struct{}{}
	}
	unaccepted := false
	for _, module := range expectedModules {
		for _, advisory := range findings[module] {
			key := findingKey{Advisory: advisory, Module: module}
			exception, accepted := exceptions[key]
			if !accepted {
				if _, hasScopedException := exceptedAdvisories[advisory]; hasScopedException {
					fmt.Fprintf(stdout, "%s: reachable vulnerability %s: OUTSIDE ACCEPTED SCOPE (not fixed)\n", module, advisory)
					configurationError = true
					continue
				}
				fmt.Fprintf(stdout, "%s: reachable vulnerability %s: UNACCEPTED\n", module, advisory)
				unaccepted = true
				continue
			}

			used[key] = true
			if expired(exception.Expiry, now) {
				fmt.Fprintf(stdout, "%s: reachable vulnerability %s: EXPIRED ACCEPTED RISK (expired %s, owner %s; not fixed)\n", module, advisory, exception.Expiry.Format(dateLayout), exception.Owner)
				configurationError = true
				continue
			}
			fmt.Fprintf(stdout, "%s: reachable vulnerability %s: ACCEPTED RISK through %s (owner %s; not fixed)\n", module, advisory, exception.Expiry.Format(dateLayout), exception.Owner)
		}
	}

	keys := sortedExceptionKeys(exceptions)
	for _, key := range keys {
		exception := exceptions[key]
		if expired(exception.Expiry, now) {
			fmt.Fprintf(stderr, "%s:%d: accepted risk %s for module %s expired on %s\n", exceptionPath, exception.Line, key.Advisory, key.Module, exception.Expiry.Format(dateLayout))
			configurationError = true
		}
		if validReports[key.Module] && !used[key] {
			fmt.Fprintf(stderr, "%s:%d: unused exception %s for module %s\n", exceptionPath, exception.Line, key.Advisory, key.Module)
			configurationError = true
		}
	}

	if configurationError {
		return 1
	}
	if unaccepted {
		return reachableVulnerabilityExitCode
	}
	return 0
}

func loadExceptions(path string) (map[findingKey]exception, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	exceptions, parseErr := parseExceptions(file)
	closeErr := file.Close()
	if err := errors.Join(parseErr, closeErr); err != nil {
		return nil, err
	}
	return exceptions, nil
}

func printPolicyUnavailableFindings(output io.Writer, findings map[string][]string) {
	for _, module := range expectedModules {
		for _, advisory := range findings[module] {
			fmt.Fprintf(output, "%s: reachable vulnerability %s: POLICY UNAVAILABLE (not fixed)\n", module, advisory)
		}
	}
}

func parseReportPaths(values []string) ([]reportPath, error) {
	paths := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid --module %q, want name=path", value)
		}
		module := strings.TrimSpace(parts[0])
		path := strings.TrimSpace(parts[1])
		if !knownModule(module) {
			return nil, fmt.Errorf("unknown module %q", module)
		}
		if _, duplicate := paths[module]; duplicate {
			return nil, fmt.Errorf("duplicate module %q", module)
		}
		paths[module] = path
	}

	result := make([]reportPath, 0, len(expectedModules))
	for _, module := range expectedModules {
		path, ok := paths[module]
		if !ok {
			return nil, fmt.Errorf("missing module %q", module)
		}
		result = append(result, reportPath{Module: module, Path: path})
	}
	return result, nil
}

func parseScanErrors(values []string) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		module := strings.TrimSpace(value)
		if !knownModule(module) {
			return nil, fmt.Errorf("unknown module %q", module)
		}
		if result[module] {
			return nil, fmt.Errorf("duplicate module %q", module)
		}
		result[module] = true
	}
	return result, nil
}

func parseExceptions(reader io.Reader) (map[findingKey]exception, error) {
	result := make(map[findingKey]exception)
	scanner := bufio.NewScanner(reader)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 6 {
			return nil, fmt.Errorf("line %d: got %d fields, want advisory|modules|owner|reason|mitigation|expiry", lineNumber, len(fields))
		}
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
			if fields[index] == "" {
				return nil, fmt.Errorf("line %d: field %d is empty", lineNumber, index+1)
			}
		}

		advisory, moduleList, owner := fields[0], fields[1], fields[2]
		if !advisoryPattern.MatchString(advisory) {
			return nil, fmt.Errorf("line %d: invalid advisory %q", lineNumber, advisory)
		}
		if !strings.HasPrefix(owner, "@") || len(owner) == 1 {
			return nil, fmt.Errorf("line %d: invalid owner %q", lineNumber, owner)
		}
		expiry, err := time.Parse(dateLayout, fields[5])
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid expiry %q: %w", lineNumber, fields[5], err)
		}

		modulesOnLine := make(map[string]struct{})
		for _, moduleValue := range strings.Split(moduleList, ",") {
			module := strings.TrimSpace(moduleValue)
			if module == "" {
				return nil, fmt.Errorf("line %d: empty module", lineNumber)
			}
			if !knownModule(module) {
				return nil, fmt.Errorf("line %d: unknown module %q", lineNumber, module)
			}
			if _, duplicate := modulesOnLine[module]; duplicate {
				return nil, fmt.Errorf("line %d: duplicate module %q", lineNumber, module)
			}
			modulesOnLine[module] = struct{}{}

			key := findingKey{Advisory: advisory, Module: module}
			if previous, duplicate := result[key]; duplicate {
				return nil, fmt.Errorf("line %d: duplicate exception %s for module %s (first declared on line %d)", lineNumber, advisory, module, previous.Line)
			}
			result[key] = exception{
				Advisory:   advisory,
				Module:     module,
				Owner:      owner,
				Reason:     fields[3],
				Mitigation: fields[4],
				Expiry:     expiry,
				Line:       lineNumber,
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read exceptions: %w", err)
	}
	return result, nil
}

func knownModule(module string) bool {
	for _, expected := range expectedModules {
		if module == expected {
			return true
		}
	}
	return false
}

func expired(expiry, now time.Time) bool {
	// Expiry is an inclusive UTC calendar date so CI and local time zones agree.
	return !now.UTC().Before(expiry.UTC().AddDate(0, 0, 1))
}

func sortedExceptionKeys(exceptions map[findingKey]exception) []findingKey {
	keys := make([]findingKey, 0, len(exceptions))
	for key := range exceptions {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Advisory != keys[j].Advisory {
			return keys[i].Advisory < keys[j].Advisory
		}
		return keys[i].Module < keys[j].Module
	})
	return keys
}

func sortedFindingIDs(ids map[string]struct{}) []string {
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func reachableFindings(reader io.Reader, module string) ([]string, error) {
	expectedModulePath, ok := expectedModulePaths[module]
	if !ok {
		return nil, fmt.Errorf("unknown report module %q", module)
	}

	decoder := json.NewDecoder(reader)
	sawConfig := false
	sawSBOM := false
	messageIndex := 0
	ids := make(map[string]struct{})
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sortedFindingIDs(ids), fmt.Errorf("decode govulncheck report: %w", err)
		}
		msg, err := decodeReportMessage(raw)
		if err != nil {
			return sortedFindingIDs(ids), fmt.Errorf("decode govulncheck report message %d: %w", messageIndex+1, err)
		}
		if messageIndex == 0 && msg.Kind != "config" {
			return sortedFindingIDs(ids), errors.New("govulncheck config must be the first message")
		}
		messageIndex++

		switch msg.Kind {
		case "config":
			if sawConfig {
				return sortedFindingIDs(ids), errors.New("govulncheck report has multiple config messages")
			}
			sawConfig = true
			if err := validateConfig(msg.Config); err != nil {
				return sortedFindingIDs(ids), err
			}
		case "SBOM":
			if sawSBOM {
				return sortedFindingIDs(ids), errors.New("govulncheck report has multiple SBOM messages")
			}
			sawSBOM = true
			if len(msg.SBOM.Modules) == 0 || msg.SBOM.Modules[0].Path != expectedModulePath {
				actual := ""
				if len(msg.SBOM.Modules) != 0 {
					actual = msg.SBOM.Modules[0].Path
				}
				return sortedFindingIDs(ids), fmt.Errorf("SBOM root module is %q, want %q for %s", actual, expectedModulePath, module)
			}
		case "finding":
			if msg.Finding.OSV == "" || !advisoryPattern.MatchString(msg.Finding.OSV) || len(msg.Finding.Trace) == 0 {
				return sortedFindingIDs(ids), errors.New("govulncheck report has malformed finding")
			}
			if msg.Finding.Trace[0].Function != "" {
				ids[msg.Finding.OSV] = struct{}{}
			}
		}
	}
	if !sawConfig {
		return sortedFindingIDs(ids), errors.New("govulncheck report has no config message")
	}
	if !sawSBOM {
		return sortedFindingIDs(ids), errors.New("govulncheck report has no SBOM message")
	}
	return sortedFindingIDs(ids), nil
}

func decodeReportMessage(raw json.RawMessage) (reportMessage, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return reportMessage{}, err
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return reportMessage{}, err
	}
	if len(envelope) != 1 {
		return reportMessage{}, fmt.Errorf("protocol message has %d fields, want exactly one", len(envelope))
	}

	for kind, payload := range envelope {
		if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
			return reportMessage{}, fmt.Errorf("protocol message %q is null", kind)
		}
		msg := reportMessage{Kind: kind}
		switch kind {
		case "config":
			msg.Config = &config{}
			if err := json.Unmarshal(payload, msg.Config); err != nil {
				return reportMessage{}, fmt.Errorf("decode config: %w", err)
			}
		case "SBOM":
			msg.SBOM = &sbom{}
			if err := json.Unmarshal(payload, msg.SBOM); err != nil {
				return reportMessage{}, fmt.Errorf("decode SBOM: %w", err)
			}
		case "finding":
			msg.Finding = &finding{}
			if err := json.Unmarshal(payload, msg.Finding); err != nil {
				return reportMessage{}, fmt.Errorf("decode finding: %w", err)
			}
		case "osv", "progress":
			var object map[string]json.RawMessage
			if err := json.Unmarshal(payload, &object); err != nil {
				return reportMessage{}, fmt.Errorf("decode %s object: %w", kind, err)
			}
			if object == nil {
				return reportMessage{}, fmt.Errorf("decode %s object: null object", kind)
			}
		default:
			return reportMessage{}, fmt.Errorf("unknown protocol message %q", kind)
		}
		return msg, nil
	}
	panic("unreachable")
}

func validateConfig(value *config) error {
	if value.ProtocolVersion != "v1.0.0" {
		return fmt.Errorf("protocol version is %q, want v1.0.0", value.ProtocolVersion)
	}
	if value.ScannerName != "govulncheck" || value.ScannerVersion == "" {
		return fmt.Errorf("scanner identity is %q version %q, want govulncheck with a version", value.ScannerName, value.ScannerVersion)
	}
	if value.ScanMode != "source" {
		return fmt.Errorf("scan mode is %q, want source", value.ScanMode)
	}
	if value.ScanLevel != "symbol" {
		return fmt.Errorf("scan level is %q, want symbol", value.ScanLevel)
	}
	return nil
}

func rejectDuplicateJSONKeys(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON message has trailing data")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delimiter {
	case '{':
		var seen []string
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			// encoding/json uses Unicode simple folding for struct fields. Use
			// EqualFold too so aliases such as "osv" and "o\u017fv" cannot
			// overwrite a security-relevant field during the typed decode.
			for _, previous := range seen {
				if strings.EqualFold(previous, key) {
					return fmt.Errorf("duplicate JSON object key %q", key)
				}
			}
			seen = append(seen, key)
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object has invalid closing delimiter")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array has invalid closing delimiter")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
