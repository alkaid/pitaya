package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageRepositoryCopiesOnlyTrackedRegularFiles(t *testing.T) {
	repo := t.TempDir()
	destination := t.TempDir()
	tracked := filepath.Join(repo, "nested", "tracked.sh")
	if err := os.MkdirAll(filepath.Dir(tracked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("tracked\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "tracked-link")); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "add", "nested/tracked.sh", "tracked-link")

	if err := stageRepository(repo, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "tracked.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "tracked\n" {
		t.Fatalf("staged content = %q", data)
	}
	info, err := os.Stat(filepath.Join(destination, "nested", "tracked.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %o, want 755", info.Mode().Perm())
	}
	for _, name := range []string{"untracked.txt", "tracked-link"} {
		if _, err := os.Lstat(filepath.Join(destination, name)); !os.IsNotExist(err) {
			t.Fatalf("excluded path %q exists or returned unexpected error: %v", name, err)
		}
	}
}

func TestStageRepositoryRejectsSymlinkedAncestor(t *testing.T) {
	tests := []struct {
		name       string
		linkTarget func(base, repo string) string
	}{
		{
			name: "outside repository",
			linkTarget: func(base, _ string) string {
				return filepath.Join(base, "outside")
			},
		},
		{
			name: "inside repository",
			linkTarget: func(_, repo string) string {
				return filepath.Join(repo, "real")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			destination := filepath.Join(base, "context")
			trackedDir := filepath.Join(repo, "tracked")
			if err := os.MkdirAll(trackedDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(trackedDir, "file.txt"), []byte("tracked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init", "-q")
			git(t, repo, "add", "tracked/file.txt")

			target := test.linkTarget(base, repo)
			if err := os.Rename(trackedDir, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, trackedDir); err != nil {
				t.Skipf("create symlink: %v", err)
			}

			err := stageRepository(repo, destination)
			if err == nil || !strings.Contains(err.Error(), "symlinked ancestor") {
				t.Fatalf("stageRepository() error = %v, want symlinked ancestor rejection", err)
			}
			if _, err := os.Stat(filepath.Join(destination, "tracked", "file.txt")); !os.IsNotExist(err) {
				t.Fatalf("escaped file was staged or returned unexpected error: %v", err)
			}
		})
	}
}

func TestStageFilesCopiesExplicitUntrackedFile(t *testing.T) {
	repo := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := stageFiles(repo, destination, []string{"untracked.go"}, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "untracked.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package example\n" {
		t.Fatalf("staged content = %q", data)
	}
}

func TestStageFilesRequireAll(t *testing.T) {
	err := stageFiles(t.TempDir(), t.TempDir(), []string{"missing.go"}, true)
	if err == nil || !strings.Contains(err.Error(), "required path") {
		t.Fatalf("stageFiles() error = %v, want required path error", err)
	}
}

func TestReadNULFiles(t *testing.T) {
	files, err := readNULFiles(bytes.NewBufferString("one.go\x00dir/two.go\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(files, ","), "one.go,dir/two.go"; got != want {
		t.Fatalf("readNULFiles() = %q, want %q", got, want)
	}

	for _, input := range []string{"missing-terminator", "one.go\x00\x00"} {
		if _, err := readNULFiles(bytes.NewBufferString(input)); err == nil {
			t.Fatalf("readNULFiles(%q) unexpectedly succeeded", input)
		}
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
