package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	var repo string
	var destination string
	var filesFromStdin bool
	var requireAll bool
	flag.StringVar(&repo, "repo", "", "repository root")
	flag.StringVar(&destination, "destination", "", "staging destination")
	flag.BoolVar(&filesFromStdin, "files-from-stdin", false, "read a NUL-delimited file list from stdin")
	flag.BoolVar(&requireAll, "require-all", false, "fail if a listed path is missing or not a regular file")
	flag.Parse()

	if repo == "" || destination == "" {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if filesFromStdin {
		var files []string
		files, err = readNULFiles(os.Stdin)
		if err == nil {
			err = stageFiles(repo, destination, files, requireAll)
		}
	} else {
		err = stageRepository(repo, destination)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage files: %v\n", err)
		os.Exit(1)
	}
}

func stageRepository(repo, destination string) error {
	files, err := trackedFiles(repo)
	if err != nil {
		return err
	}
	return stageFiles(repo, destination, files, false)
}

func stageFiles(repo, destination string, files []string, requireAll bool) error {
	repoRoot, err := os.OpenRoot(repo)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer repoRoot.Close()

	for _, name := range files {
		source, info, err := openTrackedRegular(repoRoot, name)
		if err != nil {
			return fmt.Errorf("open tracked file %q: %w", name, err)
		}
		if source == nil {
			if requireAll {
				return fmt.Errorf("required path %q is missing or not a regular file", name)
			}
			continue
		}
		if err := copyFile(source, info, filepath.Join(destination, filepath.FromSlash(name))); err != nil {
			source.Close()
			return fmt.Errorf("copy tracked file %q: %w", name, err)
		}
		if err := source.Close(); err != nil {
			return fmt.Errorf("close tracked file %q: %w", name, err)
		}
	}
	return nil
}

func readNULFiles(input io.Reader) ([]string, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, fmt.Errorf("read file list: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("file list is not NUL-terminated")
	}

	entries := strings.Split(string(data[:len(data)-1]), "\x00")
	for _, entry := range entries {
		if entry == "" {
			return nil, fmt.Errorf("file list contains an empty path")
		}
	}
	return entries, nil
}

func trackedFiles(repo string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "ls-files", "-z", "--cached")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}
	entries := strings.Split(string(output), "\x00")
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry != "" {
			files = append(files, entry)
		}
	}
	return files, nil
}

func openTrackedRegular(repoRoot *os.Root, gitName string) (*os.File, os.FileInfo, error) {
	name := filepath.FromSlash(gitName)
	if !filepath.IsLocal(name) || name == "." {
		return nil, nil, fmt.Errorf("path is not local")
	}
	parts := strings.Split(name, string(filepath.Separator))
	current := repoRoot
	var openedRoots []*os.Root
	defer func() {
		for index := len(openedRoots) - 1; index >= 0; index-- {
			openedRoots[index].Close()
		}
	}()

	for _, part := range parts[:len(parts)-1] {
		before, err := current.Lstat(part)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if before.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symlinked ancestor %q", part)
		}
		if !before.IsDir() {
			return nil, nil, fmt.Errorf("ancestor %q is not a directory", part)
		}

		next, err := current.OpenRoot(part)
		if err != nil {
			return nil, nil, err
		}
		openedRoots = append(openedRoots, next)
		after, err := next.Stat(".")
		if err != nil {
			return nil, nil, err
		}
		if !os.SameFile(before, after) {
			return nil, nil, fmt.Errorf("ancestor %q changed while opening", part)
		}
		current = next
	}

	leaf := parts[len(parts)-1]
	before, err := current.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, nil
	}

	file, err := current.Open(leaf)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, nil, fmt.Errorf("file changed while opening")
	}
	return file, after, nil
}

func copyFile(source *os.File, info os.FileInfo, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, source)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(destination, info.ModTime(), info.ModTime())
}
