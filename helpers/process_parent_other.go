//go:build !unix

package helpers

import "os/exec"

func prepareProcess(_ *exec.Cmd) {}
