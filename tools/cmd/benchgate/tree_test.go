package main

import (
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCheckoutTreeAcceptsExactRawBytes(t *testing.T) {
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	mustWriteFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored.tmp\n"))
	mustWriteFile(t, filepath.Join(repo, "plain.txt"), []byte("plain\n"))
	mustWriteFile(t, filepath.Join(repo, "executable.sh"), []byte("#!/bin/sh\n"))
	if err := os.Symlink("plain.txt", filepath.Join(repo, "plain.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "executable.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repo, "add", ".")
	gitCommand(t, repo, "commit", "-q", "-m", "tree")
	commit := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := verifyCheckoutTree(repo, commit); err != nil {
		t.Fatalf("verify exact tree: %v", err)
	}
	mustWriteFile(t, filepath.Join(repo, "ignored.tmp"), []byte("hidden input\n"))
	if err := verifyCheckoutTree(repo, commit); err == nil || !strings.Contains(err.Error(), "ignored path") {
		t.Fatalf("verify ignored extra error = %v, want ignored path", err)
	}
	if err := os.Remove(filepath.Join(repo, "ignored.tmp")); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repo, "config", "core.filemode", "false")
	if err := os.Chmod(filepath.Join(repo, "plain.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyCheckoutTree(repo, commit); err == nil || !strings.Contains(err.Error(), "executable=true, want false") {
		t.Fatalf("verify mode mismatch error = %v", err)
	}
}

func TestVerifyCheckoutTreeSupportsSHA256Objects(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "-C", repo, "init", "-q", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("Git SHA-256 repositories are unavailable: %v: %s", err, output)
	}
	configureGitIdentity(t, repo)
	mustWriteFile(t, filepath.Join(repo, "payload.txt"), []byte("sha256 payload\n"))
	gitCommand(t, repo, "add", ".")
	gitCommand(t, repo, "commit", "-q", "-m", "sha256 tree")
	commit := gitOutput(t, repo, "rev-parse", "HEAD")
	if len(commit) != sha256.Size*2 {
		t.Fatalf("SHA-256 commit length = %d, want %d", len(commit), sha256.Size*2)
	}
	if err := verifyCheckoutTree(repo, commit); err != nil {
		t.Fatalf("verify SHA-256 tree: %v", err)
	}
}

func TestVerifyCheckoutTreeRejectsCleanSmudgeBytesHiddenFromStatus(t *testing.T) {
	repo := t.TempDir()
	gitCommand(t, repo, "init", "-q")
	configureGitIdentity(t, repo)
	mustWriteFile(t, filepath.Join(repo, ".gitattributes"), []byte("payload.txt filter=synthetic\n"))
	mustWriteFile(t, filepath.Join(repo, "payload.txt"), []byte("canonical\n"))
	gitCommand(t, repo, "add", ".")
	gitCommand(t, repo, "commit", "-q", "-m", "filtered tree")
	gitCommand(t, repo, "config", "filter.synthetic.smudge", "sed s/canonical/altered/")
	gitCommand(t, repo, "config", "filter.synthetic.clean", "sed s/altered/canonical/")
	commit := gitOutput(t, repo, "rev-parse", "HEAD")
	worktree := filepath.Join(t.TempDir(), "filtered")
	gitCommand(t, repo, "worktree", "add", "-q", "--detach", worktree, commit)
	t.Cleanup(func() { gitCommand(t, repo, "worktree", "remove", "--force", worktree) })
	if got := strings.TrimSpace(string(mustReadProductionFile(t, filepath.Join(worktree, "payload.txt")))); got != "altered" {
		t.Fatalf("smudged payload = %q, want altered", got)
	}
	if status := gitOutput(t, worktree, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("filtered worktree is not Git-clean: %q", status)
	}
	if err := verifyCheckoutTree(worktree, commit); err == nil || !strings.Contains(err.Error(), "raw blob is") {
		t.Fatalf("verify filtered tree error = %v, want raw blob mismatch", err)
	}
}

func TestRunVerifyTreeRejectsUsageAndReportsSuccess(t *testing.T) {
	repo, _, commit := createTwoCommitRepository(t)
	var stdout, stderr strings.Builder
	if err := runVerifyTree([]string{"-repo", repo, "-commit", commit}, &stdout, &stderr); err != nil {
		t.Fatalf("run verify-tree: %v", err)
	}
	if !strings.Contains(stdout.String(), "checkout tree verified: "+commit) {
		t.Fatalf("verify-tree output = %q", stdout.String())
	}
	if err := runVerifyTree([]string{"-repo", repo, "-commit", "bad"}, &stdout, &stderr); !errors.Is(err, errUsage) {
		t.Fatalf("invalid verify-tree error = %v, want usage", err)
	}
}
