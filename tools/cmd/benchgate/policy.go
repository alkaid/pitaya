package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const policySchemaVersion = 1

type benchmarkPolicy struct {
	SchemaVersion  int      `json:"schema_version"`
	Profile        string   `json:"profile"`
	Package        string   `json:"package"`
	Pattern        string   `json:"pattern"`
	Benchtime      string   `json:"benchtime"`
	Count          int      `json:"count"`
	Timeout        string   `json:"timeout"`
	Tags           []string `json:"tags"`
	TestRun        string   `json:"test_run"`
	Benchmem       bool     `json:"benchmem"`
	BenchmarkNames []string `json:"benchmark_names"`
	ApparatusPaths []string `json:"apparatus_paths"`
}

func readPolicy(filename string) (benchmarkPolicy, string, error) {
	contents, err := readRegularFile(filename, maximumMetadataBytes)
	if err != nil {
		return benchmarkPolicy{}, "", err
	}
	// encoding/json otherwise accepts duplicate and case-folded field names;
	// rejecting them keeps one unambiguous policy interpretation.
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return benchmarkPolicy{}, "", err
	}
	var policy benchmarkPolicy
	if err := decodeStrictJSON(contents, &policy); err != nil {
		return benchmarkPolicy{}, "", err
	}
	if err := validatePolicy(policy); err != nil {
		return benchmarkPolicy{}, "", err
	}
	digest := sha256.Sum256(contents)
	return policy, fmt.Sprintf("%x", digest[:]), nil
}

func validatePolicy(policy benchmarkPolicy) error {
	if policy.SchemaVersion != policySchemaVersion {
		return fmt.Errorf("policy schema_version is %d, want %d", policy.SchemaVersion, policySchemaVersion)
	}
	if err := validateID("profile", policy.Profile); err != nil {
		return err
	}
	if policy.Package == "" || len(policy.Package) > 512 || !utf8.ValidString(policy.Package) || strings.ContainsAny(policy.Package, "\x00\r\n") {
		return errors.New("policy package must be a non-empty single-line UTF-8 value")
	}
	configured := benchmarkConfig{Profile: policy.Profile, Package: policy.Package, Pattern: policy.Pattern, Benchtime: policy.Benchtime, Count: policy.Count, Timeout: policy.Timeout, PolicySHA256: strings.Repeat("0", 64), ApparatusSHA256: strings.Repeat("0", 64), GateSourceSHA: strings.Repeat("0", 40)}
	if err := validateBenchmarkConfig(configured); err != nil {
		return err
	}
	if policy.Count < minimumSamples {
		return fmt.Errorf("policy count %d is below required minimum %d", policy.Count, minimumSamples)
	}
	if len(policy.Tags) != 1 || policy.Tags[0] != "benchmark" {
		return errors.New("policy tags must be exactly [\"benchmark\"]")
	}
	if policy.TestRun != "^$" {
		return fmt.Errorf("policy test_run is %q, want ^$", policy.TestRun)
	}
	if !policy.Benchmem {
		return errors.New("policy benchmem must be true")
	}
	if len(policy.BenchmarkNames) == 0 {
		return errors.New("policy benchmark_names must not be empty")
	}
	if err := validateSortedUniqueValues("benchmark_names", policy.BenchmarkNames, validateBenchmarkName); err != nil {
		return err
	}
	if len(policy.ApparatusPaths) == 0 {
		return errors.New("policy apparatus_paths must not be empty")
	}
	if err := validateSortedUniqueValues("apparatus_paths", policy.ApparatusPaths, validateApparatusPath); err != nil {
		return err
	}
	return nil
}

func configFromPolicy(policy benchmarkPolicy, policyDigest, apparatus, gateSourceSHA string) benchmarkConfig {
	return benchmarkConfig{
		Profile:         policy.Profile,
		Package:         policy.Package,
		Pattern:         policy.Pattern,
		Benchtime:       policy.Benchtime,
		Count:           policy.Count,
		Timeout:         policy.Timeout,
		PolicySHA256:    policyDigest,
		ApparatusSHA256: apparatus,
		GateSourceSHA:   gateSourceSHA,
	}
}

func validateSortedUniqueValues(field string, values []string, validate func(string) error) error {
	for index, value := range values {
		if err := validate(value); err != nil {
			return fmt.Errorf("policy %s[%d]: %w", field, index, err)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("policy %s must be strictly sorted and unique", field)
		}
	}
	return nil
}

func validateBenchmarkName(value string) error {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("benchmark name must be a non-empty single-line UTF-8 value")
	}
	if strings.HasPrefix(value, "Benchmark") {
		return errors.New("benchmark name must omit the Benchmark prefix")
	}
	return nil
}

func validateApparatusPath(value string) error {
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != filepath.ToSlash(value) || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, `\`) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("apparatus path must be a clean repository-relative slash path")
	}
	return nil
}

func apparatusDigest(repo string, paths []string) (string, error) {
	hash := sha256.New()
	for _, relative := range paths {
		if err := requireTrackedPath(repo, relative); err != nil {
			return "", fmt.Errorf("apparatus %s: %w", relative, err)
		}
		filename := filepath.Join(repo, filepath.FromSlash(relative))
		contents, err := readRegularFile(filename, maxBenchmarkBytes)
		if err != nil {
			return "", fmt.Errorf("apparatus %s: %w", relative, err)
		}
		// Length-prefix both names and contents so concatenated path/file pairs
		// cannot collide through ambiguous boundaries.
		fmt.Fprintf(hash, "%d:%s\n%d:", len(relative), relative, len(contents))
		if _, err := hash.Write(contents); err != nil {
			return "", err
		}
		if _, err := io.WriteString(hash, "\n"); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

type apparatusBuild struct {
	target       string
	tags         []string
	includeTests bool
}

type listedGoPackage struct {
	Dir             string
	ImportPath      string
	ForTest         string
	GoFiles         []string
	CgoFiles        []string
	CFiles          []string
	CXXFiles        []string
	MFiles          []string
	HFiles          []string
	FFiles          []string
	SFiles          []string
	SwigFiles       []string
	SwigCXXFiles    []string
	SysoFiles       []string
	EmbedFiles      []string
	TestGoFiles     []string
	XTestGoFiles    []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

func apparatusDigestForPolicy(repo string, policy benchmarkPolicy) (string, error) {
	selected, err := selectedApparatusFiles(repo, policy)
	if err != nil {
		return "", err
	}
	paths := append(append([]string(nil), policy.ApparatusPaths...), selected...)
	sort.Strings(paths)
	paths = slices.Compact(paths)
	return apparatusDigest(repo, paths)
}

func selectedApparatusFiles(repo string, policy benchmarkPolicy) ([]string, error) {
	protectedDirs := make(map[string]struct{})
	// Mirror the production harness: the benchmark target is test-compiled with
	// policy tags, while each listed main package is built without extra tags.
	// The orchestrator applies -mod=readonly and -pgo=off to both this go list and
	// the real builds so vendor/automatic PGO cannot create a second build graph.
	builds := []apparatusBuild{{target: policy.Package, tags: policy.Tags, includeTests: true}}
	seenBuilds := map[string]struct{}{policy.Package + "\x00" + strings.Join(policy.Tags, ",") + "\x001": {}}
	for _, relative := range policy.ApparatusPaths {
		if filepath.Ext(relative) != ".go" {
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(relative))
		protectedDirs[directory] = struct{}{}
		if filepath.Base(relative) != "main.go" {
			continue
		}
		target := "./" + directory
		key := target + "\x00\x000"
		if _, ok := seenBuilds[key]; !ok {
			seenBuilds[key] = struct{}{}
			builds = append(builds, apparatusBuild{target: target})
		}
	}
	if len(protectedDirs) == 0 {
		return nil, nil
	}

	repo, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]struct{})
	seenDirs := make(map[string]struct{})
	for _, build := range builds {
		packages, err := listBuildPackages(repo, build)
		if err != nil {
			return nil, err
		}
		for _, pkg := range packages {
			if pkg.ForTest != "" || strings.HasSuffix(pkg.ImportPath, ".test") {
				continue
			}
			directory, err := filepath.Rel(repo, pkg.Dir)
			if err != nil {
				return nil, err
			}
			directory = filepath.ToSlash(directory)
			if _, protected := protectedDirs[directory]; !protected {
				continue
			}
			seenDirs[directory] = struct{}{}
			files := append([]string{}, pkg.GoFiles...)
			for _, group := range [][]string{pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles} {
				files = append(files, group...)
			}
			if build.includeTests && pkg.ImportPath == policy.Package {
				files = append(files, pkg.TestGoFiles...)
				files = append(files, pkg.XTestGoFiles...)
				files = append(files, pkg.TestEmbedFiles...)
				files = append(files, pkg.XTestEmbedFiles...)
			}
			for _, name := range files {
				relative, err := selectedApparatusPath(directory, name)
				if err != nil {
					return nil, err
				}
				selected[relative] = struct{}{}
			}
		}
	}
	// Explicit paths define the narrow harness package boundary. With the fixed
	// module/PGO flags, go list defines the files compiled inside it, so additions
	// cannot evade the digest while production packages remain measured inputs.
	for directory := range protectedDirs {
		if _, ok := seenDirs[directory]; !ok {
			return nil, fmt.Errorf("apparatus package %q is not selected by a declared benchmark build", directory)
		}
	}
	paths := make([]string, 0, len(selected))
	for relative := range selected {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	return paths, nil
}

func selectedApparatusPath(directory, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("selected apparatus source %q is absolute", name)
	}
	relative := filepath.ToSlash(filepath.Join(directory, name))
	if err := validateApparatusPath(relative); err != nil {
		return "", fmt.Errorf("selected apparatus %q: %w", relative, err)
	}
	return relative, nil
}

func listBuildPackages(repo string, build apparatusBuild) ([]listedGoPackage, error) {
	args := []string{"list", "-deps", "-json", "-mod=readonly"}
	if build.includeTests {
		args = append(args, "-test")
	}
	if len(build.tags) > 0 {
		args = append(args, "-tags="+strings.Join(build.tags, ","))
	}
	args = append(args, build.target)
	command := exec.Command("go", args...)
	command.Dir = repo
	command.Env = buildEnvironment(os.Environ())
	output, err := command.Output()
	if err != nil {
		detail := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			detail = strings.TrimSpace(string(exitError.Stderr))
		}
		return nil, fmt.Errorf("list apparatus build %q: %w: %s", build.target, err, detail)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var packages []listedGoPackage
	for {
		var pkg listedGoPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list for %q: %w", build.target, err)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("list apparatus build %q returned no packages", build.target)
	}
	return packages, nil
}

func buildEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+3)
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if key != "GOWORK" && key != "GOOS" && key != "GOARCH" {
			result = append(result, value)
		}
	}
	return append(result, "GOWORK=off", "GOOS=linux", "GOARCH=amd64")
}

func requireTrackedPath(repo, relative string) error {
	command := exec.Command("git", "-C", repo, "ls-files", "--error-unmatch", "--", relative)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("not a tracked file: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != relative {
		return fmt.Errorf("git resolved path as %q", strings.TrimSpace(string(output)))
	}
	return nil
}

func readRegularFile(filename string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return contents, nil
}
