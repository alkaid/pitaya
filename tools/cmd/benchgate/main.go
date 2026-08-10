package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	metadataSchemaVersion = 2
	reportSchemaVersion   = 2
	minimumSamples        = 10
	maximumSamples        = 100
	regressionPercent     = 10.0
	maximumArtifactAge    = 2 * time.Hour
	maximumFutureSkew     = 5 * time.Minute
	maximumMetadataBytes  = 1 << 20
)

var (
	errUsage               = errors.New("invalid command usage")
	errRegression          = errors.New("benchmark regression")
	idPattern              = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	iterationTime          = regexp.MustCompile(`^[1-9][0-9]{0,6}x$`)
	durationTime           = regexp.MustCompile(`^([0-9]+(ns|us|ms|s|m|h))+$`)
	nowUTC                 = func() time.Time { return time.Now().UTC() }
	embeddedProvenanceKeys = []string{
		"bench-role", "bench-run-id", "bench-commit", "bench-started-at", "bench-finished-at", "bench-stamped-at",
		"bench-gate-source-sha",
		"bench-runner-id", "bench-go-version", "bench-num-cpu", "bench-gomaxprocs",
		"bench-cpu-fingerprint", "bench-machine-id", "bench-boot-id", "bench-kernel", "bench-cpu-constraints",
		"bench-goamd64", "bench-goexperiment", "bench-cgo-enabled", "bench-goflags", "bench-godebug",
		"bench-gogc", "bench-gomemlimit", "bench-gotoolchain", "bench-docker", "bench-compose",
		"bench-profile", "bench-package", "bench-pattern", "bench-benchtime", "bench-count", "bench-timeout",
		"bench-policy-sha256", "bench-apparatus-sha256",
	}
)

type benchmarkConfig struct {
	Profile         string `json:"profile"`
	Package         string `json:"package"`
	Pattern         string `json:"pattern"`
	Benchtime       string `json:"benchtime"`
	Count           int    `json:"count"`
	Timeout         string `json:"timeout"`
	PolicySHA256    string `json:"policy_sha256"`
	ApparatusSHA256 string `json:"apparatus_sha256"`
	GateSourceSHA   string `json:"gate_source_sha"`
}

type runnerProvenance struct {
	ID                   string `json:"id"`
	GoVersion            string `json:"go_version"`
	GOOS                 string `json:"goos"`
	GOARCH               string `json:"goarch"`
	NumCPU               int    `json:"num_cpu"`
	GOMAXPROCS           int    `json:"gomaxprocs"`
	CPU                  string `json:"cpu"`
	CPUFingerprint       string `json:"cpu_fingerprint"`
	MachineIDSHA256      string `json:"machine_id_sha256"`
	BootIDSHA256         string `json:"boot_id_sha256"`
	KernelRelease        string `json:"kernel_release"`
	CPUConstraintsSHA256 string `json:"cpu_constraints_sha256"`
	GOAMD64              string `json:"goamd64"`
	GOEXPERIMENT         string `json:"goexperiment"`
	CGOEnabled           string `json:"cgo_enabled"`
	GOFLAGS              string `json:"goflags"`
	GODEBUG              string `json:"godebug"`
	GOGC                 string `json:"gogc"`
	GOMEMLIMIT           string `json:"gomemlimit"`
	GOTOOLCHAIN          string `json:"gotoolchain"`
	DockerVersion        string `json:"docker_version"`
	DockerComposeVersion string `json:"docker_compose_version"`
}

type resultProvenance struct {
	SchemaVersion int              `json:"schema_version"`
	Role          string           `json:"role"`
	RunID         string           `json:"run_id"`
	CommitSHA     string           `json:"commit_sha"`
	GateSourceSHA string           `json:"gate_source_sha"`
	ResultSHA256  string           `json:"result_sha256"`
	StartedAt     string           `json:"started_at"`
	FinishedAt    string           `json:"finished_at"`
	StampedAt     string           `json:"stamped_at"`
	Runner        runnerProvenance `json:"runner"`
	Benchmark     benchmarkConfig  `json:"benchmark"`
}

type provenanceTimes struct {
	Started  time.Time
	Finished time.Time
	Stamped  time.Time
}

type gateReport struct {
	SchemaVersion           int                `json:"schema_version"`
	RunID                   string             `json:"run_id"`
	BaseSHA                 string             `json:"base_sha"`
	CandidateSHA            string             `json:"candidate_sha"`
	GateSourceSHA           string             `json:"gate_source_sha"`
	BaseResultSHA256        string             `json:"base_result_sha256"`
	CandidateResultSHA256   string             `json:"candidate_result_sha256"`
	BaseMetadataSHA256      string             `json:"base_metadata_sha256"`
	CandidateMetadataSHA256 string             `json:"candidate_metadata_sha256"`
	GeneratedAt             string             `json:"generated_at"`
	MinimumSamples          int                `json:"minimum_samples"`
	RegressionPercent       float64            `json:"regression_percent"`
	Runner                  runnerProvenance   `json:"runner"`
	Benchmark               benchmarkConfig    `json:"benchmark"`
	Passed                  bool               `json:"passed"`
	RegressionCount         int                `json:"regression_count"`
	Comparisons             []metricComparison `json:"comparisons"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "fingerprint":
		err = runFingerprint(args[1:], stdout, stderr)
	case "verify-tree":
		err = runVerifyTree(args[1:], stdout, stderr)
	case "probe-runner":
		err = runProbeRunner(args[1:], stdout, stderr)
	case "stamp":
		err = runStamp(args[1:], stdout, stderr)
	case "compare":
		err = runCompare(args[1:], stdout, stderr)
	case "verify-report":
		err = runVerifyReport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err == nil {
		return 0
	}
	// Keep caller mistakes (2), gate failures (1), and measured regressions (3)
	// distinguishable to the fixed runner; explicit flag help remains success.
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errUsage) {
		if err != errUsage {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	if errors.Is(err, errRegression) {
		fmt.Fprintln(stderr, err)
		return 3
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func asUsageError(err error) error {
	if errors.Is(err, flag.ErrHelp) || errors.Is(err, errUsage) {
		return err
	}
	return fmt.Errorf("%w: %v", errUsage, err)
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "usage:")
	fmt.Fprintln(output, "  benchgate fingerprint -policy FILE -repo DIR -pattern REGEXP -benchtime TIME -count N -timeout TIME")
	fmt.Fprintln(output, "  benchgate verify-tree -repo DIR -commit SHA")
	fmt.Fprintln(output, "  benchgate probe-runner -runner-id ID")
	fmt.Fprintln(output, "  benchgate stamp -policy FILE -repo DIR -raw FILE -runner-probe FILE -result FILE -metadata FILE -role base|candidate -commit SHA -gate-source-sha SHA -run-id ID -runner-id ID -started-at TIME -finished-at TIME")
	fmt.Fprintln(output, "  benchgate compare -base-policy FILE -candidate-policy FILE -base-repo DIR -candidate-repo DIR -base FILE -candidate FILE -base-metadata FILE -candidate-metadata FILE -base-sha SHA -candidate-sha SHA -gate-source-sha SHA -run-id ID -runner-id ID -report FILE")
	fmt.Fprintln(output, "  benchgate verify-report -report FILE -policy FILE -base-result FILE -candidate-result FILE -base-metadata FILE -candidate-metadata FILE -base-sha SHA -candidate-sha SHA -gate-source-sha SHA -policy-sha256 DIGEST -apparatus-sha256 DIGEST -run-id ID -runner-id ID -expect pass|regression")
}

func runProbeRunner(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("probe-runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var runnerID string
	flags.StringVar(&runnerID, "runner-id", "", "stable dedicated-runner identity")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || runnerID == "" {
		flags.Usage()
		return errUsage
	}
	if err := validateID("runner ID", runnerID); err != nil {
		return asUsageError(err)
	}
	runner, err := currentRunner(runnerID, "<probe>")
	if err != nil {
		return err
	}
	// A probe has no clock field: byte equality therefore means the runner
	// identity observed immediately around one harness invocation did not drift.
	if err := json.NewEncoder(stdout).Encode(runner); err != nil {
		return fmt.Errorf("write runner probe: %w", err)
	}
	return nil
}

func runFingerprint(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var policyPath, repo, pattern, benchtime, timeout string
	var count int
	flags.StringVar(&policyPath, "policy", "", "fixed benchmark policy")
	flags.StringVar(&repo, "repo", "", "clean benchmark worktree")
	flags.StringVar(&pattern, "pattern", "", "actual benchmark pattern")
	flags.StringVar(&benchtime, "benchtime", "", "actual benchmark time")
	flags.IntVar(&count, "count", 0, "actual sample count")
	flags.StringVar(&timeout, "timeout", "", "actual benchmark timeout")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || policyPath == "" || repo == "" || pattern == "" || benchtime == "" || count == 0 || timeout == "" {
		flags.Usage()
		return errUsage
	}
	// The shell anchors the policy digest outside the candidate, then compares
	// this policy/apparatus pair before running either revision. Apparatus changes
	// are incomparable, not performance passes or failures.
	policy, policyDigest, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	if pattern != policy.Pattern || benchtime != policy.Benchtime || count != policy.Count || timeout != policy.Timeout {
		return fmt.Errorf("benchmark invocation does not match policy: pattern=%q benchtime=%q count=%d timeout=%q",
			pattern, benchtime, count, timeout)
	}
	repo, err = filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	if err := requireCleanCheckout(repo); err != nil {
		return err
	}
	apparatus, err := apparatusDigestForPolicy(repo, policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s\n", policyDigest, apparatus)
	return nil
}

func runStamp(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("stamp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var policyPath, repo, rawPath, runnerProbePath, resultPath, metadataPath, role, commit, gateSourceSHA, runID, runnerID, startedAt, finishedAt string
	flags.StringVar(&policyPath, "policy", "", "fixed benchmark policy")
	flags.StringVar(&repo, "repo", "", "clean benchmark worktree")
	flags.StringVar(&rawPath, "raw", "", "unsealed Go benchmark result")
	flags.StringVar(&runnerProbePath, "runner-probe", "", "post-harness runner probe")
	flags.StringVar(&resultPath, "result", "", "output sealed benchmark result")
	flags.StringVar(&metadataPath, "metadata", "", "output provenance sidecar")
	flags.StringVar(&role, "role", "", "base or candidate")
	flags.StringVar(&commit, "commit", "", "full benchmarked commit SHA")
	flags.StringVar(&gateSourceSHA, "gate-source-sha", "", "full trusted gate source SHA")
	flags.StringVar(&runID, "run-id", "", "fresh comparison run identity")
	flags.StringVar(&runnerID, "runner-id", "", "stable dedicated-runner identity")
	flags.StringVar(&startedAt, "started-at", "", "orchestrator-owned UTC start time")
	flags.StringVar(&finishedAt, "finished-at", "", "orchestrator-owned UTC finish time")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || policyPath == "" || repo == "" || rawPath == "" || runnerProbePath == "" || resultPath == "" || metadataPath == "" || role == "" || commit == "" || gateSourceSHA == "" || runID == "" || runnerID == "" || startedAt == "" || finishedAt == "" {
		flags.Usage()
		return errUsage
	}
	if role != "base" && role != "candidate" {
		return asUsageError(errors.New("role must be base or candidate"))
	}
	if err := validateID("run ID", runID); err != nil {
		return asUsageError(err)
	}
	if err := validateID("runner ID", runnerID); err != nil {
		return asUsageError(err)
	}
	if err := validateSHA(commit); err != nil {
		return asUsageError(fmt.Errorf("commit SHA: %w", err))
	}
	if err := validateSHA(gateSourceSHA); err != nil {
		return asUsageError(fmt.Errorf("gate source SHA: %w", err))
	}
	started, err := parseUTCTimestamp(startedAt)
	if err != nil {
		return asUsageError(fmt.Errorf("started-at: %w", err))
	}
	finished, err := parseUTCTimestamp(finishedAt)
	if err != nil {
		return asUsageError(fmt.Errorf("finished-at: %w", err))
	}
	if !finished.After(started) {
		return asUsageError(errors.New("finished-at must be after started-at"))
	}

	policy, policyDigest, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	repo, err = filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	rawPath, err = filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("resolve raw result: %w", err)
	}
	runnerProbePath, err = filepath.Abs(runnerProbePath)
	if err != nil {
		return fmt.Errorf("resolve runner probe: %w", err)
	}
	resultPath, err = filepath.Abs(resultPath)
	if err != nil {
		return fmt.Errorf("resolve sealed result: %w", err)
	}
	metadataPath, err = filepath.Abs(metadataPath)
	if err != nil {
		return fmt.Errorf("resolve metadata: %w", err)
	}
	if err := requireAbsent(metadataPath); err != nil {
		return fmt.Errorf("metadata output: %w", err)
	}
	if err := requireAbsent(resultPath); err != nil {
		return fmt.Errorf("sealed result output: %w", err)
	}
	if rawPath == runnerProbePath || rawPath == resultPath || rawPath == metadataPath || runnerProbePath == resultPath || runnerProbePath == metadataPath || resultPath == metadataPath {
		return asUsageError(errors.New("raw result, runner probe, sealed result, and metadata paths must differ"))
	}
	probe, err := readRunnerProbe(runnerProbePath, runnerID)
	if err != nil {
		return fmt.Errorf("read runner probe: %w", err)
	}
	resolved, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve benchmark checkout: %w", err)
	}
	if resolved != commit {
		return fmt.Errorf("benchmark checkout is %s, want %s", resolved, commit)
	}
	if err := requireCleanCheckout(repo); err != nil {
		return err
	}
	apparatus, err := apparatusDigestForPolicy(repo, policy)
	if err != nil {
		return fmt.Errorf("measurement apparatus: %w", err)
	}
	// The fixed runner supplies the reviewed gate identity. Sealing it beside the
	// policy detects accidental source/result mismatch; same-UID hostile candidate
	// code is outside this maintainer-only gate's threat model.
	configured := configFromPolicy(policy, policyDigest, apparatus, gateSourceSHA)

	rawArtifact, err := loadBenchmarkArtifact(rawPath)
	if err != nil {
		return fmt.Errorf("read raw benchmark result: %w", err)
	}
	if err := validateSampleCounts(rawArtifact.Data, configured.Count); err != nil {
		return err
	}
	if err := validateExpectedBenchmarkSet(rawArtifact.Data, policy); err != nil {
		return err
	}
	if artifactHasEmbeddedProvenance(rawArtifact.Data) {
		return errors.New("raw benchmark result already contains benchgate provenance")
	}
	// This is defense in depth against restamping an old raw file. The exclusive
	// orchestrator, which owns the run interval and paths, remains the trust edge.
	if rawArtifact.ModTime.Before(started.Add(-2*time.Second)) || rawArtifact.ModTime.After(finished.Add(2*time.Second)) {
		return fmt.Errorf("raw benchmark modification time %s is outside run interval %s..%s", rawArtifact.ModTime.Format(time.RFC3339Nano), startedAt, finishedAt)
	}
	if rawArtifact.Data.Environment.GOOS != runtime.GOOS || rawArtifact.Data.Environment.GOARCH != runtime.GOARCH {
		return fmt.Errorf("raw benchmark platform is %s/%s, stamping runtime is %s/%s",
			rawArtifact.Data.Environment.GOOS, rawArtifact.Data.Environment.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	runner, err := currentRunner(runnerID, rawArtifact.Data.Environment.CPU)
	if err != nil {
		return err
	}
	if err := validateRunnerProbe(probe, runner); err != nil {
		return fmt.Errorf("runner probe: %w", err)
	}
	stamped := nowUTC()
	if stamped.Before(finished) || stamped.After(finished.Add(maximumFutureSkew)) {
		return fmt.Errorf("stamp time %s is not immediately after benchmark finish %s", stamped.Format(time.RFC3339Nano), finishedAt)
	}
	metadata := resultProvenance{
		SchemaVersion: metadataSchemaVersion,
		Role:          role,
		RunID:         runID,
		CommitSHA:     commit,
		GateSourceSHA: gateSourceSHA,
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		StampedAt:     stamped.Format(time.RFC3339Nano),
		Runner:        runner,
		Benchmark:     configured,
	}
	if err := sealBenchmarkResult(rawPath, resultPath, embeddedProvenance(metadata)); err != nil {
		return fmt.Errorf("seal benchmark result: %w", err)
	}
	artifact, err := loadBenchmarkArtifact(resultPath)
	if err != nil {
		return fmt.Errorf("read sealed benchmark result: %w", err)
	}
	metadata.ResultSHA256 = artifact.Digest
	if err := validateProvenance(metadata); err != nil {
		return fmt.Errorf("generated metadata: %w", err)
	}
	if err := validateEmbeddedProvenance(artifact.Data, metadata); err != nil {
		return fmt.Errorf("sealed result provenance: %w", err)
	}
	if err := requireCleanCheckout(repo); err != nil {
		return err
	}
	if err := writeJSONExclusive(metadataPath, metadata); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	fmt.Fprintf(stdout, "%s benchmark stamped: %s (%s)\n", role, commit, metadataPath)
	return nil
}

func runCompare(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var basePolicyPath, candidatePolicyPath, baseRepo, candidateRepo, basePath, candidatePath, baseMetadataPath, candidateMetadataPath string
	var baseSHA, candidateSHA, gateSourceSHA, runID, runnerID, reportPath string
	flags.StringVar(&basePolicyPath, "base-policy", "", "policy applied to the base revision")
	flags.StringVar(&candidatePolicyPath, "candidate-policy", "", "policy applied to the candidate revision")
	flags.StringVar(&baseRepo, "base-repo", "", "clean base worktree")
	flags.StringVar(&candidateRepo, "candidate-repo", "", "clean candidate worktree")
	flags.StringVar(&basePath, "base", "", "base raw benchmark result")
	flags.StringVar(&candidatePath, "candidate", "", "candidate raw benchmark result")
	flags.StringVar(&baseMetadataPath, "base-metadata", "", "base provenance sidecar")
	flags.StringVar(&candidateMetadataPath, "candidate-metadata", "", "candidate provenance sidecar")
	flags.StringVar(&baseSHA, "base-sha", "", "full base commit SHA")
	flags.StringVar(&candidateSHA, "candidate-sha", "", "full candidate commit SHA")
	flags.StringVar(&gateSourceSHA, "gate-source-sha", "", "full trusted gate source SHA")
	flags.StringVar(&runID, "run-id", "", "fresh comparison run identity")
	flags.StringVar(&runnerID, "runner-id", "", "stable dedicated-runner identity")
	flags.StringVar(&reportPath, "report", "", "output gate report")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || basePolicyPath == "" || candidatePolicyPath == "" || baseRepo == "" || candidateRepo == "" || basePath == "" || candidatePath == "" || baseMetadataPath == "" || candidateMetadataPath == "" || baseSHA == "" || candidateSHA == "" || gateSourceSHA == "" || runID == "" || runnerID == "" || reportPath == "" {
		flags.Usage()
		return errUsage
	}
	if err := validateSHA(baseSHA); err != nil {
		return asUsageError(fmt.Errorf("base SHA: %w", err))
	}
	if err := validateSHA(candidateSHA); err != nil {
		return asUsageError(fmt.Errorf("candidate SHA: %w", err))
	}
	if baseSHA == candidateSHA {
		return asUsageError(errors.New("base and candidate SHAs must differ"))
	}
	if err := validateSHA(gateSourceSHA); err != nil {
		return asUsageError(fmt.Errorf("gate source SHA: %w", err))
	}
	if err := validateID("run ID", runID); err != nil {
		return asUsageError(err)
	}
	if err := validateID("runner ID", runnerID); err != nil {
		return asUsageError(err)
	}
	if err := requireAbsent(reportPath); err != nil {
		return fmt.Errorf("report output: %w", err)
	}
	// The orchestrator passes the same externally reviewed file twice so a base
	// may predate the gate. Other callers still cannot compare policy variants.
	basePolicy, basePolicyDigest, err := readPolicy(basePolicyPath)
	if err != nil {
		return fmt.Errorf("read base policy: %w", err)
	}
	_, candidatePolicyDigest, err := readPolicy(candidatePolicyPath)
	if err != nil {
		return fmt.Errorf("read candidate policy: %w", err)
	}
	if basePolicyDigest != candidatePolicyDigest {
		return fmt.Errorf("benchmark policies differ: base=%s candidate=%s", basePolicyDigest, candidatePolicyDigest)
	}
	policy := basePolicy
	policyDigest := basePolicyDigest
	baseRepo, err = filepath.Abs(baseRepo)
	if err != nil {
		return fmt.Errorf("resolve base repository: %w", err)
	}
	candidateRepo, err = filepath.Abs(candidateRepo)
	if err != nil {
		return fmt.Errorf("resolve candidate repository: %w", err)
	}
	if err := validateCheckout(baseRepo, baseSHA); err != nil {
		return fmt.Errorf("base checkout: %w", err)
	}
	if err := validateCheckout(candidateRepo, candidateSHA); err != nil {
		return fmt.Errorf("candidate checkout: %w", err)
	}
	if err := requireAncestor(candidateRepo, baseSHA, candidateSHA); err != nil {
		return err
	}
	// Changed benchmark bodies, harness topology, or dependencies make the two
	// samples incomparable even when their benchmark names still match.
	baseApparatus, err := apparatusDigestForPolicy(baseRepo, policy)
	if err != nil {
		return fmt.Errorf("base measurement apparatus: %w", err)
	}
	candidateApparatus, err := apparatusDigestForPolicy(candidateRepo, policy)
	if err != nil {
		return fmt.Errorf("candidate measurement apparatus: %w", err)
	}
	if baseApparatus != candidateApparatus {
		return fmt.Errorf("measurement apparatus differs: base=%s candidate=%s", baseApparatus, candidateApparatus)
	}
	configured := configFromPolicy(policy, policyDigest, baseApparatus, gateSourceSHA)

	baseMetadata, err := readProvenance(baseMetadataPath)
	if err != nil {
		return fmt.Errorf("read base metadata: %w", err)
	}
	candidateMetadata, err := readProvenance(candidateMetadataPath)
	if err != nil {
		return fmt.Errorf("read candidate metadata: %w", err)
	}
	if err := validateProvenancePair(baseMetadata, candidateMetadata, baseSHA, candidateSHA, runID, runnerID, configured, nowUTC()); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}

	baseArtifact, err := loadBenchmarkArtifact(basePath)
	if err != nil {
		return fmt.Errorf("read base result: %w", err)
	}
	candidateArtifact, err := loadBenchmarkArtifact(candidatePath)
	if err != nil {
		return fmt.Errorf("read candidate result: %w", err)
	}
	if baseArtifact.Digest != baseMetadata.ResultSHA256 {
		return fmt.Errorf("base result digest is %s, metadata names %s", baseArtifact.Digest, baseMetadata.ResultSHA256)
	}
	if candidateArtifact.Digest != candidateMetadata.ResultSHA256 {
		return fmt.Errorf("candidate result digest is %s, metadata names %s", candidateArtifact.Digest, candidateMetadata.ResultSHA256)
	}
	if err := validateEmbeddedProvenance(baseArtifact.Data, baseMetadata); err != nil {
		return fmt.Errorf("base embedded provenance: %w", err)
	}
	if err := validateEmbeddedProvenance(candidateArtifact.Data, candidateMetadata); err != nil {
		return fmt.Errorf("candidate embedded provenance: %w", err)
	}
	if err := validateRawEnvironment(baseArtifact.Data.Environment, baseMetadata.Runner); err != nil {
		return fmt.Errorf("base result environment: %w", err)
	}
	if err := validateRawEnvironment(candidateArtifact.Data.Environment, candidateMetadata.Runner); err != nil {
		return fmt.Errorf("candidate result environment: %w", err)
	}
	if err := validateExpectedBenchmarkSet(baseArtifact.Data, policy); err != nil {
		return fmt.Errorf("base benchmark set: %w", err)
	}
	if err := validateExpectedBenchmarkSet(candidateArtifact.Data, policy); err != nil {
		return fmt.Errorf("candidate benchmark set: %w", err)
	}
	baseMetadataDigest, err := regularFileDigest(baseMetadataPath, maximumMetadataBytes)
	if err != nil {
		return fmt.Errorf("hash base metadata: %w", err)
	}
	candidateMetadataDigest, err := regularFileDigest(candidateMetadataPath, maximumMetadataBytes)
	if err != nil {
		return fmt.Errorf("hash candidate metadata: %w", err)
	}

	comparisons, err := compareBenchmarkData(baseArtifact.Data, candidateArtifact.Data, policy.Count, regressionPercent)
	if err != nil {
		return err
	}
	regressions := comparisonRegressions(comparisons)
	report := gateReport{
		SchemaVersion:           reportSchemaVersion,
		RunID:                   runID,
		BaseSHA:                 baseSHA,
		CandidateSHA:            candidateSHA,
		GateSourceSHA:           gateSourceSHA,
		BaseResultSHA256:        baseArtifact.Digest,
		CandidateResultSHA256:   candidateArtifact.Digest,
		BaseMetadataSHA256:      baseMetadataDigest,
		CandidateMetadataSHA256: candidateMetadataDigest,
		GeneratedAt:             nowUTC().Format(time.RFC3339Nano),
		MinimumSamples:          minimumSamples,
		RegressionPercent:       regressionPercent,
		Runner:                  baseMetadata.Runner,
		Benchmark:               configured,
		Passed:                  regressions == 0,
		RegressionCount:         regressions,
		Comparisons:             comparisons,
	}
	if err := validateGateReport(report); err != nil {
		return fmt.Errorf("generated gate report: %w", err)
	}
	if err := writeJSONExclusive(reportPath, report); err != nil {
		return fmt.Errorf("write gate report: %w", err)
	}
	printComparisons(stdout, comparisons)
	if regressions > 0 {
		return fmt.Errorf("%w: %d metric(s) exceeded %.2f%%", errRegression, regressions, regressionPercent)
	}
	fmt.Fprintf(stdout, "benchmark gate passed: %d benchmarks, %d metrics, median regression limit %.2f%%\n",
		len(comparisons)/len(requiredMetrics), len(comparisons), regressionPercent)
	return nil
}

func runVerifyReport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var reportPath, policyPath, baseResultPath, candidateResultPath, baseMetadataPath, candidateMetadataPath string
	var baseSHA, candidateSHA, gateSourceSHA, policyDigest, apparatusDigest, runID, runnerID, expect string
	flags.StringVar(&reportPath, "report", "", "published gate report")
	flags.StringVar(&policyPath, "policy", "", "reviewed benchmark policy")
	flags.StringVar(&baseResultPath, "base-result", "", "sealed base benchmark result")
	flags.StringVar(&candidateResultPath, "candidate-result", "", "sealed candidate benchmark result")
	flags.StringVar(&baseMetadataPath, "base-metadata", "", "base provenance sidecar")
	flags.StringVar(&candidateMetadataPath, "candidate-metadata", "", "candidate provenance sidecar")
	flags.StringVar(&baseSHA, "base-sha", "", "full expected base commit SHA")
	flags.StringVar(&candidateSHA, "candidate-sha", "", "full expected candidate commit SHA")
	flags.StringVar(&gateSourceSHA, "gate-source-sha", "", "full trusted gate source SHA")
	flags.StringVar(&policyDigest, "policy-sha256", "", "expected reviewed policy digest")
	flags.StringVar(&apparatusDigest, "apparatus-sha256", "", "expected measured apparatus digest")
	flags.StringVar(&runID, "run-id", "", "expected comparison run identity")
	flags.StringVar(&runnerID, "runner-id", "", "expected dedicated-runner identity")
	flags.StringVar(&expect, "expect", "", "expected gate outcome: pass or regression")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || reportPath == "" || policyPath == "" || baseResultPath == "" || candidateResultPath == "" || baseMetadataPath == "" || candidateMetadataPath == "" || baseSHA == "" || candidateSHA == "" || gateSourceSHA == "" || policyDigest == "" || apparatusDigest == "" || runID == "" || runnerID == "" || expect == "" {
		flags.Usage()
		return errUsage
	}
	if err := validateSHA(baseSHA); err != nil {
		return asUsageError(fmt.Errorf("base SHA: %w", err))
	}
	if err := validateSHA(candidateSHA); err != nil {
		return asUsageError(fmt.Errorf("candidate SHA: %w", err))
	}
	if baseSHA == candidateSHA {
		return asUsageError(errors.New("base and candidate SHAs must differ"))
	}
	if err := validateSHA(gateSourceSHA); err != nil {
		return asUsageError(fmt.Errorf("gate source SHA: %w", err))
	}
	if err := validateDigest(policyDigest); err != nil {
		return asUsageError(fmt.Errorf("policy SHA-256: %w", err))
	}
	if err := validateDigest(apparatusDigest); err != nil {
		return asUsageError(fmt.Errorf("apparatus SHA-256: %w", err))
	}
	if err := validateID("run ID", runID); err != nil {
		return asUsageError(err)
	}
	if err := validateID("runner ID", runnerID); err != nil {
		return asUsageError(err)
	}
	if expect != "pass" && expect != "regression" {
		return asUsageError(errors.New("expect must be pass or regression"))
	}
	policy, actualPolicyDigest, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read reviewed policy: %w", err)
	}
	if actualPolicyDigest != policyDigest {
		return fmt.Errorf("reviewed policy SHA-256 is %s, want %s", actualPolicyDigest, policyDigest)
	}

	report, err := readGateReport(reportPath)
	if err != nil {
		return fmt.Errorf("read gate report: %w", err)
	}
	// Recompute the report's decision and bind its inventory, configuration, and
	// exact artifact bytes to independently supplied policy and run identities.
	if err := validateGateReport(report); err != nil {
		return fmt.Errorf("invalid gate report: %w", err)
	}
	if report.BaseSHA != baseSHA || report.CandidateSHA != candidateSHA || report.RunID != runID {
		return fmt.Errorf("gate report identity is base=%s candidate=%s run=%q, want %s %s %q",
			report.BaseSHA, report.CandidateSHA, report.RunID, baseSHA, candidateSHA, runID)
	}
	if report.Runner.ID != runnerID {
		return fmt.Errorf("gate report runner ID is %q, want %q", report.Runner.ID, runnerID)
	}
	if report.GateSourceSHA != gateSourceSHA {
		return fmt.Errorf("gate report source SHA is %s, want %s", report.GateSourceSHA, gateSourceSHA)
	}
	if report.Benchmark.PolicySHA256 != policyDigest {
		return fmt.Errorf("gate report policy SHA-256 is %s, want %s", report.Benchmark.PolicySHA256, policyDigest)
	}
	if report.Benchmark.ApparatusSHA256 != apparatusDigest {
		return fmt.Errorf("gate report apparatus SHA-256 is %s, want %s", report.Benchmark.ApparatusSHA256, apparatusDigest)
	}
	expectedConfig := configFromPolicy(policy, policyDigest, apparatusDigest, gateSourceSHA)
	if report.Benchmark != expectedConfig {
		return fmt.Errorf("gate report benchmark configuration does not match the reviewed policy")
	}
	if err := validateReportBenchmarkSet(report.Comparisons, policy); err != nil {
		return fmt.Errorf("gate report benchmark set: %w", err)
	}
	artifacts := []struct {
		name     string
		path     string
		maximum  int64
		reported string
	}{
		{name: "base result", path: baseResultPath, maximum: maxBenchmarkBytes, reported: report.BaseResultSHA256},
		{name: "candidate result", path: candidateResultPath, maximum: maxBenchmarkBytes, reported: report.CandidateResultSHA256},
		{name: "base metadata", path: baseMetadataPath, maximum: maximumMetadataBytes, reported: report.BaseMetadataSHA256},
		{name: "candidate metadata", path: candidateMetadataPath, maximum: maximumMetadataBytes, reported: report.CandidateMetadataSHA256},
	}
	for _, artifact := range artifacts {
		digest, err := regularFileDigest(artifact.path, artifact.maximum)
		if err != nil {
			return fmt.Errorf("hash %s: %w", artifact.name, err)
		}
		if digest != artifact.reported {
			return fmt.Errorf("gate report %s SHA-256 is %s, artifact is %s", artifact.name, artifact.reported, digest)
		}
	}
	wantPassed := expect == "pass"
	if report.Passed != wantPassed {
		return fmt.Errorf("gate report passed=%t is inconsistent with expected %s outcome", report.Passed, expect)
	}
	if _, err := fmt.Fprintf(stdout, "gate report verified: %s\n", expect); err != nil {
		return fmt.Errorf("write verification result: %w", err)
	}
	return nil
}

func readGateReport(filename string) (gateReport, error) {
	contents, err := readRegularFile(filename, maximumMetadataBytes)
	if err != nil {
		return gateReport{}, err
	}
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return gateReport{}, err
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictJSON(contents, &fields); err != nil {
		return gateReport{}, err
	}
	for _, key := range []string{
		"schema_version", "run_id", "base_sha", "candidate_sha", "gate_source_sha", "generated_at",
		"base_result_sha256", "candidate_result_sha256", "base_metadata_sha256", "candidate_metadata_sha256",
		"minimum_samples", "regression_percent", "runner", "benchmark", "passed", "regression_count", "comparisons",
	} {
		if _, ok := fields[key]; !ok {
			return gateReport{}, fmt.Errorf("gate report omits required field %q", key)
		}
	}
	var comparisonFields []map[string]json.RawMessage
	if err := decodeStrictJSON(fields["comparisons"], &comparisonFields); err != nil {
		return gateReport{}, fmt.Errorf("comparisons: %w", err)
	}
	for index, comparison := range comparisonFields {
		for _, key := range []string{"benchmark", "metric", "blocking", "base_samples", "candidate_samples", "base_median", "candidate_median", "regression"} {
			if _, ok := comparison[key]; !ok {
				return gateReport{}, fmt.Errorf("comparisons[%d] omits required field %q", index, key)
			}
		}
	}
	var report gateReport
	if err := decodeStrictJSON(contents, &report); err != nil {
		return gateReport{}, err
	}
	return report, nil
}

func regularFileDigest(filename string, maximum int64) (string, error) {
	contents, err := readRegularFile(filename, maximum)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), nil
}

func validateReportBenchmarkSet(comparisons []metricComparison, policy benchmarkPolicy) error {
	prefix := policy.Package + ":Benchmark"
	data := benchmarkData{Samples: make(map[benchmarkKey]map[string][]float64)}
	for _, comparison := range comparisons {
		if !strings.HasPrefix(comparison.Benchmark, prefix) {
			return fmt.Errorf("comparison benchmark %q does not use package %q", comparison.Benchmark, policy.Package)
		}
		name := strings.TrimPrefix(comparison.Benchmark, policy.Package+":Benchmark")
		data.Samples[benchmarkKey{Package: policy.Package, Name: name}] = nil
	}
	return validateExpectedBenchmarkSet(data, policy)
}

func validateGateReport(report gateReport) error {
	if report.SchemaVersion != reportSchemaVersion {
		return fmt.Errorf("schema_version is %d, want %d", report.SchemaVersion, reportSchemaVersion)
	}
	if err := validateID("run ID", report.RunID); err != nil {
		return err
	}
	if err := validateSHA(report.BaseSHA); err != nil {
		return fmt.Errorf("base_sha: %w", err)
	}
	if err := validateSHA(report.CandidateSHA); err != nil {
		return fmt.Errorf("candidate_sha: %w", err)
	}
	if report.BaseSHA == report.CandidateSHA {
		return errors.New("base_sha and candidate_sha must differ")
	}
	if err := validateSHA(report.GateSourceSHA); err != nil {
		return fmt.Errorf("gate_source_sha: %w", err)
	}
	for _, artifact := range []struct {
		name   string
		digest string
	}{
		{name: "base_result_sha256", digest: report.BaseResultSHA256},
		{name: "candidate_result_sha256", digest: report.CandidateResultSHA256},
		{name: "base_metadata_sha256", digest: report.BaseMetadataSHA256},
		{name: "candidate_metadata_sha256", digest: report.CandidateMetadataSHA256},
	} {
		if err := validateDigest(artifact.digest); err != nil {
			return fmt.Errorf("%s: %w", artifact.name, err)
		}
	}
	if _, err := parseUTCTimestamp(report.GeneratedAt); err != nil {
		return fmt.Errorf("generated_at: %w", err)
	}
	if report.MinimumSamples != minimumSamples {
		return fmt.Errorf("minimum_samples is %d, want %d", report.MinimumSamples, minimumSamples)
	}
	if report.RegressionPercent != regressionPercent {
		return fmt.Errorf("regression_percent is %v, want %v", report.RegressionPercent, regressionPercent)
	}
	if err := validateRunner(report.Runner); err != nil {
		return err
	}
	if err := validateBenchmarkConfig(report.Benchmark); err != nil {
		return err
	}
	if report.Benchmark.Count < report.MinimumSamples {
		return fmt.Errorf("benchmark count is %d, below report minimum %d", report.Benchmark.Count, report.MinimumSamples)
	}
	if report.Benchmark.GateSourceSHA != report.GateSourceSHA {
		return fmt.Errorf("benchmark gate_source_sha is %s, want report value %s", report.Benchmark.GateSourceSHA, report.GateSourceSHA)
	}
	if err := validateReportComparisons(report.Comparisons, report.Benchmark.Count); err != nil {
		return err
	}
	regressions := comparisonRegressions(report.Comparisons)
	if report.RegressionCount != regressions {
		return fmt.Errorf("regression_count is %d, comparisons contain %d regressions", report.RegressionCount, regressions)
	}
	if report.Passed != (regressions == 0) {
		return fmt.Errorf("passed=%t is inconsistent with %d comparison regressions", report.Passed, regressions)
	}
	return nil
}

func validateReportComparisons(comparisons []metricComparison, sampleCount int) error {
	if len(comparisons) == 0 || len(comparisons)%len(requiredMetrics) != 0 {
		return fmt.Errorf("comparisons contain %d entries, want a non-empty multiple of %d", len(comparisons), len(requiredMetrics))
	}
	previousBenchmark := ""
	for index, comparison := range comparisons {
		metricIndex := index % len(requiredMetrics)
		expectedMetric := displayMetric(requiredMetrics[metricIndex])
		if comparison.Metric != expectedMetric {
			return fmt.Errorf("comparisons[%d].metric is %q, want %q", index, comparison.Metric, expectedMetric)
		}
		expectedBlocking := requiredMetrics[metricIndex] != "B/op"
		if comparison.Blocking != expectedBlocking {
			return fmt.Errorf("comparisons[%d].blocking is %t, want %t", index, comparison.Blocking, expectedBlocking)
		}
		if comparison.Benchmark == "" || len(comparison.Benchmark) > 1024 || !utf8.ValidString(comparison.Benchmark) || strings.ContainsAny(comparison.Benchmark, "\x00\r\n") {
			return fmt.Errorf("comparisons[%d].benchmark is empty or invalid", index)
		}
		if metricIndex == 0 {
			if previousBenchmark != "" && comparison.Benchmark <= previousBenchmark {
				return errors.New("comparison benchmark groups must be strictly sorted and unique")
			}
			previousBenchmark = comparison.Benchmark
		} else if comparison.Benchmark != previousBenchmark {
			return fmt.Errorf("comparisons[%d].benchmark is %q, want group benchmark %q", index, comparison.Benchmark, previousBenchmark)
		}
		if err := validateReportSamples(index, "base", comparison.BaseSamples, sampleCount, expectedMetric == "time/op"); err != nil {
			return err
		}
		if err := validateReportSamples(index, "candidate", comparison.CandidateSamples, sampleCount, expectedMetric == "time/op"); err != nil {
			return err
		}
		baseMedian := median(comparison.BaseSamples)
		candidateMedian := median(comparison.CandidateSamples)
		if comparison.BaseMedian != baseMedian || comparison.CandidateMedian != candidateMedian {
			return fmt.Errorf("comparisons[%d] medians are %v/%v, samples produce %v/%v", index,
				comparison.BaseMedian, comparison.CandidateMedian, baseMedian, candidateMedian)
		}
		var expectedDelta *float64
		if baseMedian != 0 {
			delta := (candidateMedian/baseMedian - 1) * 100
			if !math.IsNaN(delta) && !math.IsInf(delta, 0) {
				expectedDelta = &delta
			}
		}
		if (comparison.DeltaPercent == nil) != (expectedDelta == nil) ||
			comparison.DeltaPercent != nil && *comparison.DeltaPercent != *expectedDelta {
			return fmt.Errorf("comparisons[%d].delta_percent is %s, samples produce %s", index, optionalFloat(comparison.DeltaPercent), optionalFloat(expectedDelta))
		}
		expectedRegression := false
		if expectedBlocking {
			if expectedMetric == "allocs/op" && allZero(comparison.BaseSamples) {
				expectedRegression = anyPositive(comparison.CandidateSamples)
			} else {
				if baseMedian == 0 {
					return fmt.Errorf("comparisons[%d] has a zero median without an all-zero allocation baseline", index)
				}
				expectedRegression = exceedsRegressionThreshold(baseMedian, candidateMedian, regressionPercent)
			}
		}
		if comparison.Regression != expectedRegression {
			return fmt.Errorf("comparisons[%d].regression is %t, samples require %t", index, comparison.Regression, expectedRegression)
		}
	}
	return nil
}

func optionalFloat(value *float64) string {
	if value == nil {
		return "<omitted>"
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

func validateReportSamples(index int, role string, samples []float64, count int, requirePositive bool) error {
	if len(samples) != count {
		return fmt.Errorf("comparisons[%d].%s_samples has %d samples, want %d", index, role, len(samples), count)
	}
	for sampleIndex, value := range samples {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || requirePositive && value == 0 {
			return fmt.Errorf("comparisons[%d].%s_samples[%d] is negative, non-finite, or invalidly zero", index, role, sampleIndex)
		}
	}
	return nil
}

func validateProvenancePair(base, candidate resultProvenance, baseSHA, candidateSHA, runID, runnerID string, configured benchmarkConfig, now time.Time) error {
	if err := validateProvenance(base); err != nil {
		return fmt.Errorf("base metadata: %w", err)
	}
	if err := validateProvenance(candidate); err != nil {
		return fmt.Errorf("candidate metadata: %w", err)
	}
	if base.Role != "base" || candidate.Role != "candidate" {
		return fmt.Errorf("roles are base=%q candidate=%q", base.Role, candidate.Role)
	}
	if base.CommitSHA != baseSHA || candidate.CommitSHA != candidateSHA {
		return fmt.Errorf("commit mismatch: base=%s candidate=%s, want %s and %s", base.CommitSHA, candidate.CommitSHA, baseSHA, candidateSHA)
	}
	if base.RunID != runID || candidate.RunID != runID {
		return fmt.Errorf("run ID mismatch: base=%q candidate=%q, want %q", base.RunID, candidate.RunID, runID)
	}
	if base.Runner.ID != runnerID || candidate.Runner.ID != runnerID {
		return fmt.Errorf("runner ID mismatch: base=%q candidate=%q, want %q", base.Runner.ID, candidate.Runner.ID, runnerID)
	}
	if base.Benchmark != configured || candidate.Benchmark != configured {
		return fmt.Errorf("benchmark configuration mismatch: base=%+v candidate=%+v, want %+v", base.Benchmark, candidate.Benchmark, configured)
	}
	// Runner identity includes the boot and CPU constraints. This prevents a
	// nominal runner label from joining measurements taken in different regimes.
	if base.Runner != candidate.Runner {
		return fmt.Errorf("runner provenance differs: base=%+v candidate=%+v", base.Runner, candidate.Runner)
	}
	expectedRunner, err := currentRunner(runnerID, base.Runner.CPU)
	if err != nil {
		return err
	}
	if base.Runner != expectedRunner {
		return fmt.Errorf("comparison runtime differs from stamped runner: stamped=%+v current=%+v", base.Runner, expectedRunner)
	}
	baseTimes, err := parseProvenanceTimes(base)
	if err != nil {
		return fmt.Errorf("base metadata: %w", err)
	}
	candidateTimes, err := parseProvenanceTimes(candidate)
	if err != nil {
		return fmt.Errorf("candidate metadata: %w", err)
	}
	oldest := now.Add(-maximumArtifactAge)
	latest := now.Add(maximumFutureSkew)
	if baseTimes.Started.Before(oldest) || candidateTimes.Started.Before(oldest) {
		return fmt.Errorf("benchmark artifacts are older than %s", maximumArtifactAge)
	}
	if baseTimes.Stamped.After(latest) || candidateTimes.Stamped.After(latest) {
		return fmt.Errorf("benchmark artifact timestamp is more than %s in the future", maximumFutureSkew)
	}
	if !baseTimes.Finished.After(baseTimes.Started) || baseTimes.Stamped.Before(baseTimes.Finished) || baseTimes.Stamped.Sub(baseTimes.Finished) > maximumFutureSkew {
		return errors.New("base run timestamps are not ordered or were not stamped immediately")
	}
	if !candidateTimes.Finished.After(candidateTimes.Started) || candidateTimes.Stamped.Before(candidateTimes.Finished) || candidateTimes.Stamped.Sub(candidateTimes.Finished) > maximumFutureSkew {
		return errors.New("candidate run timestamps are not ordered or were not stamped immediately")
	}
	// Non-overlap removes simultaneous contention. It does not remove residual
	// base-first thermal or cache effects, which the fixed-runner procedure must
	// control by quiescing and stabilizing the host before a deciding run.
	if baseTimes.Finished.After(candidateTimes.Started) {
		return fmt.Errorf("base and candidate runs overlap: base finished %s, candidate started %s", base.FinishedAt, candidate.StartedAt)
	}
	if candidateTimes.Stamped.Sub(baseTimes.Started) > maximumArtifactAge {
		return fmt.Errorf("base/candidate artifact span exceeds %s", maximumArtifactAge)
	}
	return nil
}

func parseProvenanceTimes(metadata resultProvenance) (provenanceTimes, error) {
	started, err := parseUTCTimestamp(metadata.StartedAt)
	if err != nil {
		return provenanceTimes{}, fmt.Errorf("started_at: %w", err)
	}
	finished, err := parseUTCTimestamp(metadata.FinishedAt)
	if err != nil {
		return provenanceTimes{}, fmt.Errorf("finished_at: %w", err)
	}
	stamped, err := parseUTCTimestamp(metadata.StampedAt)
	if err != nil {
		return provenanceTimes{}, fmt.Errorf("stamped_at: %w", err)
	}
	return provenanceTimes{Started: started, Finished: finished, Stamped: stamped}, nil
}

func validateRawEnvironment(raw benchmarkEnvironment, runner runnerProvenance) error {
	if raw.GOOS != runner.GOOS || raw.GOARCH != runner.GOARCH || raw.CPU != runner.CPU {
		return fmt.Errorf("raw result is %s/%s %q, metadata is %s/%s %q",
			raw.GOOS, raw.GOARCH, raw.CPU, runner.GOOS, runner.GOARCH, runner.CPU)
	}
	return nil
}

func embeddedProvenance(metadata resultProvenance) map[string]string {
	return map[string]string{
		"bench-role":             metadata.Role,
		"bench-run-id":           metadata.RunID,
		"bench-commit":           metadata.CommitSHA,
		"bench-gate-source-sha":  metadata.GateSourceSHA,
		"bench-started-at":       metadata.StartedAt,
		"bench-finished-at":      metadata.FinishedAt,
		"bench-stamped-at":       metadata.StampedAt,
		"bench-runner-id":        metadata.Runner.ID,
		"bench-go-version":       metadata.Runner.GoVersion,
		"bench-num-cpu":          strconv.Itoa(metadata.Runner.NumCPU),
		"bench-gomaxprocs":       strconv.Itoa(metadata.Runner.GOMAXPROCS),
		"bench-cpu-fingerprint":  metadata.Runner.CPUFingerprint,
		"bench-machine-id":       metadata.Runner.MachineIDSHA256,
		"bench-boot-id":          metadata.Runner.BootIDSHA256,
		"bench-kernel":           metadata.Runner.KernelRelease,
		"bench-cpu-constraints":  metadata.Runner.CPUConstraintsSHA256,
		"bench-goamd64":          embeddedValue(metadata.Runner.GOAMD64),
		"bench-goexperiment":     embeddedValue(metadata.Runner.GOEXPERIMENT),
		"bench-cgo-enabled":      embeddedValue(metadata.Runner.CGOEnabled),
		"bench-goflags":          embeddedValue(metadata.Runner.GOFLAGS),
		"bench-godebug":          embeddedValue(metadata.Runner.GODEBUG),
		"bench-gogc":             embeddedValue(metadata.Runner.GOGC),
		"bench-gomemlimit":       embeddedValue(metadata.Runner.GOMEMLIMIT),
		"bench-gotoolchain":      embeddedValue(metadata.Runner.GOTOOLCHAIN),
		"bench-docker":           metadata.Runner.DockerVersion,
		"bench-compose":          metadata.Runner.DockerComposeVersion,
		"bench-profile":          metadata.Benchmark.Profile,
		"bench-package":          metadata.Benchmark.Package,
		"bench-pattern":          metadata.Benchmark.Pattern,
		"bench-benchtime":        metadata.Benchmark.Benchtime,
		"bench-count":            strconv.Itoa(metadata.Benchmark.Count),
		"bench-timeout":          metadata.Benchmark.Timeout,
		"bench-policy-sha256":    metadata.Benchmark.PolicySHA256,
		"bench-apparatus-sha256": metadata.Benchmark.ApparatusSHA256,
	}
}

func embeddedValue(value string) string {
	return value
}

func artifactHasEmbeddedProvenance(data benchmarkData) bool {
	for _, value := range data.Embedded {
		if value != "" {
			return true
		}
	}
	return false
}

func validateEmbeddedProvenance(data benchmarkData, metadata resultProvenance) error {
	expected := embeddedProvenance(metadata)
	for _, key := range embeddedProvenanceKeys {
		if data.Embedded[key] != expected[key] {
			return fmt.Errorf("%s is %q, want %q", key, data.Embedded[key], expected[key])
		}
	}
	return nil
}

func validateProvenance(metadata resultProvenance) error {
	if metadata.SchemaVersion != metadataSchemaVersion {
		return fmt.Errorf("schema_version is %d, want %d", metadata.SchemaVersion, metadataSchemaVersion)
	}
	if metadata.Role != "base" && metadata.Role != "candidate" {
		return fmt.Errorf("invalid role %q", metadata.Role)
	}
	if err := validateID("run ID", metadata.RunID); err != nil {
		return err
	}
	if err := validateSHA(metadata.CommitSHA); err != nil {
		return fmt.Errorf("commit_sha: %w", err)
	}
	if err := validateSHA(metadata.GateSourceSHA); err != nil {
		return fmt.Errorf("gate_source_sha: %w", err)
	}
	if err := validateDigest(metadata.ResultSHA256); err != nil {
		return fmt.Errorf("result_sha256: %w", err)
	}
	for name, value := range map[string]string{"started_at": metadata.StartedAt, "finished_at": metadata.FinishedAt, "stamped_at": metadata.StampedAt} {
		if _, err := parseUTCTimestamp(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := validateRunner(metadata.Runner); err != nil {
		return err
	}
	if err := validateBenchmarkConfig(metadata.Benchmark); err != nil {
		return err
	}
	if metadata.GateSourceSHA != metadata.Benchmark.GateSourceSHA {
		return fmt.Errorf("gate_source_sha is %s, benchmark names %s", metadata.GateSourceSHA, metadata.Benchmark.GateSourceSHA)
	}
	return nil
}

func validateRunner(runner runnerProvenance) error {
	if err := validateID("runner ID", runner.ID); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"go_version": runner.GoVersion, "goos": runner.GOOS, "goarch": runner.GOARCH,
		"cpu": runner.CPU, "kernel_release": runner.KernelRelease,
		"cgo_enabled": runner.CGOEnabled, "docker_version": runner.DockerVersion,
		"docker_compose_version": runner.DockerComposeVersion,
	} {
		if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("runner %s is empty or invalid", name)
		}
	}
	if runner.NumCPU <= 0 || runner.GOMAXPROCS <= 0 {
		return fmt.Errorf("runner CPU counts must be positive: num_cpu=%d gomaxprocs=%d", runner.NumCPU, runner.GOMAXPROCS)
	}
	if err := validateDigest(runner.CPUFingerprint); err != nil {
		return fmt.Errorf("cpu_fingerprint: %w", err)
	}
	for name, value := range map[string]string{
		"machine_id_sha256":      runner.MachineIDSHA256,
		"boot_id_sha256":         runner.BootIDSHA256,
		"cpu_constraints_sha256": runner.CPUConstraintsSHA256,
	} {
		if err := validateDigest(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"goamd64": runner.GOAMD64, "goexperiment": runner.GOEXPERIMENT, "goflags": runner.GOFLAGS,
		"godebug": runner.GODEBUG, "gogc": runner.GOGC, "gomemlimit": runner.GOMEMLIMIT,
		"gotoolchain": runner.GOTOOLCHAIN,
	} {
		if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("runner %s is empty or invalid", name)
		}
	}
	return nil
}

func validateBenchmarkConfig(configured benchmarkConfig) error {
	if err := validateID("benchmark profile", configured.Profile); err != nil {
		return err
	}
	if configured.Package == "" || !utf8.ValidString(configured.Package) || strings.ContainsAny(configured.Package, "\x00\r\n") {
		return errors.New("benchmark package is empty or invalid")
	}
	if configured.Pattern == "" || len(configured.Pattern) > 1024 || !utf8.ValidString(configured.Pattern) || strings.ContainsAny(configured.Pattern, "\x00\r\n") {
		return errors.New("benchmark pattern must be a non-empty single-line UTF-8 value of at most 1024 bytes")
	}
	if err := validateBenchtime(configured.Benchtime); err != nil {
		return err
	}
	if configured.Count < 1 || configured.Count > maximumSamples {
		return fmt.Errorf("benchmark count must be between 1 and %d", maximumSamples)
	}
	if err := validateDuration("benchmark timeout", configured.Timeout); err != nil {
		return err
	}
	if err := validateDigest(configured.PolicySHA256); err != nil {
		return fmt.Errorf("policy_sha256: %w", err)
	}
	if err := validateDigest(configured.ApparatusSHA256); err != nil {
		return fmt.Errorf("apparatus_sha256: %w", err)
	}
	if err := validateSHA(configured.GateSourceSHA); err != nil {
		return fmt.Errorf("gate_source_sha: %w", err)
	}
	return nil
}

func validateDuration(name, value string) error {
	if !durationTime.MatchString(value) {
		return fmt.Errorf("%s must be a positive integer-component Go duration: %q", name, value)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("%s must be positive and finite: %q", name, value)
	}
	return nil
}

func validateBenchtime(value string) error {
	if iterationTime.MatchString(value) {
		iterations, err := strconv.Atoi(strings.TrimSuffix(value, "x"))
		if err == nil && iterations >= 1 && iterations <= 1_000_000 {
			return nil
		}
	}
	if !durationTime.MatchString(value) {
		return fmt.Errorf("benchmark time must be 1x..1000000x or a positive integer-component Go duration: %q", value)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("benchmark time must be positive and finite: %q", value)
	}
	return nil
}

func validateID(name, value string) error {
	if !idPattern.MatchString(value) {
		return fmt.Errorf("%s must match %s", name, idPattern)
	}
	return nil
}

func validateSHA(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return errors.New("must be a full 40- or 64-character lowercase hexadecimal object ID")
	}
	for _, char := range value {
		if char < '0' || (char > '9' && char < 'a') || char > 'f' {
			return errors.New("must be a full 40- or 64-character lowercase hexadecimal object ID")
		}
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != 64 {
		return errors.New("must be 64 lowercase hexadecimal characters")
	}
	for _, char := range value {
		if char < '0' || (char > '9' && char < 'a') || char > 'f' {
			return errors.New("must be 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

func parseUTCTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	_, offset := parsed.Zone()
	if offset != 0 || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("must be a canonical UTC RFC3339 timestamp")
	}
	return parsed, nil
}

func readRunnerProbe(filename, runnerID string) (runnerProvenance, error) {
	var probe runnerProvenance
	if err := readStrictJSON(filename, &probe); err != nil {
		return runnerProvenance{}, err
	}
	if err := validateRunner(probe); err != nil {
		return runnerProvenance{}, err
	}
	if probe.ID != runnerID {
		return runnerProvenance{}, fmt.Errorf("runner ID is %q, want %q", probe.ID, runnerID)
	}
	if probe.CPU != "<probe>" {
		return runnerProvenance{}, fmt.Errorf("CPU is %q, want probe sentinel", probe.CPU)
	}
	return probe, nil
}

func validateRunnerProbe(probe, observed runnerProvenance) error {
	expected := observed
	// CPU comes from the benchmark artifact and is bound by validateRawEnvironment.
	// Project only that value to the probe sentinel; every runtime field must match.
	expected.CPU = "<probe>"
	if probe != expected {
		return fmt.Errorf("runner state differs after harness: probe=%+v current=%+v", probe, expected)
	}
	return nil
}

func currentRunner(id, cpu string) (runnerProvenance, error) {
	// BENCH_RUNNER_ID is only a label. Bind it to observable machine, boot,
	// runtime, cgroup, and container-tool state before trusting a comparison.
	fingerprint, err := fingerprintCPU()
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("fingerprint CPU: %w", err)
	}
	machineID, err := hashRequiredFile("/etc/machine-id")
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("machine identity: %w", err)
	}
	bootID, err := hashRequiredFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("boot identity: %w", err)
	}
	kernel, err := readSingleLine("/proc/sys/kernel/osrelease")
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("kernel release: %w", err)
	}
	constraints, err := cpuConstraintsFingerprint()
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("CPU constraints: %w", err)
	}
	goEnvironment, err := readGoEnvironment()
	if err != nil {
		return runnerProvenance{}, err
	}
	dockerVersion, err := commandSingleLine("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("docker version: %w", err)
	}
	composeVersion, err := commandSingleLine("docker", "compose", "version", "--short")
	if err != nil {
		return runnerProvenance{}, fmt.Errorf("docker compose version: %w", err)
	}
	runner := runnerProvenance{
		ID:                   id,
		GoVersion:            runtime.Version(),
		GOOS:                 runtime.GOOS,
		GOARCH:               runtime.GOARCH,
		NumCPU:               runtime.NumCPU(),
		GOMAXPROCS:           runtime.GOMAXPROCS(0),
		CPU:                  cpu,
		CPUFingerprint:       fingerprint,
		MachineIDSHA256:      machineID,
		BootIDSHA256:         bootID,
		KernelRelease:        kernel,
		CPUConstraintsSHA256: constraints,
		GOAMD64:              recordedValue(goEnvironment["GOAMD64"]),
		GOEXPERIMENT:         recordedValue(goEnvironment["GOEXPERIMENT"]),
		CGOEnabled:           recordedValue(goEnvironment["CGO_ENABLED"]),
		GOFLAGS:              recordedValue(goEnvironment["GOFLAGS"]),
		GODEBUG:              recordedValue(os.Getenv("GODEBUG")),
		GOGC:                 recordedValue(os.Getenv("GOGC")),
		GOMEMLIMIT:           recordedValue(os.Getenv("GOMEMLIMIT")),
		GOTOOLCHAIN:          recordedValue(goEnvironment["GOTOOLCHAIN"]),
		DockerVersion:        dockerVersion,
		DockerComposeVersion: composeVersion,
	}
	if err := validateRunner(runner); err != nil {
		return runnerProvenance{}, err
	}
	return runner, nil
}

func recordedValue(value string) string {
	if value == "" {
		return "<empty>"
	}
	return value
}

func hashRequiredFile(filename string) (string, error) {
	value, err := readSingleLine(filename)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:]), nil
}

func readSingleLine(filename string) (string, error) {
	contents, err := readRegularFile(filename, maximumMetadataBytes)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("value is empty or multiline")
	}
	return value, nil
}

func commandSingleLine(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	value := strings.TrimSpace(string(output))
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("unexpected output %q", value)
	}
	return value, nil
}

func readGoEnvironment() (map[string]string, error) {
	keys := []string{"GOAMD64", "GOEXPERIMENT", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN"}
	args := append([]string{"env", "-json"}, keys...)
	command := exec.Command("go", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := rejectDuplicateJSONKeys(output); err != nil {
		return nil, fmt.Errorf("go env JSON: %w", err)
	}
	var values map[string]string
	if err := decodeStrictJSON(output, &values); err != nil {
		return nil, fmt.Errorf("go env JSON: %w", err)
	}
	if len(values) != len(keys) {
		return nil, fmt.Errorf("go env returned %d fields, want %d", len(values), len(keys))
	}
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("go env omitted %s", key)
		}
	}
	return values, nil
}

func cpuConstraintsFingerprint() (string, error) {
	return cpuConstraintsFingerprintFiles("/proc/self/status", "/proc/self/cgroup", "/proc/self/mountinfo")
}

func fingerprintCPU() (string, error) {
	// Keep the hardware fingerprint independent of the artifact-owned CPU label
	// so a runner probe and the subsequent stamp can compare it byte-for-byte.
	parts := []string{
		"goos=" + runtime.GOOS,
		"goarch=" + runtime.GOARCH,
		"num_cpu=" + strconv.Itoa(runtime.NumCPU()),
		"gomaxprocs=" + strconv.Itoa(runtime.GOMAXPROCS(0)),
	}
	if runtime.GOOS == "linux" {
		details, err := stableLinuxCPUDetails("/proc/cpuinfo")
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return "", err
		}
		if details != "" {
			parts = append(parts, details)
		}
		if online, err := os.ReadFile("/sys/devices/system/cpu/online"); err == nil {
			parts = append(parts, "online="+strings.TrimSpace(string(online)))
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return "", err
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("%x", digest[:]), nil
}

func stableLinuxCPUDetails(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	wanted := map[string]bool{
		"vendor_id": true, "cpu family": true, "model": true, "model name": true,
		"stepping": true, "microcode": true, "flags": true, "features": true,
		"cpu implementer": true, "cpu architecture": true, "cpu variant": true,
		"cpu part": true, "cpu revision": true, "hardware": true,
	}
	profiles := make(map[string]struct{})
	current := make(map[string]string)
	flush := func() {
		if len(current) == 0 {
			return
		}
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var lines []string
		for _, key := range keys {
			lines = append(lines, key+"="+current[key])
		}
		profiles[strings.Join(lines, "\n")] = struct{}{}
		current = make(map[string]string)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if wanted[key] {
			current[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	flush()
	values := make([]string, 0, len(profiles))
	for profile := range profiles {
		values = append(values, profile)
	}
	sort.Strings(values)
	return strings.Join(values, "\n--cpu-profile--\n"), nil
}

func readProvenance(filename string) (resultProvenance, error) {
	var metadata resultProvenance
	if err := readStrictJSON(filename, &metadata); err != nil {
		return resultProvenance{}, err
	}
	if err := validateProvenance(metadata); err != nil {
		return resultProvenance{}, err
	}
	return metadata, nil
}

func readStrictJSON(filename string, target any) error {
	contents, err := readRegularFile(filename, maximumMetadataBytes)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return err
	}
	return decodeStrictJSON(contents, target)
}

func decodeStrictJSON(contents []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON has trailing data")
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

func writeJSONExclusive(filename string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return writeBytesExclusive(filename, contents, 0o644)
}

func writeBytesExclusive(filename string, contents []byte, mode os.FileMode) error {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	if err := requireAbsent(filename); err != nil {
		return err
	}
	directory := filepath.Dir(filename)
	file, err := os.CreateTemp(directory, ".benchgate-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	// Sync contents before a no-clobber hard-link publish. Without syncing the
	// parent directory, this helper does not claim crash-durable directory metadata.
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, filename); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		_ = os.Remove(filename)
		return err
	}
	keep = true
	return nil
}

func requireAbsent(filename string) error {
	_, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("refusing to replace existing path %s", filename)
}

func resolveCommit(repo, revision string) (string, error) {
	command := exec.Command("git", "-C", repo, "rev-parse", "--verify", revision+"^{commit}")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %q: %w: %s", revision, err, strings.TrimSpace(string(output)))
	}
	resolved := strings.TrimSpace(string(output))
	if err := validateSHA(resolved); err != nil {
		return "", fmt.Errorf("resolved object ID %q: %w", resolved, err)
	}
	return resolved, nil
}

func validateCheckout(repo, expected string) error {
	resolved, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return err
	}
	if resolved != expected {
		return fmt.Errorf("HEAD is %s, want %s", resolved, expected)
	}
	return requireCleanCheckout(repo)
}

func requireAncestor(repo, ancestor, descendant string) error {
	command := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return fmt.Errorf("base commit %s is not an ancestor of candidate %s", ancestor, descendant)
	}
	return fmt.Errorf("git merge-base --is-ancestor: %w: %s", err, strings.TrimSpace(string(output)))
}

func requireCleanCheckout(repo string) error {
	command := exec.Command("git", "-C", repo, "status", "--porcelain=v1", "--untracked-files=all")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if len(output) != 0 {
		return fmt.Errorf("benchmark checkout is not clean:\n%s", strings.TrimSpace(string(output)))
	}
	return nil
}

func printComparisons(output io.Writer, comparisons []metricComparison) {
	fmt.Fprintln(output, "benchmark\tmetric\tbase median\tcandidate median\tdelta\tstatus")
	for _, comparison := range comparisons {
		status := "INFO"
		if comparison.Blocking {
			status = "PASS"
		}
		if comparison.Regression {
			status = "FAIL"
		}
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%s\t%s\n",
			comparison.Benchmark, comparison.Metric,
			formatMedian(comparison.BaseMedian), formatMedian(comparison.CandidateMedian),
			formatDelta(comparison), status)
	}
}
