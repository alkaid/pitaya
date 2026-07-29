package main

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTrackedFilesExcludesUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "secret.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "go.mod"}} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}

	files, err := trackedFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, "go.mod") {
		t.Fatalf("tracked files = %v, want go.mod", files)
	}
	if slices.Contains(files, "secret.txt") {
		t.Fatalf("tracked files include untracked secret: %v", files)
	}
}

func TestWriteModuleSkipsSymlinksOutsideRepository(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	output := filepath.Join(base, "proxy")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repo, "go.mod"),
		[]byte("module "+rootPath+"\n\ngo 1.26.5\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("must not enter the module archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "leak.txt")); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	spec := moduleSpec{path: rootPath, version: defaultRootVer}
	if err := writeModule(repo, output, []string{"go.mod", "leak.txt"}, spec, defaultRootVer); err != nil {
		t.Fatalf("writeModule() error = %v", err)
	}

	archive := filepath.Join(
		output,
		"github.com", "alkaid", "pitaya", "v2", "@v",
		defaultRootVer+".zip",
	)
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	foundGoMod := false
	for _, file := range reader.File {
		if strings.HasSuffix(file.Name, "/leak.txt") {
			t.Fatalf("module archive contains outside-checkout symlink target: %s", file.Name)
		}
		if strings.HasSuffix(file.Name, "/go.mod") {
			foundGoMod = true
		}
	}
	if !foundGoMod {
		t.Fatal("module archive does not contain go.mod")
	}
}

func TestWriteModuleRejectsSymlinkedAncestorEscapes(t *testing.T) {
	tests := []struct {
		name          string
		spec          moduleSpec
		symlinkTarget func(base, repo string) string
	}{
		{
			name: "repository root",
			spec: moduleSpec{path: rootPath, version: defaultRootVer},
			symlinkTarget: func(base, _ string) string {
				return filepath.Join(base, "outside-repository")
			},
		},
		{
			name: "child module root",
			spec: moduleSpec{path: cliPath, version: defaultCLIVer, sourceDir: "pitaya-cli"},
			symlinkTarget: func(_, repo string) string {
				return filepath.Join(repo, "outside-module")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			output := filepath.Join(base, "proxy")
			moduleRoot := filepath.Join(repo, test.spec.sourceDir)
			if err := os.MkdirAll(moduleRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(moduleRoot, "go.mod"),
				[]byte("module "+test.spec.path+"\n\ngo 1.26.5\n"),
				0o644,
			); err != nil {
				t.Fatal(err)
			}

			outside := test.symlinkTarget(base, repo)
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "leak.txt"), []byte("outside module root"), 0o644); err != nil {
				t.Fatal(err)
			}
			symlinkTarget, err := filepath.Rel(moduleRoot, outside)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(symlinkTarget, filepath.Join(moduleRoot, "escape")); err != nil {
				t.Skipf("create symlink: %v", err)
			}

			files := []string{
				filepath.Join(test.spec.sourceDir, "go.mod"),
				filepath.Join(test.spec.sourceDir, "escape", "leak.txt"),
			}
			if err := writeModule(repo, output, files, test.spec, defaultRootVer); err == nil {
				t.Fatal("writeModule() accepted a file reached through a symlinked ancestor outside the module root")
			} else if !strings.Contains(err.Error(), "inspect source") {
				t.Fatalf("writeModule() error = %v, want source containment error", err)
			}
		})
	}
}

func TestRewriteChildGoModRejectsReplaceDirectives(t *testing.T) {
	data := []byte("module " + cliPath + "\n\ngo 1.26.5\n\nrequire " + rootPath + " v2.11.1-ak-35\n\nreplace " + rootPath + " => ../\n")
	if _, err := rewriteChildGoMod("go.mod", data, defaultRootVer); err == nil {
		t.Fatal("rewriteChildGoMod() accepted a replace directive")
	}
}
