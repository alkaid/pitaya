package main

import (
	"bytes"
	"crypto/sha1" // Git SHA-1 object IDs remain part of the repository format.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type gitTreeEntry struct {
	mode   string
	typeID string
	object string
	path   string
}

func runVerifyTree(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var repo, commit string
	flags.StringVar(&repo, "repo", "", "detached worktree to verify")
	flags.StringVar(&commit, "commit", "", "expected full commit SHA")
	if err := flags.Parse(args); err != nil {
		return asUsageError(err)
	}
	if flags.NArg() != 0 || repo == "" || commit == "" {
		flags.Usage()
		return errUsage
	}
	if err := validateSHA(commit); err != nil {
		return asUsageError(fmt.Errorf("commit SHA: %w", err))
	}
	if err := validateCheckout(repo, commit); err != nil {
		return err
	}
	if err := verifyCheckoutTree(repo, commit); err != nil {
		return fmt.Errorf("verify checkout tree: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "checkout tree verified: %s\n", commit); err != nil {
		return fmt.Errorf("write tree verification result: %w", err)
	}
	return nil
}

func verifyCheckoutTree(repo, commit string) error {
	if err := requireNoExtraWorktreeFiles(repo); err != nil {
		return err
	}
	format, err := gitSingleLine(repo, "rev-parse", "--show-object-format")
	if err != nil {
		return fmt.Errorf("identify Git object format: %w", err)
	}
	objectBytes := 0
	switch format {
	case "sha1":
		objectBytes = sha1.Size
	case "sha256":
		objectBytes = sha256.Size
	default:
		return fmt.Errorf("unsupported Git object format %q", format)
	}
	command := exec.Command("git", "-C", repo, "ls-tree", "-r", "-z", "--full-tree", commit)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("list pinned tree: %w", err)
	}
	entries, err := parseGitTree(output, objectBytes)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := verifyGitTreeEntry(repo, format, entry); err != nil {
			return fmt.Errorf("%s: %w", entry.path, err)
		}
	}
	return nil
}

func requireNoExtraWorktreeFiles(repo string) error {
	// A fresh formal worktree has no generated state. Include ignored paths so a
	// filter or checkout callback cannot add build inputs that ordinary clean
	// status intentionally hides.
	command := exec.Command("git", "-C", repo, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect exact worktree contents: %w", err)
	}
	if len(output) != 0 {
		first := output
		if separator := bytes.IndexByte(first, 0); separator >= 0 {
			first = first[:separator]
		}
		return fmt.Errorf("worktree contains modified, untracked, or ignored path %q", first)
	}
	return nil
}

func parseGitTree(output []byte, objectBytes int) ([]gitTreeEntry, error) {
	if len(output) == 0 || output[len(output)-1] != 0 {
		return nil, errors.New("Git tree output is empty or not NUL-terminated")
	}
	records := bytes.Split(output[:len(output)-1], []byte{0})
	entries := make([]gitTreeEntry, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		tab := bytes.IndexByte(record, '\t')
		if tab <= 0 || tab == len(record)-1 {
			return nil, fmt.Errorf("invalid Git tree record %q", record)
		}
		header := strings.Fields(string(record[:tab]))
		if len(header) != 3 {
			return nil, fmt.Errorf("invalid Git tree header %q", record[:tab])
		}
		entry := gitTreeEntry{mode: header[0], typeID: header[1], object: header[2], path: string(record[tab+1:])}
		if filepath.IsAbs(entry.path) || filepath.Clean(entry.path) != entry.path || entry.path == "." || entry.path == ".." || strings.HasPrefix(entry.path, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe Git tree path %q", entry.path)
		}
		object, err := hex.DecodeString(entry.object)
		if err != nil || len(object) != objectBytes {
			return nil, fmt.Errorf("invalid Git object ID %q", entry.object)
		}
		if _, ok := seen[entry.path]; ok {
			return nil, fmt.Errorf("duplicate Git tree path %q", entry.path)
		}
		seen[entry.path] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, nil
}

func verifyGitTreeEntry(repo, format string, entry gitTreeEntry) error {
	filename := filepath.Join(repo, entry.path)
	switch entry.mode {
	case "100644", "100755":
		if entry.typeID != "blob" {
			return fmt.Errorf("mode %s has type %s, want blob", entry.mode, entry.typeID)
		}
		info, err := os.Lstat(filename)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mode is %s, want regular file", info.Mode())
		}
		wantExecutable := entry.mode == "100755"
		if gotExecutable := info.Mode().Perm()&0o111 != 0; gotExecutable != wantExecutable {
			return fmt.Errorf("executable=%t, want %t", gotExecutable, wantExecutable)
		}
		actual, err := gitBlobOID(format, info.Size(), func(digest hash.Hash) error {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(digest, file)
			return err
		})
		if err != nil {
			return err
		}
		if actual != entry.object {
			return fmt.Errorf("raw blob is %s, want %s", actual, entry.object)
		}
		return nil
	case "120000":
		if entry.typeID != "blob" {
			return fmt.Errorf("symlink has type %s, want blob", entry.typeID)
		}
		target, err := os.Readlink(filename)
		if err != nil {
			return fmt.Errorf("read symlink: %w", err)
		}
		actual, err := gitBlobOID(format, int64(len(target)), func(digest hash.Hash) error {
			_, err := io.WriteString(digest, target)
			return err
		})
		if err != nil {
			return err
		}
		if actual != entry.object {
			return fmt.Errorf("raw symlink blob is %s, want %s", actual, entry.object)
		}
		return nil
	case "160000":
		if entry.typeID != "commit" {
			return fmt.Errorf("gitlink has type %s, want commit", entry.typeID)
		}
		info, err := os.Lstat(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("uninitialized gitlink is %s, want directory", info.Mode())
		}
		children, err := os.ReadDir(filename)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return errors.New("gitlink must remain uninitialized for a formal gate")
		}
		return nil
	default:
		return fmt.Errorf("unsupported Git tree mode %q", entry.mode)
	}
}

func gitBlobOID(format string, size int64, writeContents func(hash.Hash) error) (string, error) {
	var digest hash.Hash
	switch format {
	case "sha1":
		digest = sha1.New()
	case "sha256":
		digest = sha256.New()
	default:
		return "", fmt.Errorf("unsupported Git object format %q", format)
	}
	if _, err := fmt.Fprintf(digest, "blob %d%c", size, byte(0)); err != nil {
		return "", err
	}
	if err := writeContents(digest); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func gitSingleLine(repo string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	value := strings.TrimSuffix(string(output), "\n")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("git %s returned invalid single-line output", strings.Join(args, " "))
	}
	return value, nil
}
