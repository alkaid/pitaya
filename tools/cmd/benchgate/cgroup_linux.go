//go:build linux

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// CGROUP_NS_INIT_INO is part of Linux's uapi/linux/nsfs.h ABI.
const cgroupNamespaceInitialInode = 0xeffffffb

func validateProductionCgroupNamespace() error {
	fd, err := unix.Open("/proc/self/ns/cgroup", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open cgroup namespace: %w", err)
	}
	defer unix.Close(fd)
	namespaceType, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil {
		return fmt.Errorf("inspect cgroup namespace type: %w", err)
	}
	if namespaceType != unix.CLONE_NEWCGROUP {
		return fmt.Errorf("namespace type %#x is not cgroup", namespaceType)
	}
	var namespaceStat unix.Stat_t
	if err := unix.Fstat(fd, &namespaceStat); err != nil {
		return fmt.Errorf("stat cgroup namespace: %w", err)
	}
	var namespaceFS unix.Statfs_t
	if err := unix.Fstatfs(fd, &namespaceFS); err != nil {
		return fmt.Errorf("statfs cgroup namespace: %w", err)
	}
	if namespaceFS.Type != int64(unix.NSFS_MAGIC) {
		return fmt.Errorf("cgroup namespace filesystem is %#x, want nsfs %#x", namespaceFS.Type, unix.NSFS_MAGIC)
	}
	// Linux reserves CGROUP_NS_INIT_INO for the initial cgroup namespace;
	// unlike mountinfo paths, this nsfs identity is not cgroup-ns virtualized.
	if namespaceStat.Ino != cgroupNamespaceInitialInode {
		return fmt.Errorf("cgroup namespace inode is %d, want initial namespace %d", namespaceStat.Ino, cgroupNamespaceInitialInode)
	}
	return nil
}

func validateProductionCgroupPath(mount cgroupMount, filename string) error {
	var stat unix.Stat_t
	if err := unix.Stat(filename, &stat); err != nil {
		return err
	}
	major, minor := unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev))
	if major != mount.deviceMajor || minor != mount.deviceMinor {
		return fmt.Errorf("st_dev is %d:%d, want mountinfo %d:%d", major, minor, mount.deviceMajor, mount.deviceMinor)
	}

	var filesystem unix.Statfs_t
	if err := unix.Statfs(filename, &filesystem); err != nil {
		return err
	}
	wantMagic := int64(unix.CGROUP_SUPER_MAGIC)
	if mount.filesystem == "cgroup2" {
		wantMagic = int64(unix.CGROUP2_SUPER_MAGIC)
	}
	if filesystem.Type != wantMagic {
		return fmt.Errorf("statfs type is %#x, want %s %#x", filesystem.Type, mount.filesystem, wantMagic)
	}

	var statx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filename, 0, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &statx); err != nil {
		return err
	}
	if statx.Mask&unix.STATX_MNT_ID == 0 {
		return errors.New("statx did not return a mount ID")
	}
	if statx.Mnt_id != mount.mountID {
		return fmt.Errorf("statx mount ID is %d, want mountinfo %d", statx.Mnt_id, mount.mountID)
	}
	return nil
}
