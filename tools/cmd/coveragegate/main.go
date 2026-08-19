package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/cover"
)

const (
	policySchemaVersion            = 1
	profileProvenanceSchemaVersion = 1
	modulePath                     = "github.com/alkaid/pitaya/v2"
)

var separatelyVerifiedModuleRoots = []string{"pitaya-cli", "tools", "xk6-pitaya"}

type policy struct {
	SchemaVersion    int              `json:"schema_version"`
	BaseSHA          string           `json:"base_sha"`
	CoveragePlatform coveragePlatform `json:"coverage_platform"`
	ExcludedFiles    []string         `json:"excluded_files"`
	Targets          targets          `json:"targets"`
	Baseline         baseline         `json:"baseline"`
}

type coveragePlatform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type profileProvenance struct {
	SchemaVersion int    `json:"schema_version"`
	CommitSHA     string `json:"commit_sha"`
	ProfileSHA256 string `json:"profile_sha256"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
}

type targets struct {
	OverallPercent  float64            `json:"overall_percent"`
	PackagePercent  map[string]float64 `json:"package_percent"`
	DiffPercent     float64            `json:"diff_percent"`
	EnforceAbsolute bool               `json:"enforce_absolute"`
}

type baseline struct {
	Overall  counts            `json:"overall"`
	Packages map[string]counts `json:"packages"`
}

type counts struct {
	Covered int64 `json:"covered"`
	Total   int64 `json:"total"`
}

type profileBlock struct {
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	NumStmt   int
	Count     int
}

type fileCoverage struct {
	Blocks []profileBlock
}

type coverageData struct {
	Mode    string
	Summary baseline
	Files   map[string]fileCoverage
}

type stringList []string

func (list *stringList) String() string {
	return strings.Join(*list, ",")
}

func (list *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*list = append(*list, value)
	return nil
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
	case "generate":
		err = runGenerate(args[1:], stdout, stderr)
	case "check":
		err = runCheck(args[1:], stdout, stderr)
	case "update":
		err = runUpdate(args[1:], stdout, stderr)
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
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errUsage) {
		if err != errUsage {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	fmt.Fprintln(stderr, err)
	return 1
}

var errUsage = errors.New("invalid command usage")

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "usage:")
	fmt.Fprintln(output, "  coveragegate generate -policy FILE -profile FILE [-repo DIR]")
	fmt.Fprintln(output, "  coveragegate check -policy FILE -profile FILE -base SHA -head SHA [-repo DIR]")
	fmt.Fprintln(output, "  coveragegate update -policy FILE -profile FILE [-profile FILE ...] -base-sha SHA [-repo DIR]")
}

func runGenerate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var policyPath string
	var profile string
	var repo string
	flags.StringVar(&policyPath, "policy", "", "coverage policy JSON")
	flags.StringVar(&profile, "profile", "", "Go coverage profile")
	flags.StringVar(&repo, "repo", ".", "repository root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || policyPath == "" || profile == "" {
		flags.Usage()
		return errUsage
	}
	var err error
	profile, err = filepath.Abs(profile)
	if err != nil {
		return fmt.Errorf("resolve profile path: %w", err)
	}
	repo, err = filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolve repository path: %w", err)
	}
	if err := removeFileIfPresent(profileProvenancePath(profile)); err != nil {
		return fmt.Errorf("invalidate profile provenance: %w", err)
	}

	configured, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	if err := validateRuntimePlatform(configured.CoveragePlatform); err != nil {
		return err
	}
	if err := validateExcludedFiles(repo, configured.ExcludedFiles); err != nil {
		return fmt.Errorf("validate exclusions: %w", err)
	}
	head, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve checkout HEAD: %w", err)
	}
	moduleRoots, err := nestedModuleRoots(repo)
	if err != nil {
		return err
	}
	if err := ensureCleanRootCheckout(repo, moduleRoots, profile, profileProvenancePath(profile)); err != nil {
		return err
	}
	if err := removeFileIfPresent(profile); err != nil {
		return fmt.Errorf("remove stale coverage profile: %w", err)
	}

	command := exec.Command("go", "test", "./...", "-count=1", "-coverpkg=./...", "-coverprofile="+profile)
	command.Dir = repo
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"GOARCH":  configured.CoveragePlatform.GOARCH,
		"GOFLAGS": "",
		"GOOS":    configured.CoveragePlatform.GOOS,
		"GOWORK":  "off",
	})
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("generate coverage profile: %w", err)
	}
	after, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve checkout HEAD after coverage: %w", err)
	}
	if after != head {
		return fmt.Errorf("checkout HEAD changed during coverage generation: %s -> %s", head, after)
	}
	moduleRoots, err = nestedModuleRoots(repo)
	if err != nil {
		return err
	}
	if err := ensureCleanRootCheckout(repo, moduleRoots, profile, profileProvenancePath(profile)); err != nil {
		return err
	}
	if err := writeProfileProvenance(profile, head, configured.CoveragePlatform); err != nil {
		return fmt.Errorf("write profile provenance: %w", err)
	}
	fmt.Fprintf(stdout, "coverage profile stamped: %s (%s %s/%s)\n", profile, head, configured.CoveragePlatform.GOOS, configured.CoveragePlatform.GOARCH)
	return nil
}

func runCheck(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var policyPath string
	var profiles stringList
	var repo string
	var base string
	var head string
	flags.StringVar(&policyPath, "policy", "", "coverage policy JSON")
	flags.Var(&profiles, "profile", "Go coverage profile")
	flags.StringVar(&repo, "repo", ".", "repository root")
	flags.StringVar(&base, "base", "", "base Git revision")
	flags.StringVar(&head, "head", "", "head Git revision")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || policyPath == "" || len(profiles) == 0 || base == "" || head == "" {
		flags.Usage()
		return errUsage
	}
	if len(profiles) != 1 {
		return fmt.Errorf("%w: check requires exactly one -profile; repeated profiles are only supported by update", errUsage)
	}
	if err := validateSHA(base); err != nil {
		return fmt.Errorf("base SHA: %w", err)
	}
	if err := validateSHA(head); err != nil {
		return fmt.Errorf("head SHA: %w", err)
	}

	configured, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	if err := validateRuntimePlatform(configured.CoveragePlatform); err != nil {
		return err
	}
	base, err = resolveCommit(repo, base)
	if err != nil {
		return fmt.Errorf("resolve base SHA: %w", err)
	}
	head, err = resolveCommit(repo, head)
	if err != nil {
		return fmt.Errorf("resolve head SHA: %w", err)
	}
	checkout, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve checkout HEAD: %w", err)
	}
	if head != checkout {
		return fmt.Errorf("head SHA %s does not match checked out HEAD %s", head, checkout)
	}
	if base == head {
		return errors.New("base and head SHAs must differ")
	}
	policyBase, err := resolveCommit(repo, configured.BaseSHA)
	if err != nil {
		return fmt.Errorf("resolve policy base_sha: %w", err)
	}
	if err := requireAncestor(repo, policyBase, head); err != nil {
		return fmt.Errorf("policy base_sha: %w", err)
	}
	moduleRoots, err := nestedModuleRoots(repo)
	if err != nil {
		return err
	}
	if err := ensureCleanRootCheckout(repo, moduleRoots, profiles[0], profileProvenancePath(profiles[0])); err != nil {
		return err
	}
	if err := validateProfileProvenances(profiles, head, configured.CoveragePlatform); err != nil {
		return fmt.Errorf("profile provenance: %w", err)
	}
	if err := validateExcludedFiles(repo, configured.ExcludedFiles); err != nil {
		return fmt.Errorf("validate exclusions: %w", err)
	}
	current, err := readCoverageProfiles(profiles, configured.ExcludedFiles)
	if err != nil {
		return fmt.Errorf("read coverage: %w", err)
	}
	if err := compareBaseline(configured.Baseline, current.Summary); err != nil {
		return fmt.Errorf("coverage regression: %w", err)
	}
	if configured.Targets.EnforceAbsolute {
		if err := enforceAbsoluteTargets(configured.Targets, current.Summary); err != nil {
			return fmt.Errorf("absolute coverage target: %w", err)
		}
	} else {
		fmt.Fprintln(stdout, "absolute coverage targets pending (enforce_absolute=false)")
	}

	changed, err := gitChangedLines(repo, base, head, configured.ExcludedFiles)
	if err != nil {
		return fmt.Errorf("read changed lines: %w", err)
	}
	diff, err := measureDiffCoverage(repo, changed, current.Files)
	if err != nil {
		return fmt.Errorf("diff coverage: %w", err)
	}
	if diff.Total > 0 && !meetsPercent(diff, configured.Targets.DiffPercent) {
		return fmt.Errorf("diff coverage %.2f%% is below %.2f%% (%d/%d)", percent(diff), configured.Targets.DiffPercent, diff.Covered, diff.Total)
	}

	fmt.Fprintf(stdout, "coverage gate passed: overall %.2f%% (%d/%d), diff %.2f%% (%d/%d)\n",
		percent(current.Summary.Overall), current.Summary.Overall.Covered, current.Summary.Overall.Total,
		percent(diff), diff.Covered, diff.Total)
	return nil
}

func runUpdate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var policyPath string
	var profiles stringList
	var baseSHA string
	var repo string
	flags.StringVar(&policyPath, "policy", "", "coverage policy JSON")
	flags.Var(&profiles, "profile", "Go coverage profile (repeatable)")
	flags.StringVar(&baseSHA, "base-sha", "", "full baseline commit SHA")
	flags.StringVar(&repo, "repo", ".", "repository root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || policyPath == "" || len(profiles) == 0 || baseSHA == "" {
		flags.Usage()
		return errUsage
	}
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("base SHA: %w", err)
	}

	configured, err := readPolicy(policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	if err := validateRuntimePlatform(configured.CoveragePlatform); err != nil {
		return err
	}
	baseSHA, err = resolveCommit(repo, baseSHA)
	if err != nil {
		return fmt.Errorf("resolve base SHA: %w", err)
	}
	checkout, err := resolveCommit(repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve checkout HEAD: %w", err)
	}
	if baseSHA != checkout {
		return fmt.Errorf("base SHA %s does not match checked out HEAD %s", baseSHA, checkout)
	}
	moduleRoots, err := nestedModuleRoots(repo)
	if err != nil {
		return err
	}
	allowed := append([]string(nil), profiles...)
	for _, profile := range profiles {
		allowed = append(allowed, profileProvenancePath(profile))
	}
	if err := ensureCleanRootCheckout(repo, moduleRoots, allowed...); err != nil {
		return err
	}
	if err := validateProfileProvenances(profiles, baseSHA, configured.CoveragePlatform); err != nil {
		return fmt.Errorf("profile provenance: %w", err)
	}
	if err := validateExcludedFiles(repo, configured.ExcludedFiles); err != nil {
		return fmt.Errorf("validate exclusions: %w", err)
	}
	current, err := readCoverageProfiles(profiles, configured.ExcludedFiles)
	if err != nil {
		return fmt.Errorf("read coverage: %w", err)
	}
	configured.BaseSHA = baseSHA
	configured.Baseline = current.Summary
	if err := validatePolicy(configured); err != nil {
		return fmt.Errorf("updated policy: %w", err)
	}
	if err := writePolicyAtomic(policyPath, configured); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	fmt.Fprintf(stdout, "updated %s at %s: %.2f%% (%d/%d)\n", policyPath, baseSHA, percent(current.Summary.Overall), current.Summary.Overall.Covered, current.Summary.Overall.Total)
	return nil
}

type policyWire struct {
	SchemaVersion    *int                  `json:"schema_version"`
	BaseSHA          *string               `json:"base_sha"`
	CoveragePlatform *coveragePlatformWire `json:"coverage_platform"`
	ExcludedFiles    *[]string             `json:"excluded_files"`
	Targets          *targetsWire          `json:"targets"`
	Baseline         *baselineWire         `json:"baseline"`
}

type coveragePlatformWire struct {
	GOOS   *string `json:"goos"`
	GOARCH *string `json:"goarch"`
}

type targetsWire struct {
	OverallPercent  *float64            `json:"overall_percent"`
	PackagePercent  *map[string]float64 `json:"package_percent"`
	DiffPercent     *float64            `json:"diff_percent"`
	EnforceAbsolute *bool               `json:"enforce_absolute"`
}

type baselineWire struct {
	Overall  *countsWire            `json:"overall"`
	Packages *map[string]countsWire `json:"packages"`
}

type countsWire struct {
	Covered *int64 `json:"covered"`
	Total   *int64 `json:"total"`
}

func readPolicy(filename string) (policy, error) {
	file, err := os.Open(filename)
	if err != nil {
		return policy{}, err
	}
	defer file.Close()
	return decodePolicy(file)
}

func decodePolicy(input io.Reader) (policy, error) {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var wire policyWire
	if err := decoder.Decode(&wire); err != nil {
		return policy{}, fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return policy{}, errors.New("decode JSON: multiple values")
		}
		return policy{}, fmt.Errorf("decode JSON trailer: %w", err)
	}

	if wire.SchemaVersion == nil || wire.BaseSHA == nil || wire.CoveragePlatform == nil || wire.ExcludedFiles == nil || wire.Targets == nil || wire.Baseline == nil {
		return policy{}, errors.New("policy is missing a required top-level field")
	}
	if wire.CoveragePlatform.GOOS == nil || wire.CoveragePlatform.GOARCH == nil {
		return policy{}, errors.New("coverage_platform is missing a required field")
	}
	if wire.Targets.OverallPercent == nil || wire.Targets.PackagePercent == nil || wire.Targets.DiffPercent == nil || wire.Targets.EnforceAbsolute == nil {
		return policy{}, errors.New("targets is missing a required field")
	}
	if wire.Baseline.Overall == nil || wire.Baseline.Packages == nil {
		return policy{}, errors.New("baseline is missing a required field")
	}
	overall, err := unwrapCounts("baseline.overall", *wire.Baseline.Overall)
	if err != nil {
		return policy{}, err
	}
	packages := make(map[string]counts, len(*wire.Baseline.Packages))
	for name, value := range *wire.Baseline.Packages {
		entry, err := unwrapCounts("baseline.packages["+strconv.Quote(name)+"]", value)
		if err != nil {
			return policy{}, err
		}
		packages[name] = entry
	}

	result := policy{
		SchemaVersion: *wire.SchemaVersion,
		BaseSHA:       *wire.BaseSHA,
		CoveragePlatform: coveragePlatform{
			GOOS:   *wire.CoveragePlatform.GOOS,
			GOARCH: *wire.CoveragePlatform.GOARCH,
		},
		ExcludedFiles: *wire.ExcludedFiles,
		Targets: targets{
			OverallPercent:  *wire.Targets.OverallPercent,
			PackagePercent:  *wire.Targets.PackagePercent,
			DiffPercent:     *wire.Targets.DiffPercent,
			EnforceAbsolute: *wire.Targets.EnforceAbsolute,
		},
		Baseline: baseline{Overall: overall, Packages: packages},
	}
	if err := validatePolicy(result); err != nil {
		return policy{}, err
	}
	return result, nil
}

func unwrapCounts(field string, value countsWire) (counts, error) {
	if value.Covered == nil || value.Total == nil {
		return counts{}, fmt.Errorf("%s is missing covered or total", field)
	}
	return counts{Covered: *value.Covered, Total: *value.Total}, nil
}

func validatePolicy(configured policy) error {
	if configured.SchemaVersion != policySchemaVersion {
		return fmt.Errorf("schema_version is %d, want %d", configured.SchemaVersion, policySchemaVersion)
	}
	if err := validateSHA(configured.BaseSHA); err != nil {
		return fmt.Errorf("base_sha: %w", err)
	}
	if configured.CoveragePlatform.GOOS == "" || configured.CoveragePlatform.GOARCH == "" {
		return errors.New("coverage_platform.goos and coverage_platform.goarch must not be empty")
	}
	if configured.ExcludedFiles == nil {
		return errors.New("excluded_files must be an array")
	}
	for index, name := range configured.ExcludedFiles {
		if err := validateRepoFile(name); err != nil {
			return fmt.Errorf("excluded_files[%d]: %w", index, err)
		}
		if !strings.HasSuffix(name, ".go") {
			return fmt.Errorf("excluded_files[%d]: %q is not a Go source file", index, name)
		}
		if index > 0 && configured.ExcludedFiles[index-1] >= name {
			return errors.New("excluded_files must be sorted and unique")
		}
	}
	if configured.Targets.PackagePercent == nil {
		return errors.New("targets.package_percent must be an object")
	}
	if err := validatePercent("targets.overall_percent", configured.Targets.OverallPercent); err != nil {
		return err
	}
	if err := validatePercent("targets.diff_percent", configured.Targets.DiffPercent); err != nil {
		return err
	}
	for name, target := range configured.Targets.PackagePercent {
		if err := validatePackage(name); err != nil {
			return fmt.Errorf("targets.package_percent[%q]: %w", name, err)
		}
		if err := validatePercent("targets.package_percent["+strconv.Quote(name)+"]", target); err != nil {
			return err
		}
	}
	if configured.Baseline.Packages == nil {
		return errors.New("baseline.packages must be an object")
	}
	if err := validateCounts("baseline.overall", configured.Baseline.Overall); err != nil {
		return err
	}
	sum := counts{}
	for name, value := range configured.Baseline.Packages {
		if err := validatePackage(name); err != nil {
			return fmt.Errorf("baseline.packages[%q]: %w", name, err)
		}
		if err := validateCounts("baseline.packages["+strconv.Quote(name)+"]", value); err != nil {
			return err
		}
		var err error
		sum.Covered, err = addNonnegative(sum.Covered, value.Covered)
		if err != nil {
			return fmt.Errorf("baseline covered count: %w", err)
		}
		sum.Total, err = addNonnegative(sum.Total, value.Total)
		if err != nil {
			return fmt.Errorf("baseline total count: %w", err)
		}
	}
	if sum != configured.Baseline.Overall {
		return fmt.Errorf("baseline.overall is %d/%d, package sum is %d/%d", configured.Baseline.Overall.Covered, configured.Baseline.Overall.Total, sum.Covered, sum.Total)
	}
	return nil
}

func validateSHA(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return errors.New("must be a full 40- or 64-character hexadecimal object ID")
	}
	for _, char := range value {
		if char < '0' || (char > '9' && char < 'a') || char > 'f' {
			return errors.New("must contain only lowercase hexadecimal characters")
		}
	}
	return nil
}

func validateRuntimePlatform(configured coveragePlatform) error {
	if runtime.GOOS != configured.GOOS || runtime.GOARCH != configured.GOARCH {
		return fmt.Errorf("coverage policy requires %s/%s, current runtime is %s/%s", configured.GOOS, configured.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	return nil
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

func requireAncestor(repo, ancestor, descendant string) error {
	command := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return fmt.Errorf("commit %s is not an ancestor of %s", ancestor, descendant)
	}
	return fmt.Errorf("git merge-base --is-ancestor %s %s: %w: %s", ancestor, descendant, err, strings.TrimSpace(string(output)))
}

func profileProvenancePath(profile string) string {
	return profile + ".meta.json"
}

func validateProfileProvenances(profiles []string, expectedCommit string, platform coveragePlatform) error {
	for _, profile := range profiles {
		if err := requireRegularFile(profile); err != nil {
			return fmt.Errorf("profile %q: %w", profile, err)
		}
		metadataPath := profileProvenancePath(profile)
		if err := requireRegularFile(metadataPath); err != nil {
			return fmt.Errorf("metadata %q: %w", metadataPath, err)
		}
		metadata, err := readProfileProvenance(metadataPath)
		if err != nil {
			return fmt.Errorf("metadata %q: %w", metadataPath, err)
		}
		if metadata.SchemaVersion != profileProvenanceSchemaVersion {
			return fmt.Errorf("metadata %q schema_version is %d, want %d", metadataPath, metadata.SchemaVersion, profileProvenanceSchemaVersion)
		}
		if err := validateSHA(metadata.CommitSHA); err != nil {
			return fmt.Errorf("metadata %q commit_sha: %w", metadataPath, err)
		}
		if metadata.CommitSHA != expectedCommit {
			return fmt.Errorf("metadata %q names commit %s, want %s", metadataPath, metadata.CommitSHA, expectedCommit)
		}
		if metadata.GOOS != platform.GOOS || metadata.GOARCH != platform.GOARCH {
			return fmt.Errorf("metadata %q platform is %s/%s, want %s/%s", metadataPath, metadata.GOOS, metadata.GOARCH, platform.GOOS, platform.GOARCH)
		}
		digest, err := profileDigest(profile)
		if err != nil {
			return fmt.Errorf("hash profile %q: %w", profile, err)
		}
		if metadata.ProfileSHA256 != digest {
			return fmt.Errorf("metadata %q profile_sha256 is %s, want %s", metadataPath, metadata.ProfileSHA256, digest)
		}
	}
	return nil
}

func readProfileProvenance(filename string) (profileProvenance, error) {
	file, err := os.Open(filename)
	if err != nil {
		return profileProvenance{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var metadata profileProvenance
	if err := decoder.Decode(&metadata); err != nil {
		return profileProvenance{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return profileProvenance{}, errors.New("multiple JSON values")
		}
		return profileProvenance{}, err
	}
	return metadata, nil
}

func writeProfileProvenance(profile, commit string, platform coveragePlatform) error {
	if err := requireRegularFile(profile); err != nil {
		return err
	}
	digest, err := profileDigest(profile)
	if err != nil {
		return err
	}
	return writeJSONAtomic(profileProvenancePath(profile), profileProvenance{
		SchemaVersion: profileProvenanceSchemaVersion,
		CommitSHA:     commit,
		ProfileSHA256: digest,
		GOOS:          platform.GOOS,
		GOARCH:        platform.GOARCH,
	})
}

func profileDigest(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func requireRegularFile(filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func removeFileIfPresent(filename string) error {
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("refusing to remove a directory")
	}
	return os.Remove(filename)
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := replacements[name]; !replaced {
			result = append(result, entry)
		}
	}
	for _, name := range sortedKeys(replacements) {
		result = append(result, name+"="+replacements[name])
	}
	return result
}

func ensureCleanRootCheckout(repo string, moduleRoots []string, allowedPaths ...string) error {
	allowed, err := repoRelativePaths(repo, allowedPaths)
	if err != nil {
		return err
	}
	var names []string
	for _, args := range [][]string{
		{"diff", "--name-only", "--no-ext-diff", "--no-renames", "-z", "HEAD", "--"},
		{"ls-files", "--others", "--exclude-standard", "-z", "--"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", ":(glob)**/*.go", ":(glob)**/*.s", ":(glob)**/*.c", ":(glob)**/*.h", ":(glob)**/*.syso", ":(glob)**/go.mod", ":(glob)**/go.sum"},
	} {
		current, err := gitPathList(repo, args...)
		if err != nil {
			return fmt.Errorf("inspect checkout state: %w", err)
		}
		names = append(names, current...)
	}
	dirty := make(map[string]struct{})
	for _, name := range names {
		if err := validateRepoFile(name); err != nil {
			return fmt.Errorf("checkout path: %w", err)
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		if withinNestedModule(name, moduleRoots) && !isCoverageVerifierInput(name) {
			continue
		}
		dirty[name] = struct{}{}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("coverage checkout differs from HEAD: %v", sortedKeys(dirty))
	}
	return nil
}

func isCoverageVerifierInput(name string) bool {
	return name == "tools/go.mod" || name == "tools/go.sum" || strings.HasPrefix(name, "tools/cmd/coveragegate/")
}

func repoRelativePaths(repo string, filenames []string) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	repoAbsolute, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	for _, filename := range filenames {
		absolute, err := filepath.Abs(filename)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(repoAbsolute, absolute)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		name := filepath.ToSlash(relative)
		if err := validateRepoFile(name); err != nil {
			return nil, err
		}
		result[name] = struct{}{}
	}
	return result, nil
}

func gitPathList(repo string, args ...string) ([]string, error) {
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	var result []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			result = append(result, name)
		}
	}
	return result, nil
}

func validateRepoFile(name string) error {
	if name == "" || name == "." || strings.Contains(name, `\`) || strings.IndexByte(name, 0) >= 0 || path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("%q is not a canonical repo-relative file path", name)
	}
	return nil
}

func validateExcludedFiles(repo string, names []string) error {
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("excluded file %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("excluded file %q is not a regular file", name)
		}
	}
	return nil
}

func validatePackage(name string) error {
	if name == "." {
		return nil
	}
	if err := validateRepoFile(name); err != nil {
		return errors.New("package must be '.' or a canonical repo-relative directory")
	}
	return nil
}

func validatePercent(field string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return fmt.Errorf("%s must be between 0 and 100", field)
	}
	return nil
}

func validateCounts(field string, value counts) error {
	if value.Covered < 0 || value.Total < 0 {
		return fmt.Errorf("%s counts must be nonnegative", field)
	}
	if value.Covered > value.Total {
		return fmt.Errorf("%s covered count exceeds total", field)
	}
	return nil
}

func writePolicyAtomic(filename string, configured policy) error {
	return writeJSONAtomic(filename, configured)
}

func writeJSONAtomic(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(filename); statErr == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	directory := filepath.Dir(filename)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(filename)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return err
	}
	if directoryFile, err := os.Open(directory); err == nil {
		syncErr := directoryFile.Sync()
		closeErr := directoryFile.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func readCoverageProfiles(filenames []string, exclusions []string) (coverageData, error) {
	if len(filenames) == 0 {
		return coverageData{}, errors.New("at least one profile is required")
	}
	excluded := makeStringSet(exclusions)
	var conservative coverageData
	for index, filename := range filenames {
		current, err := readCoverageProfile(filename, excluded)
		if err != nil {
			return coverageData{}, fmt.Errorf("%s: %w", filename, err)
		}
		if index == 0 {
			conservative = current
			continue
		}
		if err := mergeConservative(&conservative, current); err != nil {
			return coverageData{}, fmt.Errorf("%s: %w", filename, err)
		}
	}
	packageSum, err := sumPackages(conservative.Summary.Packages)
	if err != nil {
		return coverageData{}, err
	}
	if packageSum != conservative.Summary.Overall {
		return coverageData{}, fmt.Errorf("per-package conservative floors sum to %d/%d, but the minimum complete-run overall is %d/%d; use one complete low run", packageSum.Covered, packageSum.Total, conservative.Summary.Overall.Covered, conservative.Summary.Overall.Total)
	}
	return conservative, nil
}

func readCoverageProfile(filename string, excluded map[string]struct{}) (coverageData, error) {
	profiles, err := cover.ParseProfiles(filename)
	if err != nil {
		return coverageData{}, err
	}
	if len(profiles) == 0 {
		return coverageData{}, errors.New("profile contains no source files")
	}
	result := coverageData{
		Summary: baseline{Packages: make(map[string]counts)},
		Files:   make(map[string]fileCoverage),
	}
	for _, profile := range profiles {
		if result.Mode == "" {
			result.Mode = profile.Mode
		} else if result.Mode != profile.Mode {
			return coverageData{}, fmt.Errorf("profile mixes coverage modes %q and %q", result.Mode, profile.Mode)
		}
		name, err := normalizeProfileFile(profile.FileName)
		if err != nil {
			return coverageData{}, err
		}
		if _, skip := excluded[name]; skip {
			continue
		}
		if _, duplicate := result.Files[name]; duplicate {
			return coverageData{}, fmt.Errorf("duplicate normalized profile file %q", name)
		}
		file := fileCoverage{Blocks: make([]profileBlock, 0, len(profile.Blocks))}
		packageName := path.Dir(name)
		packageCounts := result.Summary.Packages[packageName]
		for _, block := range profile.Blocks {
			if block.NumStmt < 0 || block.Count < 0 {
				return coverageData{}, fmt.Errorf("profile file %q contains negative block values", name)
			}
			file.Blocks = append(file.Blocks, profileBlock{
				StartLine: block.StartLine,
				StartCol:  block.StartCol,
				EndLine:   block.EndLine,
				EndCol:    block.EndCol,
				NumStmt:   block.NumStmt,
				Count:     block.Count,
			})
			packageCounts.Total, err = addNonnegative(packageCounts.Total, int64(block.NumStmt))
			if err != nil {
				return coverageData{}, fmt.Errorf("profile file %q total: %w", name, err)
			}
			if block.Count > 0 {
				packageCounts.Covered, err = addNonnegative(packageCounts.Covered, int64(block.NumStmt))
				if err != nil {
					return coverageData{}, fmt.Errorf("profile file %q covered: %w", name, err)
				}
			}
		}
		result.Files[name] = file
		result.Summary.Packages[packageName] = packageCounts
	}
	result.Summary.Overall, err = sumPackages(result.Summary.Packages)
	if err != nil {
		return coverageData{}, err
	}
	return result, nil
}

func normalizeProfileFile(name string) (string, error) {
	name = filepath.ToSlash(name)
	prefix := modulePath + "/"
	if strings.HasPrefix(name, prefix) {
		name = strings.TrimPrefix(name, prefix)
	} else if name == modulePath || strings.HasPrefix(name, modulePath) {
		return "", fmt.Errorf("profile file %q is not below module path %s", name, modulePath)
	}
	if err := validateRepoFile(name); err != nil {
		return "", fmt.Errorf("profile file: %w", err)
	}
	if !strings.HasSuffix(name, ".go") {
		return "", fmt.Errorf("profile file %q is not a Go source file", name)
	}
	return name, nil
}

func mergeConservative(destination *coverageData, current coverageData) error {
	if destination.Mode != current.Mode {
		return fmt.Errorf("profile coverage modes differ: %q != %q", destination.Mode, current.Mode)
	}
	if onlyDestination, onlyCurrent := differentKeys(destination.Summary.Packages, current.Summary.Packages); len(onlyDestination) > 0 || len(onlyCurrent) > 0 {
		return fmt.Errorf("profile package sets differ: first-only=%v current-only=%v", onlyDestination, onlyCurrent)
	}
	for name, destinationCounts := range destination.Summary.Packages {
		currentCounts := current.Summary.Packages[name]
		if destinationCounts.Total != currentCounts.Total {
			return fmt.Errorf("profile package %q total differs: %d != %d", name, destinationCounts.Total, currentCounts.Total)
		}
		if currentCounts.Covered < destinationCounts.Covered {
			destinationCounts.Covered = currentCounts.Covered
			destination.Summary.Packages[name] = destinationCounts
		}
	}
	if onlyDestination, onlyCurrent := differentKeys(destination.Files, current.Files); len(onlyDestination) > 0 || len(onlyCurrent) > 0 {
		return fmt.Errorf("profile file sets differ: first-only=%v current-only=%v", onlyDestination, onlyCurrent)
	}
	for name, destinationFile := range destination.Files {
		currentFile := current.Files[name]
		if len(destinationFile.Blocks) != len(currentFile.Blocks) {
			return fmt.Errorf("profile file %q block count differs: %d != %d", name, len(destinationFile.Blocks), len(currentFile.Blocks))
		}
		for index, destinationBlock := range destinationFile.Blocks {
			currentBlock := currentFile.Blocks[index]
			destinationBlock.Count = 0
			currentBlock.Count = 0
			if destinationBlock != currentBlock {
				return fmt.Errorf("profile file %q block %d layout differs", name, index)
			}
		}
	}
	if current.Summary.Overall.Total != destination.Summary.Overall.Total {
		return fmt.Errorf("profile overall total differs: %d != %d", destination.Summary.Overall.Total, current.Summary.Overall.Total)
	}
	if current.Summary.Overall.Covered < destination.Summary.Overall.Covered {
		destination.Summary.Overall.Covered = current.Summary.Overall.Covered
	}
	return nil
}

func sumPackages(packages map[string]counts) (counts, error) {
	result := counts{}
	for _, value := range packages {
		var err error
		result.Covered, err = addNonnegative(result.Covered, value.Covered)
		if err != nil {
			return counts{}, err
		}
		result.Total, err = addNonnegative(result.Total, value.Total)
		if err != nil {
			return counts{}, err
		}
	}
	return result, nil
}

func addNonnegative(left, right int64) (int64, error) {
	if left < 0 || right < 0 {
		return 0, errors.New("negative count")
	}
	if left > math.MaxInt64-right {
		return 0, errors.New("count overflow")
	}
	return left + right, nil
}

func compareBaseline(expected, current baseline) error {
	if onlyExpected, onlyCurrent := differentKeys(expected.Packages, current.Packages); len(onlyExpected) > 0 || len(onlyCurrent) > 0 {
		return fmt.Errorf("package sets differ: baseline-only=%v current-only=%v", onlyExpected, onlyCurrent)
	}
	if ratioLess(current.Overall, expected.Overall) {
		return fmt.Errorf("overall ratio dropped from %d/%d to %d/%d", expected.Overall.Covered, expected.Overall.Total, current.Overall.Covered, current.Overall.Total)
	}
	names := sortedKeys(expected.Packages)
	for _, name := range names {
		if ratioLess(current.Packages[name], expected.Packages[name]) {
			before := expected.Packages[name]
			after := current.Packages[name]
			return fmt.Errorf("package %q ratio dropped from %d/%d to %d/%d", name, before.Covered, before.Total, after.Covered, after.Total)
		}
	}
	return nil
}

func ratioLess(current, expected counts) bool {
	if expected.Total == 0 {
		return false
	}
	if current.Total == 0 {
		return true
	}
	left := new(big.Int).Mul(big.NewInt(current.Covered), big.NewInt(expected.Total))
	right := new(big.Int).Mul(big.NewInt(expected.Covered), big.NewInt(current.Total))
	return left.Cmp(right) < 0
}

func enforceAbsoluteTargets(required targets, current baseline) error {
	if !meetsPercent(current.Overall, required.OverallPercent) {
		return fmt.Errorf("overall %.2f%% is below %.2f%% (%d/%d)", percent(current.Overall), required.OverallPercent, current.Overall.Covered, current.Overall.Total)
	}
	names := sortedKeys(required.PackagePercent)
	for _, name := range names {
		value, ok := current.Packages[name]
		if !ok {
			return fmt.Errorf("targeted package %q is missing from current coverage", name)
		}
		if !meetsPercent(value, required.PackagePercent[name]) {
			return fmt.Errorf("package %q %.2f%% is below %.2f%% (%d/%d)", name, percent(value), required.PackagePercent[name], value.Covered, value.Total)
		}
	}
	return nil
}

func meetsPercent(value counts, target float64) bool {
	if value.Total == 0 {
		return target == 0
	}
	targetText := strconv.FormatFloat(target, 'f', -1, 64)
	targetRatio, ok := new(big.Rat).SetString(targetText)
	if !ok {
		return false
	}
	numerator := new(big.Int).Mul(big.NewInt(value.Covered), big.NewInt(100))
	actual := new(big.Rat).SetFrac(numerator, big.NewInt(value.Total))
	return actual.Cmp(targetRatio) >= 0
}

func percent(value counts) float64 {
	if value.Total == 0 {
		return 0
	}
	return float64(value.Covered) * 100 / float64(value.Total)
}

func makeStringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func differentKeys[A, B any](left map[string]A, right map[string]B) ([]string, []string) {
	var onlyLeft []string
	var onlyRight []string
	for name := range left {
		if _, ok := right[name]; !ok {
			onlyLeft = append(onlyLeft, name)
		}
	}
	for name := range right {
		if _, ok := left[name]; !ok {
			onlyRight = append(onlyRight, name)
		}
	}
	sort.Strings(onlyLeft)
	sort.Strings(onlyRight)
	return onlyLeft, onlyRight
}

func sortedKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

type changedLines map[string]map[int]struct{}

var hunkPattern = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func gitChangedLines(repo, base, head string, exclusions []string) (changedLines, error) {
	command := exec.Command(
		"git", "-C", repo, "diff", "--no-ext-diff", "--no-color", "--no-textconv",
		"--text", "--src-prefix=a/", "--dst-prefix=b/", "--no-renames", "--unified=0",
		"--diff-filter=ACM", base, head, "--",
	)
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git diff %s %s: %w: %s", base, head, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("git diff %s %s: %w", base, head, err)
	}
	changed, err := parseChangedLines(bytes.NewReader(output), makeStringSet(exclusions))
	if err != nil {
		return nil, err
	}
	moduleRoots, err := nestedModuleRoots(repo)
	if err != nil {
		return nil, err
	}
	for name := range changed {
		if withinNestedModule(name, moduleRoots) {
			delete(changed, name)
		}
	}
	return changed, nil
}

func parseChangedLines(input io.Reader, exclusions map[string]struct{}) (changedLines, error) {
	result := make(changedLines)
	var currentFile string
	inPatch := false
	inHunk := false
	expectNewPath := false
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "diff --git ") {
			currentFile = ""
			inPatch = true
			inHunk = false
			expectNewPath = false
			continue
		}
		if inPatch && !inHunk && strings.HasPrefix(line, "--- ") {
			expectNewPath = true
			continue
		}
		if inPatch && !inHunk && expectNewPath && strings.HasPrefix(line, "+++ ") {
			name, err := parseDiffPath(strings.TrimPrefix(line, "+++ "))
			if err != nil {
				return nil, err
			}
			expectNewPath = false
			currentFile = ""
			if name == "" || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if _, skip := exclusions[name]; skip {
				continue
			}
			currentFile = name
			continue
		}
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		inHunk = true
		matches := hunkPattern.FindStringSubmatch(line)
		if matches == nil {
			return nil, fmt.Errorf("malformed diff hunk header %q", line)
		}
		if currentFile == "" {
			continue
		}
		start, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse diff hunk start: %w", err)
		}
		count := 1
		if matches[2] != "" {
			count, err = strconv.Atoi(matches[2])
			if err != nil {
				return nil, fmt.Errorf("parse diff hunk count: %w", err)
			}
		}
		if count == 0 {
			continue
		}
		lines := result[currentFile]
		if lines == nil {
			lines = make(map[int]struct{}, count)
			result[currentFile] = lines
		}
		for offset := 0; offset < count; offset++ {
			lines[start+offset] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func nestedModuleRoots(repo string) ([]string, error) {
	command := exec.Command("git", "-C", repo, "ls-files", "-z", "--cached", "--", ":(glob)**/go.mod")
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("list nested modules: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("list nested modules: %w", err)
	}
	var roots []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" || name == "go.mod" {
			continue
		}
		if err := validateRepoFile(name); err != nil {
			return nil, fmt.Errorf("nested module path: %w", err)
		}
		root := path.Dir(name)
		if root != "." {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	expected := makeStringSet(separatelyVerifiedModuleRoots)
	actual := makeStringSet(roots)
	if missing, unregistered := differentKeys(expected, actual); len(missing) > 0 || len(unregistered) > 0 {
		return nil, fmt.Errorf("nested module set differs: expected-only=%v unregistered=%v", missing, unregistered)
	}
	return roots, nil
}

func withinNestedModule(name string, roots []string) bool {
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

func parseDiffPath(value string) (string, error) {
	if value == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("parse quoted diff path %q: %w", value, err)
		}
		value = decoded
	}
	value = strings.TrimPrefix(value, "b/")
	if err := validateRepoFile(value); err != nil {
		return "", fmt.Errorf("diff path: %w", err)
	}
	return value, nil
}

func measureDiffCoverage(repo string, changed changedLines, files map[string]fileCoverage) (counts, error) {
	result := counts{}
	for _, name := range sortedKeys(changed) {
		file, found := files[name]
		if !found {
			hasStatements, err := hasExecutableStatements(filepath.Join(repo, filepath.FromSlash(name)))
			if err != nil {
				return counts{}, fmt.Errorf("inspect %q: %w", name, err)
			}
			if hasStatements {
				return counts{}, fmt.Errorf("changed production file %q has executable function statements but no coverage profile entry", name)
			}
			continue
		}
		for line := range changed[name] {
			executable := false
			covered := false
			for _, block := range file.Blocks {
				if block.NumStmt == 0 || line < block.StartLine || line > block.EndLine {
					continue
				}
				executable = true
				if block.Count > 0 {
					covered = true
				}
			}
			if executable {
				result.Total++
				if covered {
					result.Covered++
				}
			}
		}
	}
	return result, nil
}

func hasExecutableStatements(filename string) (bool, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("not a regular file")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		return false, err
	}
	for _, declaration := range parsed.Decls {
		if variables, ok := declaration.(*ast.GenDecl); ok && variables.Tok == token.VAR {
			for _, specification := range variables.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if ok && len(value.Values) > 0 {
					return true, nil
				}
			}
		}
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		found := false
		ast.Inspect(function.Body, func(bodyNode ast.Node) bool {
			statement, ok := bodyNode.(ast.Stmt)
			if !ok {
				return !found
			}
			switch statement.(type) {
			case *ast.BlockStmt, *ast.EmptyStmt:
				return !found
			default:
				found = true
				return false
			}
		})
		if found {
			return true, nil
		}
	}
	return false, nil
}
