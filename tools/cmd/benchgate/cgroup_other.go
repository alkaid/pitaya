//go:build !linux

package main

import "errors"

func validateProductionCgroupNamespace() error {
	return errors.New("cgroup provenance requires Linux")
}

func validateProductionCgroupPath(cgroupMount, string) error {
	return errors.New("cgroup mount identity validation requires Linux")
}
