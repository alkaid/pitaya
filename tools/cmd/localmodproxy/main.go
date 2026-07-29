package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const (
	rootPath       = "github.com/alkaid/pitaya/v2"
	cliPath        = "github.com/alkaid/pitaya/pitaya-cli/v2"
	xk6Path        = "github.com/alkaid/pitaya/xk6-pitaya"
	defaultRootVer = "v2.99.0-batch1d.0"
	defaultCLIVer  = "v2.99.0-batch1d.0"
	defaultXK6Ver  = "v0.99.0-batch1d.0"
)

type moduleSpec struct {
	path        string
	version     string
	sourceDir   string
	requireRoot bool
}

func main() {
	var repo string
	var output string
	var rootVersion string
	var cliVersion string
	var xk6Version string
	flag.StringVar(&repo, "repo", "..", "repository root")
	flag.StringVar(&output, "out", "", "file proxy output directory")
	flag.StringVar(&rootVersion, "root-version", defaultRootVer, "synthetic root module version")
	flag.StringVar(&cliVersion, "cli-version", defaultCLIVer, "synthetic CLI module version")
	flag.StringVar(&xk6Version, "xk6-version", defaultXK6Ver, "synthetic xk6 module version")
	flag.Parse()

	if output == "" {
		fatalf("-out is required")
	}

	var err error
	repo, err = filepath.Abs(repo)
	if err != nil {
		fatalf("resolve repository root: %v", err)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		fatalf("resolve proxy output: %v", err)
	}

	files, err := trackedFiles(repo)
	if err != nil {
		fatalf("list repository files: %v", err)
	}

	specs := []moduleSpec{
		{path: rootPath, version: rootVersion},
		{path: cliPath, version: cliVersion, sourceDir: "pitaya-cli", requireRoot: true},
		{path: xk6Path, version: xk6Version, sourceDir: "xk6-pitaya", requireRoot: true},
	}
	for _, spec := range specs {
		if err := writeModule(repo, output, files, spec, rootVersion); err != nil {
			fatalf("write %s@%s: %v", spec.path, spec.version, err)
		}
	}

	fmt.Printf("ROOT_VERSION=%s\nCLI_VERSION=%s\nXK6_VERSION=%s\n", rootVersion, cliVersion, xk6Version)
}

func trackedFiles(repo string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "ls-files", "-z", "--cached")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	entries := strings.Split(string(output), "\x00")
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry != "" {
			files = append(files, filepath.Clean(entry))
		}
	}
	return files, nil
}

func writeModule(repo, output string, files []string, spec moduleSpec, rootVersion string) error {
	if err := module.Check(spec.path, spec.version); err != nil {
		return err
	}

	stage, err := os.MkdirTemp("", "pitaya-module-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	repoRoot, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer repoRoot.Close()

	sourceRoot := repoRoot
	if spec.sourceDir != "" {
		sourceRoot, err = repoRoot.OpenRoot(filepath.FromSlash(spec.sourceDir))
		if err != nil {
			return fmt.Errorf("open module root %q: %w", spec.sourceDir, err)
		}
		defer sourceRoot.Close()
	}

	for _, name := range files {
		rel, include := moduleFile(spec.sourceDir, name)
		if !include {
			continue
		}
		source := filepath.FromSlash(rel)
		info, err := sourceRoot.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect source %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		destination := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		data, err := sourceRoot.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read source %q: %w", name, err)
		}
		if err := os.WriteFile(destination, data, info.Mode().Perm()); err != nil {
			return err
		}
	}

	goModPath := filepath.Join(stage, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		return fmt.Errorf("read staged go.mod: %w", err)
	}
	if spec.requireRoot {
		goMod, err = rewriteChildGoMod(goModPath, goMod, rootVersion)
		if err != nil {
			return err
		}
		if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
			return err
		}
	}

	escapedPath, err := module.EscapePath(spec.path)
	if err != nil {
		return err
	}
	escapedVersion, err := module.EscapeVersion(spec.version)
	if err != nil {
		return err
	}
	versionDir := filepath.Join(output, filepath.FromSlash(escapedPath), "@v")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return err
	}

	info := struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{Version: spec.version, Time: time.Unix(0, 0).UTC()}
	infoData, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(versionDir, escapedVersion+".info"), append(infoData, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(versionDir, escapedVersion+".mod"), goMod, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(versionDir, "list"), []byte(spec.version+"\n"), 0o644); err != nil {
		return err
	}

	zipFile, err := os.Create(filepath.Join(versionDir, escapedVersion+".zip"))
	if err != nil {
		return err
	}
	zipErr := modzip.CreateFromDir(zipFile, module.Version{Path: spec.path, Version: spec.version}, stage)
	closeErr := zipFile.Close()
	if zipErr != nil {
		return zipErr
	}
	return closeErr
}

func moduleFile(sourceDir, name string) (string, bool) {
	name = filepath.ToSlash(name)
	if sourceDir == "" {
		for _, nested := range []string{"pitaya-cli/", "xk6-pitaya/", "tools/"} {
			if strings.HasPrefix(name, nested) {
				return "", false
			}
		}
		return name, true
	}
	prefix := filepath.ToSlash(sourceDir) + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

func rewriteChildGoMod(filename string, data []byte, rootVersion string) ([]byte, error) {
	parsed, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return nil, err
	}
	if len(parsed.Replace) != 0 {
		return nil, fmt.Errorf("%s contains replace directive for %s", filename, parsed.Replace[0].Old.Path)
	}
	if err := parsed.AddRequire(rootPath, rootVersion); err != nil {
		return nil, err
	}
	parsed.Cleanup()
	return parsed.Format()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
