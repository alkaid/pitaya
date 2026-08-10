package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	manifestSchemaVersion = 1
	maxFuzzDuration       = time.Hour
	maxTestTimeout        = 2 * time.Hour
	defaultSeedTimeout    = 30 * time.Second
	commandTimeoutGrace   = 15 * time.Second
)

var (
	fuzzBaselinePattern = regexp.MustCompile(`^fuzz: elapsed: [^\n]*gathering baseline coverage: ([0-9]+)/([0-9]+) completed, now fuzzing with [0-9]+ workers\b`)
	fuzzProgressPattern = regexp.MustCompile(`^fuzz: elapsed: [^\n]*\bexecs: ([0-9]+)\b`)
)

type fuzzManifest struct {
	SchemaVersion int          `json:"schema_version"`
	Targets       []fuzzTarget `json:"targets"`
}

type fuzzTarget struct {
	Package string `json:"package"`
	Target  string `json:"target"`
}

func (t fuzzTarget) key() string {
	return t.Package + " " + t.Target
}

type goListPackage struct {
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type eventSummary struct {
	targetRun  bool
	targetPass bool
	targetBad  bool
	corpusRun  map[string]bool
	corpusPass map[string]bool
	corpusBad  map[string]bool
	baseline   uint64
	maxExecs   uint64
	mutating   bool
}

type commandOptions struct {
	repo      string
	artifacts string
	stdout    io.Writer
	stderr    io.Writer
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: fuzzgate <seeds|fuzz> [flags]")
		return 2
	}

	switch args[0] {
	case "seeds":
		return runSeeds(args[1:], stdout, stderr)
	case "fuzz":
		return runFuzz(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runSeeds(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("seeds", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", ".", "repository root")
	manifestPath := flags.String("manifest", "scripts/fuzz-targets.json", "fuzz target manifest")
	timeoutText := flags.String("timeout", defaultSeedTimeout.String(), "per-target seed timeout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "seeds does not accept positional arguments")
		return 2
	}

	timeout, err := parseDuration("timeout", *timeoutText, maxTestTimeout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	opts, manifest, err := prepare(*repo, *manifestPath, "", stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := verifyInventory(opts, manifest.Targets); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var failures []error
	for _, target := range manifest.Targets {
		if err := runSeedTarget(opts, target, timeout); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		fmt.Fprintln(stderr, errors.Join(failures...))
		return 1
	}
	fmt.Fprintf(stdout, "fuzz seed gate passed: %d targets\n", len(manifest.Targets))
	return 0
}

func runFuzz(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fuzz", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", ".", "repository root")
	manifestPath := flags.String("manifest", "scripts/fuzz-targets.json", "fuzz target manifest")
	artifacts := flags.String("artifacts", "", "directory for go test JSON and stderr")
	packageName := flags.String("package", "", "run one registered package")
	targetName := flags.String("target", "", "run one registered target")
	cleanCache := flags.Bool("clean-cache", false, "clear Go's fuzz cache before mutation")
	seedTimeoutText := flags.String("seedtimeout", defaultSeedTimeout.String(), "per-target seed timeout")
	fuzzTimeText := flags.String("fuzztime", "2m", "mutation time per target")
	minimizeTimeText := flags.String("minimizetime", "30s", "failure minimization time")
	timeoutText := flags.String("timeout", "4m", "per-target go test timeout")
	parallel := flags.Int("parallel", 2, "fuzz workers per target")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "fuzz does not accept positional arguments")
		return 2
	}

	seedTimeout, err := parseDuration("seedtimeout", *seedTimeoutText, maxTestTimeout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fuzzTime, err := parseDuration("fuzztime", *fuzzTimeText, maxFuzzDuration)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	minimizeTime, err := parseDuration("minimizetime", *minimizeTimeText, maxFuzzDuration)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	timeout, err := parseDuration("timeout", *timeoutText, maxTestTimeout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if timeout <= fuzzTime {
		fmt.Fprintln(stderr, "timeout must be greater than fuzztime")
		return 2
	}
	if *parallel < 1 || *parallel > 16 {
		fmt.Fprintln(stderr, "parallel must be between 1 and 16")
		return 2
	}
	if (*packageName == "") != (*targetName == "") {
		fmt.Fprintln(stderr, "package and target must be provided together")
		return 2
	}

	opts, manifest, err := prepare(*repo, *manifestPath, *artifacts, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := verifyInventory(opts, manifest.Targets); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	targets, err := selectTargets(manifest.Targets, *packageName, *targetName)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *cleanCache {
		if err := cleanFuzzCache(opts); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	var failures []error
	for _, target := range targets {
		if err := runSeedTarget(opts, target, seedTimeout); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := runFuzzTarget(opts, target, fuzzTime, minimizeTime, timeout, *parallel); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		fmt.Fprintln(stderr, errors.Join(failures...))
		return 1
	}
	fmt.Fprintf(stdout, "nightly fuzz gate passed: %d targets, %s each\n", len(targets), fuzzTime)
	return 0
}

func parseDuration(name, value string, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	if duration <= 0 || duration > maximum {
		return 0, fmt.Errorf("%s must be greater than zero and no more than %s", name, maximum)
	}
	return duration, nil
}

func prepare(repo, manifestPath, artifacts string, stdout, stderr io.Writer) (commandOptions, fuzzManifest, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return commandOptions{}, fuzzManifest{}, fmt.Errorf("resolve repository root: %w", err)
	}
	if info, err := os.Stat(filepath.Join(absRepo, "go.mod")); err != nil || info.IsDir() {
		return commandOptions{}, fuzzManifest{}, fmt.Errorf("repository root has no go.mod: %s", absRepo)
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(absRepo, manifestPath)
	}
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return commandOptions{}, fuzzManifest{}, err
	}
	if artifacts != "" && !filepath.IsAbs(artifacts) {
		artifacts = filepath.Join(absRepo, artifacts)
	}
	if artifacts != "" {
		if err := os.MkdirAll(artifacts, 0o755); err != nil {
			return commandOptions{}, fuzzManifest{}, fmt.Errorf("create fuzz artifact directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "gate-started"), []byte("fuzz gate started\n"), 0o644); err != nil {
			return commandOptions{}, fuzzManifest{}, fmt.Errorf("initialize fuzz artifacts: %w", err)
		}
	}
	return commandOptions{repo: absRepo, artifacts: artifacts, stdout: stdout, stderr: stderr}, manifest, nil
}

func loadManifest(path string) (fuzzManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return fuzzManifest{}, fmt.Errorf("open fuzz manifest: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest fuzzManifest
	if err := decoder.Decode(&manifest); err != nil {
		return fuzzManifest{}, fmt.Errorf("decode fuzz manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fuzzManifest{}, fmt.Errorf("decode fuzz manifest: %w", err)
	}
	if manifest.SchemaVersion != manifestSchemaVersion {
		return fuzzManifest{}, fmt.Errorf("unsupported fuzz manifest schema_version %d", manifest.SchemaVersion)
	}
	if len(manifest.Targets) == 0 {
		return fuzzManifest{}, errors.New("fuzz manifest has no targets")
	}

	seen := make(map[string]struct{}, len(manifest.Targets))
	previous := ""
	for i, target := range manifest.Targets {
		if err := validateTarget(target); err != nil {
			return fuzzManifest{}, fmt.Errorf("fuzz manifest target %d: %w", i, err)
		}
		key := target.key()
		if _, ok := seen[key]; ok {
			return fuzzManifest{}, fmt.Errorf("duplicate fuzz target %q", key)
		}
		if previous != "" && key <= previous {
			return fuzzManifest{}, errors.New("fuzz manifest targets must be sorted by package and target")
		}
		seen[key] = struct{}{}
		previous = key
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func validateTarget(target fuzzTarget) error {
	if !token.IsIdentifier(target.Target) || !isFuzzName(target.Target) {
		return fmt.Errorf("invalid target name %q", target.Target)
	}
	if target.Package == "." {
		return nil
	}
	if !strings.HasPrefix(target.Package, "./") || strings.Contains(target.Package, "...") || strings.Contains(target.Package, "\\") {
		return fmt.Errorf("invalid package %q", target.Package)
	}
	relative := strings.TrimPrefix(target.Package, "./")
	if relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || path.Clean(relative) != relative {
		return fmt.Errorf("invalid package %q", target.Package)
	}
	return nil
}

func verifyInventory(opts commandOptions, expected []fuzzTarget) error {
	discovered, err := discoverFuzzTargets(opts)
	if err != nil {
		return err
	}
	if equalTargets(discovered, expected) {
		return nil
	}
	return fmt.Errorf("fuzz target inventory mismatch\nmanifest: %s\nsource:   %s", targetKeys(expected), targetKeys(discovered))
}

func discoverFuzzTargets(opts commandOptions) ([]fuzzTarget, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-json", "./...")
	cmd.Dir = opts.repo
	cmd.Env = cleanGoEnv()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = opts.stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("discover fuzz packages: %w", err)
	}
	decoder := json.NewDecoder(&output)
	var packages []goListPackage
	for {
		var pkg goListPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		packages = append(packages, pkg)
	}
	return discoverFuzzTargetsFromPackages(opts.repo, packages)
}

func discoverFuzzTargetsFromPackages(repo string, packages []goListPackage) ([]fuzzTarget, error) {
	var targets []fuzzTarget
	seen := make(map[string]struct{})
	activeTestFiles := make(map[string]struct{})
	for _, pkg := range packages {
		relative, err := filepath.Rel(repo, pkg.Dir)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("package directory is outside repository: %s", pkg.Dir)
		}
		packagePattern := "."
		if relative != "." {
			packagePattern = "./" + filepath.ToSlash(relative)
		}
		for _, name := range append(append([]string{}, pkg.TestGoFiles...), pkg.XTestGoFiles...) {
			filename := filepath.Clean(filepath.Join(pkg.Dir, name))
			activeTestFiles[filename] = struct{}{}
			functions, err := fuzzFunctionsInFile(filename)
			if err != nil {
				return nil, err
			}
			for _, function := range functions {
				target := fuzzTarget{Package: packagePattern, Target: function.Name.Name}
				if _, ok := seen[target.key()]; ok {
					return nil, fmt.Errorf("duplicate source fuzz target %q", target.key())
				}
				seen[target.key()] = struct{}{}
				targets = append(targets, target)
			}
		}
	}
	if err := rejectInactiveFuzzTargets(repo, activeTestFiles); err != nil {
		return nil, err
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].key() < targets[j].key() })
	return targets, nil
}

func rejectInactiveFuzzTargets(repo string, activeTestFiles map[string]struct{}) error {
	err := filepath.WalkDir(repo, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if filename == repo {
				return nil
			}
			name := entry.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			info, err := os.Stat(filepath.Join(filename, "go.mod"))
			if err == nil && !info.IsDir() {
				return fs.SkipDir
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect nested module at %s: %w", filename, err)
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		filename = filepath.Clean(filename)
		if _, ok := activeTestFiles[filename]; ok {
			return nil
		}
		functions, err := fuzzFunctionsInFile(filename)
		if err != nil {
			return err
		}
		if len(functions) != 0 {
			return fmt.Errorf("inactive fuzz target %s in %s; build-constrained fuzz targets require a separate manifest and execution lane", functions[0].Name.Name, filename)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan inactive fuzz targets: %w", err)
	}
	return nil
}

func fuzzFunctionsInFile(filename string) ([]*ast.FuncDecl, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	var functions []*ast.FuncDecl
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || !isFuzzName(function.Name.Name) {
			continue
		}
		if !hasFuzzSignature(function) {
			return nil, fmt.Errorf("invalid fuzz target signature: %s in %s", function.Name.Name, filename)
		}
		functions = append(functions, function)
	}
	return functions, nil
}

func isFuzzName(name string) bool {
	if !strings.HasPrefix(name, "Fuzz") {
		return false
	}
	if len(name) == len("Fuzz") {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len("Fuzz"):])
	return !unicode.IsLower(r)
}

func hasFuzzSignature(function *ast.FuncDecl) bool {
	if function.Type.Results != nil && len(function.Type.Results.List) > 0 ||
		function.Type.Params == nil ||
		len(function.Type.Params.List) != 1 ||
		len(function.Type.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if name, ok := star.X.(*ast.Ident); ok && name.Name == "F" {
		return true
	}
	if selector, ok := star.X.(*ast.SelectorExpr); ok && selector.Sel.Name == "F" {
		return true
	}
	return false
}

func equalTargets(first, second []fuzzTarget) bool {
	if len(first) != len(second) {
		return false
	}
	for i := range first {
		if first[i] != second[i] {
			return false
		}
	}
	return true
}

func targetKeys(targets []fuzzTarget) string {
	keys := make([]string, len(targets))
	for i, target := range targets {
		keys[i] = target.key()
	}
	return strings.Join(keys, ", ")
}

func selectTargets(targets []fuzzTarget, packageName, targetName string) ([]fuzzTarget, error) {
	if packageName == "" {
		return append([]fuzzTarget(nil), targets...), nil
	}
	selected := fuzzTarget{Package: packageName, Target: targetName}
	if err := validateTarget(selected); err != nil {
		return nil, err
	}
	for _, target := range targets {
		if target == selected {
			return []fuzzTarget{target}, nil
		}
	}
	return nil, fmt.Errorf("fuzz target is not registered: %s", selected.key())
}

func runSeedTarget(opts commandOptions, target fuzzTarget, timeout time.Duration) error {
	fmt.Fprintf(opts.stdout, "==> seed %s\n", target.key())
	args := []string{
		"test", "-json",
		"-run", "^" + regexp.QuoteMeta(target.Target) + "$",
		"-count=1",
		"-timeout=" + timeout.String(),
		target.Package,
	}
	summary, err := runGoTest(opts, target, "seed", args, timeout+commandTimeoutGrace)
	if err != nil {
		return fmt.Errorf("seed %s: %w", target.key(), err)
	}
	if err := summary.validateSeeds(target.Target); err != nil {
		return fmt.Errorf("seed %s: %w", target.key(), err)
	}
	return nil
}

func runFuzzTarget(opts commandOptions, target fuzzTarget, fuzzTime, minimizeTime, timeout time.Duration, parallel int) error {
	fmt.Fprintf(opts.stdout, "==> fuzz %s for %s\n", target.key(), fuzzTime)
	args := []string{
		"test", "-json",
		"-run", "^$",
		"-fuzz", "^" + regexp.QuoteMeta(target.Target) + "$",
		"-fuzztime=" + fuzzTime.String(),
		"-fuzzminimizetime=" + minimizeTime.String(),
		"-timeout=" + timeout.String(),
		"-parallel=" + strconv.Itoa(parallel),
		target.Package,
	}
	summary, err := runGoTest(opts, target, "fuzz", args, timeout+commandTimeoutGrace)
	if err != nil {
		return fmt.Errorf("fuzz %s: %w", target.key(), err)
	}
	if err := summary.validateFuzz(target.Target); err != nil {
		return fmt.Errorf("fuzz %s: %w", target.key(), err)
	}
	return nil
}

func cleanFuzzCache(opts commandOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "clean", "-fuzzcache")
	cmd.Dir = opts.repo
	cmd.Env = cleanGoEnv()
	cmd.Stdout = opts.stdout
	cmd.Stderr = opts.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clear fuzz cache: %w", err)
	}
	return nil
}

func runGoTest(opts commandOptions, target fuzzTarget, phase string, args []string, commandTimeout time.Duration) (eventSummary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = opts.repo
	cmd.Env = cleanGoEnv()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return eventSummary{}, err
	}
	var rawJSON io.Writer
	var closers []io.Closer
	stderrWriter := opts.stderr
	if opts.artifacts != "" {
		artifactDir := targetArtifactDir(opts.artifacts, target)
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			return eventSummary{}, fmt.Errorf("create fuzz artifact directory: %w", err)
		}
		if err := writeTargetArtifactMetadata(artifactDir, target); err != nil {
			return eventSummary{}, err
		}
		base := phase
		jsonFile, err := os.Create(filepath.Join(artifactDir, base+".jsonl"))
		if err != nil {
			return eventSummary{}, err
		}
		stderrFile, err := os.Create(filepath.Join(artifactDir, base+".stderr"))
		if err != nil {
			jsonFile.Close()
			return eventSummary{}, err
		}
		rawJSON = jsonFile
		stderrWriter = io.MultiWriter(opts.stderr, stderrFile)
		closers = append(closers, jsonFile, stderrFile)
	}
	for _, closer := range closers {
		defer closer.Close()
	}
	cmd.Stderr = stderrWriter

	if err := cmd.Start(); err != nil {
		return eventSummary{}, err
	}
	summary, parseErr := consumeEvents(stdoutPipe, rawJSON, opts.stdout, target.Target)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return summary, fmt.Errorf("command exceeded %s: %w", commandTimeout, ctx.Err())
	}
	if parseErr != nil || waitErr != nil {
		return summary, errors.Join(parseErr, waitErr)
	}
	return summary, nil
}

func consumeEvents(reader io.Reader, rawJSON, display io.Writer, target string) (eventSummary, error) {
	if rawJSON != nil {
		reader = io.TeeReader(reader, rawJSON)
	}
	summary := eventSummary{
		corpusRun:  make(map[string]bool),
		corpusPass: make(map[string]bool),
		corpusBad:  make(map[string]bool),
	}
	decoder := json.NewDecoder(reader)
	for {
		var event testEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return summary, fmt.Errorf("decode go test event: %w", err)
		}
		if event.Output != "" {
			fmt.Fprint(display, event.Output)
		}
		if event.Action == "output" && event.Test == target {
			if match := fuzzBaselinePattern.FindStringSubmatch(event.Output); match != nil {
				completed, completedErr := strconv.ParseUint(match[1], 10, 64)
				total, totalErr := strconv.ParseUint(match[2], 10, 64)
				if completedErr == nil && totalErr == nil && completed == total {
					summary.baseline = total
					summary.mutating = true
				}
			}
			for _, match := range fuzzProgressPattern.FindAllStringSubmatch(event.Output, -1) {
				execs, err := strconv.ParseUint(match[1], 10, 64)
				if err == nil && execs > summary.maxExecs {
					summary.maxExecs = execs
				}
			}
		}
		switch {
		case event.Test == target:
			switch event.Action {
			case "run":
				summary.targetRun = true
			case "pass":
				summary.targetPass = true
			case "fail", "skip":
				summary.targetBad = true
			}
		case strings.HasPrefix(event.Test, target+"/"):
			switch event.Action {
			case "run":
				summary.corpusRun[event.Test] = true
			case "pass":
				summary.corpusPass[event.Test] = true
			case "fail", "skip":
				summary.corpusBad[event.Test] = true
			}
		}
	}
	return summary, nil
}

func targetArtifactDir(root string, target fuzzTarget) string {
	packageHash := sha256.Sum256([]byte(target.Package))
	targetHash := sha256.Sum256([]byte(target.Target))
	return filepath.Join(root, "targets", fmt.Sprintf("pkg-%x", packageHash), fmt.Sprintf("target-%x", targetHash))
}

func writeTargetArtifactMetadata(directory string, target fuzzTarget) error {
	contents, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fuzz artifact metadata: %w", err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(filepath.Join(directory, "target.json"), contents, 0o644); err != nil {
		return fmt.Errorf("write fuzz artifact metadata: %w", err)
	}
	return nil
}

func (summary eventSummary) validateSeeds(target string) error {
	if !summary.targetRun || !summary.targetPass || summary.targetBad {
		return fmt.Errorf("target %s did not run and pass", target)
	}
	if len(summary.corpusRun) == 0 {
		return fmt.Errorf("target %s ran no seed corpus entries", target)
	}
	for corpus := range summary.corpusRun {
		if !summary.corpusPass[corpus] || summary.corpusBad[corpus] {
			return fmt.Errorf("corpus entry %s did not pass", corpus)
		}
	}
	for corpus := range summary.corpusBad {
		if !summary.corpusRun[corpus] {
			return fmt.Errorf("corpus entry %s did not run and pass", corpus)
		}
	}
	return nil
}

func (summary eventSummary) validateFuzz(target string) error {
	if !summary.targetRun || !summary.targetPass || summary.targetBad {
		return fmt.Errorf("target %s did not run and pass", target)
	}
	if !summary.mutating || summary.maxExecs <= summary.baseline {
		return fmt.Errorf("target %s reported zero mutation executions (baseline %d, total %d)", target, summary.baseline, summary.maxExecs)
	}
	return nil
}

func cleanGoEnv() []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, variable := range os.Environ() {
		if strings.HasPrefix(variable, "GOWORK=") || strings.HasPrefix(variable, "GOFLAGS=") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, "GOWORK=off", "GOFLAGS=")
}
