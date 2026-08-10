package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyApparatusDigestTracksSelectedHarnessAdditions(t *testing.T) {
	tests := []struct {
		name     string
		relative string
		contents string
	}{
		{name: "benchmark test", relative: "benchmark/injected_test.go", contents: "//go:build benchmark\n\npackage benchmark\n"},
		{name: "benchmark harness", relative: "benchmark/harness/extra_linux.go", contents: "//go:build linux\n\npackage main\n"},
		{name: "testing server", relative: "examples/testing/extra_linux.go", contents: "//go:build linux\n\npackage main\n"},
		{name: "testing proto", relative: "examples/testing/protos/extra_linux.go", contents: "//go:build linux\n\npackage protos\n"},
		{name: "helpers", relative: "helpers/extra_linux.go", contents: "//go:build linux\n\npackage helpers\n"},
		{name: "test proto", relative: "protos/test/extra_linux.go", contents: "//go:build linux\n\npackage test\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, policy := createBuildSelectionRepository(t)
			before := mustPolicyApparatusDigest(t, repo, policy)
			mustWriteFile(t, filepath.Join(repo, filepath.FromSlash(test.relative)), []byte(test.contents))
			gitCommand(t, repo, "add", test.relative)
			gitCommand(t, repo, "commit", "-q", "-m", "add selected harness file")
			after := mustPolicyApparatusDigest(t, repo, policy)
			if before == after {
				t.Fatalf("apparatus digest did not change after adding selected %s", test.relative)
			}
		})
	}
}

func TestPolicyApparatusDigestTracksSelectedHarnessDeletionAndReplacement(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		repo, policy := createBuildSelectionRepository(t)
		relative := "helpers/removable_linux.go"
		mustWriteFile(t, filepath.Join(repo, relative), []byte("//go:build linux\n\npackage helpers\n"))
		gitCommand(t, repo, "add", relative)
		gitCommand(t, repo, "commit", "-q", "-m", "add removable harness file")
		before := mustPolicyApparatusDigest(t, repo, policy)
		gitCommand(t, repo, "rm", "-q", relative)
		gitCommand(t, repo, "commit", "-q", "-m", "remove harness file")
		after := mustPolicyApparatusDigest(t, repo, policy)
		if before == after {
			t.Fatal("apparatus digest did not change after deleting a selected harness file")
		}
	})

	t.Run("build-tag replacement", func(t *testing.T) {
		repo, policy := createBuildSelectionRepository(t)
		oldRelative := "helpers/old_linux.go"
		mustWriteFile(t, filepath.Join(repo, oldRelative), []byte("//go:build linux\n\npackage helpers\n"))
		gitCommand(t, repo, "add", oldRelative)
		gitCommand(t, repo, "commit", "-q", "-m", "add old harness file")
		before := mustPolicyApparatusDigest(t, repo, policy)

		mustWriteFile(t, filepath.Join(repo, oldRelative), []byte("//go:build windows\n\npackage helpers\n"))
		newRelative := "helpers/new_linux.go"
		mustWriteFile(t, filepath.Join(repo, newRelative), []byte("//go:build linux\n\npackage helpers\n"))
		gitCommand(t, repo, "add", oldRelative, newRelative)
		gitCommand(t, repo, "commit", "-q", "-m", "replace selected harness file")
		after := mustPolicyApparatusDigest(t, repo, policy)
		if before == after {
			t.Fatal("apparatus digest did not change after replacing a selected harness file")
		}
	})
}

func TestPolicyApparatusDigestIgnoresUnselectedPlatformFiles(t *testing.T) {
	repo, policy := createBuildSelectionRepository(t)
	before := mustPolicyApparatusDigest(t, repo, policy)
	relative := "helpers/extra_windows.go"
	mustWriteFile(t, filepath.Join(repo, relative), []byte("//go:build windows\n\npackage helpers\n"))
	gitCommand(t, repo, "add", relative)
	gitCommand(t, repo, "commit", "-q", "-m", "add unselected platform file")
	after := mustPolicyApparatusDigest(t, repo, policy)
	if before != after {
		t.Fatalf("unselected Windows file changed Linux apparatus digest: %s -> %s", before, after)
	}
}

func TestPolicyApparatusDigestRejectsSelectedUntrackedFile(t *testing.T) {
	repo, policy := createBuildSelectionRepository(t)
	mustWriteFile(t, filepath.Join(repo, ".gitignore"), []byte("helpers/generated_linux.go\n"))
	gitCommand(t, repo, "add", ".gitignore")
	gitCommand(t, repo, "commit", "-q", "-m", "ignore generated file")
	mustWriteFile(t, filepath.Join(repo, "helpers/generated_linux.go"), []byte("//go:build linux\n\npackage helpers\n"))
	if _, err := apparatusDigestForPolicy(repo, policy); err == nil || !strings.Contains(err.Error(), "not a tracked file") {
		t.Fatalf("selected ignored file error = %v, want tracked-file rejection", err)
	}
}

func TestSelectedApparatusPathRejectsAbsoluteGoListSource(t *testing.T) {
	if _, err := selectedApparatusPath("benchmark", "/tmp/generated.go"); err == nil || !strings.Contains(err.Error(), "is absolute") {
		t.Fatalf("absolute selected source error = %v", err)
	}
	if relative, err := selectedApparatusPath("benchmark", "subdir/source.go"); err != nil || relative != "benchmark/subdir/source.go" {
		t.Fatalf("relative selected source = %q, %v", relative, err)
	}
}

func createBuildSelectionRepository(t *testing.T) (string, benchmarkPolicy) {
	t.Helper()
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	files := map[string]string{
		"go.mod":                                "module example.com/harness\n\ngo 1.23\n",
		"benchmark/benchmark_test.go":           "//go:build benchmark\n\npackage benchmark\n\nimport (\n\t_ \"example.com/harness/helpers\"\n\t_ \"example.com/harness/protos/test\"\n\t\"testing\"\n)\n\nfunc BenchmarkAlpha(b *testing.B) {}\n",
		"benchmark/harness/main.go":             "package main\n\nimport _ \"example.com/harness/helpers\"\n\nfunc main() {}\n",
		"examples/testing/main.go":              "package main\n\nimport (\n\t_ \"example.com/harness/examples/testing/protos\"\n\t_ \"example.com/harness/helpers\"\n)\n\nfunc main() {}\n",
		"examples/testing/protos/cluster.pb.go": "package protos\n",
		"helpers/helpers.go":                    "package helpers\n",
		"protos/test/test.pb.go":                "package test\n",
	}
	paths := make([]string, 0, len(files))
	for relative, contents := range files {
		filename := filepath.Join(repo, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filename, []byte(contents))
		paths = append(paths, relative)
	}
	gitCommand(t, repo, append([]string{"add"}, paths...)...)
	gitCommand(t, repo, "commit", "-q", "-m", "initial harness")
	policy := validTestPolicy()
	policy.Package = "example.com/harness/benchmark"
	policy.BenchmarkNames = []string{"Alpha"}
	policy.ApparatusPaths = []string{
		"benchmark/benchmark_test.go",
		"benchmark/harness/main.go",
		"examples/testing/main.go",
		"examples/testing/protos/cluster.pb.go",
		"go.mod",
		"helpers/helpers.go",
		"protos/test/test.pb.go",
	}
	return repo, policy
}

func mustPolicyApparatusDigest(t *testing.T, repo string, policy benchmarkPolicy) string {
	t.Helper()
	digest, err := apparatusDigestForPolicy(repo, policy)
	if err != nil {
		t.Fatalf("apparatusDigestForPolicy error = %v", err)
	}
	return digest
}
